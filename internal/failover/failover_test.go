package failover

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wu529778790/edge-balancer/internal/cf"
	"github.com/wu529778790/edge-balancer/internal/config"
	"github.com/wu529778790/edge-balancer/internal/dns"
)

// fakeSwitcher 模拟 Workers Route 操作（不真调 CF API）；可注入错误模拟切换失败
type fakeSwitcher struct {
	route   *dns.WorkerRoute // 当前存在的 route（pattern 匹配）
	putCalls int32
	delCalls int32
	putErr   error
	delErr   error
}

func (f *fakeSwitcher) ListRoutes(zoneID string) ([]dns.WorkerRoute, error) {
	if f.route != nil {
		return []dns.WorkerRoute{*f.route}, nil
	}
	return nil, nil
}

func (f *fakeSwitcher) PutRoute(zoneID, pattern, script string) error {
	atomic.AddInt32(&f.putCalls, 1)
	if f.putErr != nil {
		return f.putErr
	}
	f.route = &dns.WorkerRoute{ID: "r1", Pattern: pattern, Script: script}
	return nil
}

func (f *fakeSwitcher) DeleteRoute(zoneID, pattern string) error {
	atomic.AddInt32(&f.delCalls, 1)
	if f.delErr != nil {
		return f.delErr
	}
	f.route = nil
	return nil
}

// fakeQuota 可控配额查询（按账号名）
type fakeQuota struct {
	usages map[string]cf.Usage
	errs   map[string]error
}

func (f *fakeQuota) query(acc config.CFAccount) (cf.Usage, error) {
	if e, ok := f.errs[acc.Name]; ok {
		return cf.Usage{Name: acc.Name}, e
	}
	u := f.usages[acc.Name]
	u.Name = acc.Name
	return u, nil
}

// mkSite 构造 3 目标队列站点：worker-a（accA）→ worker-b（accB）→ server（无限额度兜底）
func mkSite(primaryURL, backupURL string, probe config.ProbeConfig, sw Switcher, q QuotaQuery) *Site {
	targets := []config.TargetConfig{
		{Name: "worker-a", RecordType: "CNAME", DNSContent: "a.workers.dev", URL: primaryURL, Health: "/api/health", QuotaAccount: "accA", Script: "worker-a-script"},
		{Name: "worker-b", RecordType: "CNAME", DNSContent: "b.workers.dev", URL: backupURL, Health: "/api/health", QuotaAccount: "accB", Script: "worker-b-script"},
		{Name: "server", RecordType: "A", DNSContent: "1.2.3.4", URL: backupURL, Health: "/api/health"},
	}
	accounts := []config.CFAccount{
		{Name: "accA", Quota: 100000, Threshold: 90},
		{Name: "accB", Quota: 100000, Threshold: 90},
	}
	s, err := NewSite("a.test", "a.test/*", targets, accounts, probe, sw, "zone1", false, q)
	if err != nil {
		panic(err)
	}
	return s
}

func defaultProbe() config.ProbeConfig {
	return config.ProbeConfig{Mode: "server", Interval: 10, Timeout: 3, FailThreshold: 3, Cooldown: 120, QuotaInterval: 1}
}

func upSrv() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
}
func downSrv(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
}

// 探测：健康目标判定 OK
func TestProbeHealthy(t *testing.T) {
	srv := upSrv()
	defer srv.Close()
	r := probeTarget(&config.TargetConfig{Name: "x", URL: srv.URL}, "/api/health", 3*time.Second)
	if !r.OK {
		t.Fatalf("健康目标应判定 OK，实际 %+v", r)
	}
}

// 探测：5xx 判失败
func TestProbe500Fails(t *testing.T) {
	srv := downSrv(502)
	defer srv.Close()
	r := probeTarget(&config.TargetConfig{Name: "x", URL: srv.URL}, "/api/health", 3*time.Second)
	if r.OK {
		t.Fatalf("5xx 应判失败")
	}
}

// 探测：404 判失败（健康端点 404 = 服务没在跑）
func TestProbe404Fails(t *testing.T) {
	srv := downSrv(404)
	defer srv.Close()
	r := probeTarget(&config.TargetConfig{Name: "x", URL: srv.URL}, "/api/health", 3*time.Second)
	if r.OK {
		t.Fatalf("404 应判失败（健康端点不存在）")
	}
}

// 探测：超时不判挂（服务器侧线路差不代表目标挂）
func TestProbeTimeoutNotFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	r := probeTarget(&config.TargetConfig{Name: "x", URL: srv.URL}, "/api/health", 300*time.Millisecond)
	if !r.OK {
		t.Fatalf("超时应视为慢不判挂，实际 %+v", r)
	}
	if !strings.Contains(r.Detail, "timeout") {
		t.Fatalf("detail 应含 timeout 标记，实际 %q", r.Detail)
	}
}

// 配额推进：A 配额超限 → route 切到 worker-b
func TestQuotaTrip(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 1 {
		t.Fatalf("A 配额超限应切到 worker-b（index 1），实际 index=%d", s.currentIndex)
	}
	if sw.route == nil || sw.route.Script != "worker-b-script" {
		t.Fatalf("route 应指向 worker-b-script，实际 %+v", sw.route)
	}
	if sw.putCalls == 0 {
		t.Fatalf("应调用 PutRoute")
	}
}

// 配额链式：A、B 都超限 → 删除 route 切到服务器兜底
func TestQuotaChainToFallback(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{
		"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true},
		"accB": {Used: 98000, Quota: 100000, Percent: 98, OverLimit: true},
	}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 2 {
		t.Fatalf("A/B 均超限应切到 server（index 2），实际 index=%d", s.currentIndex)
	}
	if sw.route != nil {
		t.Fatalf("切服务器应删除 route，实际 %+v", sw.route)
	}
	if sw.delCalls == 0 {
		t.Fatalf("应调用 DeleteRoute")
	}
}

// 配额正常时不切换
func TestQuotaOKNoTrip(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 10000, Quota: 100000, Percent: 10, OverLimit: false}}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 0 {
		t.Fatalf("配额正常不应切换，实际 index=%d", s.currentIndex)
	}
	if sw.putCalls+sw.delCalls != 0 {
		t.Fatalf("不应有 route 操作")
	}
}

// 健康推进：A 连续失败 3 次 → 切到 B
func TestHealthTrip(t *testing.T) {
	down := downSrv(503)
	up := upSrv()
	defer down.Close()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(down.URL, up.URL, defaultProbe(), sw, q.query)

	for i := 0; i < 3; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.currentIndex != 1 {
		t.Fatalf("A 连续失败 3 次应切到 B，实际 index=%d", s.currentIndex)
	}
	if sw.route == nil || sw.route.Script != "worker-b-script" {
		t.Fatalf("route 应指向 worker-b-script，实际 %+v", sw.route)
	}
}

// 失败不足 3 次不切换
func TestNoTripBelowThreshold(t *testing.T) {
	down := downSrv(503)
	up := upSrv()
	defer down.Close()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(down.URL, up.URL, defaultProbe(), sw, q.query)

	for i := 0; i < 2; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.currentIndex != 0 {
		t.Fatalf("2 次失败不应切换，实际 index=%d", s.currentIndex)
	}
}

// 每日回切：跨天后 A 配额恢复 → 从 server 切回 A（PutRoute）
func TestDailyResetBackToFirst(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{}}
	sw := &fakeSwitcher{} // 无 route = 服务器兜底
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)
	s.mu.Lock()
	s.currentIndex = 2
	s.lastCheckDay = "2000-01-01"
	s.mu.Unlock()

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 0 {
		t.Fatalf("跨天应切回最早可用目标 A，实际 index=%d", s.currentIndex)
	}
	if sw.route == nil || sw.route.Script != "worker-a-script" {
		t.Fatalf("应 PutRoute 到 worker-a-script，实际 %+v", sw.route)
	}
}

// 每日回切跳过配额仍超限的目标：A 超限、B 可用 → 切回 B 而非 A
func TestDailyResetSkipsOverLimit(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)
	s.mu.Lock()
	s.currentIndex = 2
	s.lastCheckDay = "2000-01-01"
	s.mu.Unlock()

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 1 {
		t.Fatalf("A 仍超限应切回 B（index 1），实际 index=%d", s.currentIndex)
	}
	if sw.route == nil || sw.route.Script != "worker-b-script" {
		t.Fatalf("应 PutRoute 到 worker-b-script，实际 %+v", sw.route)
	}
}

// 冷却：切换后冷却期内不执行新的切换
func TestCooldownBlocksSwitch(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)
	s.mu.Lock()
	s.cooldownUntil = time.Now().Add(5 * time.Second)
	s.mu.Unlock()

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 0 {
		t.Fatalf("冷却期内不应切换，实际 index=%d", s.currentIndex)
	}
	if sw.putCalls+sw.delCalls != 0 {
		t.Fatalf("冷却期内不应有 route 操作")
	}
}

// 手动切换：进入手动模式，tick 不自动干预
func TestManualOverride(t *testing.T) {
	down := downSrv(503)
	up := upSrv()
	defer down.Close()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(down.URL, up.URL, defaultProbe(), sw, q.query)

	if err := s.ManualSwitch(1); err != nil {
		t.Fatal(err)
	}
	if s.state != StateManual || s.manualIndex != 1 {
		t.Fatalf("手动切换后应 manual+1，实际 %s/%d", s.state, s.manualIndex)
	}
	if sw.route == nil || sw.route.Script != "worker-b-script" {
		t.Fatalf("手动切换应 PutRoute 到 B，实际 %+v", sw.route)
	}
	for i := 0; i < 5; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.state != StateManual || s.currentIndex != 1 {
		t.Fatalf("手动模式下不应自动切换，实际 %s/%d", s.state, s.currentIndex)
	}
}

// 手动下标越界拒绝
func TestManualSwitchOutOfRange(t *testing.T) {
	up := upSrv()
	defer up.Close()
	sw := &fakeSwitcher{}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, (&fakeQuota{}).query)
	if err := s.ManualSwitch(9); err == nil {
		t.Fatalf("越界下标应报错")
	}
}

// 恢复自动：按当前 route 实际指向对齐
func TestManualAuto(t *testing.T) {
	up := upSrv()
	defer up.Close()
	// 当前无 route = 服务器兜底
	sw := &fakeSwitcher{}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, (&fakeQuota{}).query)
	if err := s.ManualSwitch(2); err != nil {
		t.Fatal(err)
	}

	if err := s.ManualAuto(); err != nil {
		t.Fatal(err)
	}
	if s.state != StateAuto || s.currentIndex != 2 || s.manualIndex != -1 {
		t.Fatalf("恢复自动应对齐到 server（index 2），实际 %s/%d/%d", s.state, s.currentIndex, s.manualIndex)
	}
}

// dry-run：自动切换只决策不执行 route 操作
func TestDryRunDoesNotExecute(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)
	s.dryRun = true

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 1 {
		t.Fatalf("dry-run 也应完成状态迁移，实际 index=%d", s.currentIndex)
	}
	if sw.putCalls+sw.delCalls != 0 {
		t.Fatalf("dry-run 自动切换不应执行 route 操作，实际 put=%d del=%d", sw.putCalls, sw.delCalls)
	}
}

// dry-run：手动切换放行（人有意识的操作应真实生效）
func TestDryRunManualStillExecutes(t *testing.T) {
	up := upSrv()
	defer up.Close()
	sw := &fakeSwitcher{}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, (&fakeQuota{}).query)
	s.dryRun = true

	if err := s.ManualSwitch(2); err != nil {
		t.Fatal(err)
	}
	if sw.delCalls != 1 {
		t.Fatalf("dry-run 手动切换应真实执行 DeleteRoute，实际 del=%d", sw.delCalls)
	}
	if s.state != StateManual {
		t.Fatalf("手动切换应进入 manual，实际 %s", s.state)
	}
}

// route 操作失败：回滚 currentIndex，状态机与真实 route 保持一致
func TestRouteFailRollback(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{putErr: fmt.Errorf("cf api down"), route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)

	s.mu.Lock()
	err := s.doSwitch(1, "quota", "测试")
	s.mu.Unlock()
	if err == nil {
		t.Fatalf("route 操作失败应返回错误")
	}
	if s.currentIndex != 0 {
		t.Fatalf("route 操作失败应回滚到原目标，实际 index=%d", s.currentIndex)
	}
	snap := s.Snapshot()
	if len(snap.Events) == 0 || !strings.Contains(snap.Events[0].Detail, "切换失败") {
		t.Fatalf("应记录失败事件，实际 %+v", snap.Events)
	}
}

// 兜底失守：所有目标都挂 → 无切换，保持现状
func TestAllDownNoSwitch(t *testing.T) {
	down := downSrv(503)
	defer down.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(down.URL, down.URL, defaultProbe(), sw, q.query)

	for i := 0; i < 5; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.currentIndex != 0 {
		t.Fatalf("A 超限但后继全挂应保持现状，实际 index=%d", s.currentIndex)
	}
	if sw.putCalls+sw.delCalls != 0 {
		t.Fatalf("不应执行 route 操作")
	}
}

// SyncActual：有 route 指向 worker-b → 对齐到 worker-b
func TestSyncActualRouteToWorker(t *testing.T) {
	up := upSrv()
	defer up.Close()
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-b-script"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, (&fakeQuota{}).query)
	if err := s.SyncActual(); err != nil {
		t.Fatal(err)
	}
	if s.currentIndex != 1 {
		t.Fatalf("route 指向 worker-b 应对齐到 index 1，实际 %d", s.currentIndex)
	}
}

// Snapshot 结构完整性（含目标队列与事件）
func TestSnapshot(t *testing.T) {
	up := upSrv()
	defer up.Close()
	sw := &fakeSwitcher{route: &dns.WorkerRoute{ID: "r1", Pattern: "a.test/*", Script: "worker-a-script"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, (&fakeQuota{}).query)
	s.mu.Lock()
	s.events = append(s.events, SwitchEvent{Time: "12:00:00", From: "worker-a", To: "server", Reason: "quota", Detail: "测试"})
	s.mu.Unlock()

	snap := s.Snapshot()
	if snap.Domain != "a.test" || len(snap.Targets) != 3 || len(snap.Events) != 1 {
		t.Fatalf("Snapshot 异常: %+v", snap)
	}
	if snap.Current != "worker-a" || snap.CurrentIndex != 0 {
		t.Fatalf("Snapshot 当前指向异常: %+v", snap)
	}
	if snap.RoutePattern != "a.test/*" {
		t.Fatalf("Snapshot route_pattern 异常: %+v", snap.RoutePattern)
	}
	if snap.Targets[2].Name != "server" || snap.Targets[2].Script != "" {
		t.Fatalf("兜底目标应无 script: %+v", snap.Targets[2])
	}
}

// BuildSites：无 targets 站点不构建
func TestBuildSitesEmpty(t *testing.T) {
	cfg := &config.Config{Sites: []config.SiteConfig{{Domain: "a.test", Upstreams: []config.UpstreamConfig{{Name: "u", URL: "http://x"}}}}}
	cfg.DNS.Zone = "shenzjd.com"
	sites, err := BuildSites(cfg, nil, nil)
	if err != nil {
		t.Fatalf("无 targets 站点不应报错: %v", err)
	}
	if len(sites) != 0 {
		t.Fatalf("应返回空，实际 %d 个", len(sites))
	}
}
