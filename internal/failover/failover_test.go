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

// fakeSwitcher 记录 PatchRecord 调用（不真调 CF API）；可注入错误模拟 PATCH 失败
type fakeSwitcher struct {
	patchCalls  int32
	patchErr    error
	lastType    string
	lastContent string
	record      dns.Record
}

func (f *fakeSwitcher) PatchRecord(zoneID, recordID, rtype, content string, ttl int, keepProxy bool) (*dns.Record, error) {
	atomic.AddInt32(&f.patchCalls, 1)
	if f.patchErr != nil {
		return nil, f.patchErr
	}
	f.lastType = rtype
	f.lastContent = content
	f.record = dns.Record{ID: recordID, Type: rtype, Name: recordID, Content: content, TTL: ttl}
	return &f.record, nil
}

func (f *fakeSwitcher) GetRecord(zoneID, recordID string) (*dns.Record, error) {
	return &f.record, nil
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

// mkSite 构造 3 目标队列站点：worker-a（accA）→ worker-b（accB）→ server（无限额度）
func mkSite(primaryURL, backupURL string, probe config.ProbeConfig, sw Switcher, q QuotaQuery) *Site {
	targets := []config.TargetConfig{
		{Name: "worker-a", RecordType: "CNAME", DNSContent: "a.workers.dev", URL: primaryURL, Health: "/api/health", QuotaAccount: "accA"},
		{Name: "worker-b", RecordType: "CNAME", DNSContent: "b.workers.dev", URL: backupURL, Health: "/api/health", QuotaAccount: "accB"},
		{Name: "server", RecordType: "A", DNSContent: "1.2.3.4", URL: backupURL, Health: "/api/health"},
	}
	accounts := []config.CFAccount{
		{Name: "accA", Quota: 100000, Threshold: 90},
		{Name: "accB", Quota: 100000, Threshold: 90},
	}
	s, err := NewSite("a.test", targets, accounts, probe, sw, "zone1", "rec1", 60, false, q)
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

// 配额推进：A 配额超限 → 切到 B
func TestQuotaTrip(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 1 {
		t.Fatalf("A 配额超限应切到 B（index 1），实际 index=%d", s.currentIndex)
	}
	if sw.lastContent != "b.workers.dev" || sw.lastType != "CNAME" {
		t.Fatalf("应 PATCH CNAME→b.workers.dev，实际 %s→%s", sw.lastType, sw.lastContent)
	}
}

// 配额链式：A、B 都超限 → 直接切到兜底 server
func TestQuotaChainToFallback(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{
		"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true},
		"accB": {Used: 98000, Quota: 100000, Percent: 98, OverLimit: true},
	}}
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 2 {
		t.Fatalf("A/B 均超限应切到 server（index 2），实际 index=%d", s.currentIndex)
	}
	if sw.lastContent != "1.2.3.4" || sw.lastType != "A" {
		t.Fatalf("应 PATCH A→1.2.3.4，实际 %s→%s", sw.lastType, sw.lastContent)
	}
}

// 配额正常时不切换
func TestQuotaOKNoTrip(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 10000, Quota: 100000, Percent: 10, OverLimit: false}}}
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 0 {
		t.Fatalf("配额正常不应切换，实际 index=%d", s.currentIndex)
	}
	if sw.patchCalls != 0 {
		t.Fatalf("不应有 PATCH 调用")
	}
}

// 健康推进：A 连续失败 3 次 → 切到 B
func TestHealthTrip(t *testing.T) {
	down := downSrv(503)
	up := upSrv()
	defer down.Close()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{}}
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(down.URL, up.URL, defaultProbe(), sw, q.query)

	for i := 0; i < 3; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.currentIndex != 1 {
		t.Fatalf("A 连续失败 3 次应切到 B，实际 index=%d", s.currentIndex)
	}
	if sw.lastContent != "b.workers.dev" {
		t.Fatalf("应 PATCH 到 B，实际 %s", sw.lastContent)
	}
}

// 失败不足 3 次不切换
func TestNoTripBelowThreshold(t *testing.T) {
	down := downSrv(503)
	up := upSrv()
	defer down.Close()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{}}
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
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

// 每日回切：跨天后 A 配额恢复 → 从 server 切回 A
func TestDailyResetBackToFirst(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{}}
	sw := &fakeSwitcher{record: dns.Record{Type: "A", Content: "1.2.3.4"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)
	s.mu.Lock()
	s.currentIndex = 2 // 当前指向 server
	s.lastCheckDay = "2000-01-01"
	s.mu.Unlock()

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 0 {
		t.Fatalf("跨天应切回最早可用目标 A，实际 index=%d", s.currentIndex)
	}
	if sw.lastContent != "a.workers.dev" {
		t.Fatalf("应 PATCH 回 A，实际 %s", sw.lastContent)
	}
}

// 每日回切跳过配额仍超限的目标：A 超限、B 可用 → 切回 B 而非 A
func TestDailyResetSkipsOverLimit(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{record: dns.Record{Type: "A", Content: "1.2.3.4"}}
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
	if sw.lastContent != "b.workers.dev" {
		t.Fatalf("应 PATCH 到 B，实际 %s", sw.lastContent)
	}
}

// 冷却：切换后冷却期内不执行新的切换
func TestCooldownBlocksSwitch(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
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
	if sw.patchCalls != 0 {
		t.Fatalf("冷却期内不应有 PATCH 调用")
	}
}

// 手动切换：进入手动模式，tick 不自动干预
func TestManualOverride(t *testing.T) {
	down := downSrv(503)
	up := upSrv()
	defer down.Close()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(down.URL, up.URL, defaultProbe(), sw, q.query)

	if err := s.ManualSwitch(1); err != nil {
		t.Fatal(err)
	}
	if s.state != StateManual || s.manualIndex != 1 {
		t.Fatalf("手动切换后应 manual+1，实际 %s/%d", s.state, s.manualIndex)
	}
	if sw.lastContent != "b.workers.dev" {
		t.Fatalf("手动切换应 PATCH 到 B，实际 %s", sw.lastContent)
	}
	// 主继续失败也不自动动
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

// 恢复自动：按当前 DNS 实际指向对齐
func TestManualAuto(t *testing.T) {
	up := upSrv()
	defer up.Close()
	sw := &fakeSwitcher{record: dns.Record{Type: "A", Content: "1.2.3.4"}}
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

// dry-run：自动切换只决策不 PATCH
func TestDryRunDoesNotPatch(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)
	s.dryRun = true

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.currentIndex != 1 {
		t.Fatalf("dry-run 也应完成状态迁移，实际 index=%d", s.currentIndex)
	}
	if sw.patchCalls != 0 {
		t.Fatalf("dry-run 自动切换不应 PATCH，实际 %d 次", sw.patchCalls)
	}
}

// dry-run：手动切换放行（人有意识的操作应真实生效）
func TestDryRunManualStillPatches(t *testing.T) {
	up := upSrv()
	defer up.Close()
	sw := &fakeSwitcher{}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, (&fakeQuota{}).query)
	s.dryRun = true

	if err := s.ManualSwitch(2); err != nil {
		t.Fatal(err)
	}
	if sw.patchCalls != 1 {
		t.Fatalf("dry-run 手动切换应真实 PATCH，实际 %d 次", sw.patchCalls)
	}
	if s.state != StateManual {
		t.Fatalf("手动切换应进入 manual，实际 %s", s.state)
	}
}

// PATCH 失败：回滚 currentIndex，状态机与真实 DNS 保持一致
func TestPatchFailRollback(t *testing.T) {
	up := upSrv()
	defer up.Close()
	q := &fakeQuota{usages: map[string]cf.Usage{"accA": {Used: 95000, Quota: 100000, Percent: 95, OverLimit: true}}}
	sw := &fakeSwitcher{patchErr: fmt.Errorf("cf api down"), record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(up.URL, up.URL, defaultProbe(), sw, q.query)

	s.mu.Lock()
	err := s.doSwitch(1, "quota", "测试")
	s.mu.Unlock()
	if err == nil {
		t.Fatalf("PATCH 失败应返回错误")
	}
	if s.currentIndex != 0 {
		t.Fatalf("PATCH 失败应回滚到原目标，实际 index=%d", s.currentIndex)
	}
	// 事件应记录失败
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
	sw := &fakeSwitcher{}
	s := mkSite(down.URL, down.URL, defaultProbe(), sw, q.query)

	for i := 0; i < 5; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.currentIndex != 0 {
		t.Fatalf("A 超限但后继全挂应保持现状（nextAvailable 返回 -1），实际 index=%d", s.currentIndex)
	}
	if sw.patchCalls != 0 {
		t.Fatalf("不应 PATCH")
	}
}

// Snapshot 结构完整性（含目标队列与事件）
func TestSnapshot(t *testing.T) {
	up := upSrv()
	defer up.Close()
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
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
	if snap.Targets[2].Name != "server" || snap.Targets[2].QuotaAccount != "" {
		t.Fatalf("兜底目标应为无限额度: %+v", snap.Targets[2])
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
