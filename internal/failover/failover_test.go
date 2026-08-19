package failover

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wu529778790/edge-balancer/internal/config"
	"github.com/wu529778790/edge-balancer/internal/dns"
)

// fakeSwitcher 记录 PatchRecord 调用（不真调 CF API）
type fakeSwitcher struct {
	patchCalls int32
	lastType   string
	lastContent string
	record     dns.Record
}

func (f *fakeSwitcher) PatchRecord(zoneID, recordID, rtype, content string, ttl int, keepProxy bool) (*dns.Record, error) {
	atomic.AddInt32(&f.patchCalls, 1)
	f.lastType = rtype
	f.lastContent = content
	f.record = dns.Record{ID: recordID, Type: rtype, Name: recordID, Content: content, TTL: ttl}
	return &f.record, nil
}

func (f *fakeSwitcher) GetRecord(zoneID, recordID string) (*dns.Record, error) {
	return &f.record, nil
}

// mkSite 构造测试站点：主/备指向 httptest 服务器地址
func mkSite(primaryURL, backupURL string, probe config.ProbeConfig, sw Switcher) *Site {
	s, err := NewSite("a.test",
		config.TargetConfig{Name: "worker", RecordType: "CNAME", DNSContent: "a.workers.dev", URL: primaryURL, Health: "/api/health"},
		config.TargetConfig{Name: "server", RecordType: "A", DNSContent: "1.2.3.4", URL: backupURL, Health: "/api/health"},
		probe, sw, "zone1", "rec1", 60, false)
	if err != nil {
		panic(err)
	}
	return s
}

func defaultProbe() config.ProbeConfig {
	return config.ProbeConfig{Mode: "server", Interval: 10, Timeout: 3, FailThreshold: 3, RecoverThreshold: 10, Cooldown: 120}
}

// 探测：健康目标判定 OK
func TestProbeHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			t.Errorf("探测路径错误: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	r := probeTarget(&config.TargetConfig{Name: "x", URL: srv.URL}, "/api/health", 3*time.Second)
	if !r.OK {
		t.Fatalf("健康目标应判定 OK，实际 %+v", r)
	}
}

// 探测：5xx 判失败
func TestProbe500Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()
	r := probeTarget(&config.TargetConfig{Name: "x", URL: srv.URL}, "/api/health", 3*time.Second)
	if r.OK {
		t.Fatalf("5xx 应判失败")
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

// 判挂：主连续失败 3 次且备健康 → 切到备（PATCH A 记录）
func TestTripToBackup(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer down.Close()
	defer up.Close()
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(down.URL, up.URL, defaultProbe(), sw)

	for i := 0; i < 3; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.state != StateFailedOver {
		t.Fatalf("主连续失败 3 次应切到备，实际 state=%s", s.state)
	}
	if sw.lastContent != "1.2.3.4" || sw.lastType != "A" {
		t.Fatalf("应 PATCH 为 A→1.2.3.4，实际 %s→%s", sw.lastType, sw.lastContent)
	}
}

// 判挂不足：主失败 2 次不切换
func TestNoTripBelowThreshold(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer down.Close()
	defer up.Close()
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(down.URL, up.URL, defaultProbe(), sw)

	for i := 0; i < 2; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.state != StateActive {
		t.Fatalf("2 次失败不应切换，实际 state=%s", s.state)
	}
}

// 判恢复：备状态时主连续成功 10 次 → 切回主（PATCH CNAME）
func TestRecoverToPrimary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	sw := &fakeSwitcher{record: dns.Record{Type: "A", Content: "1.2.3.4"}}
	s := mkSite(srv.URL, srv.URL, defaultProbe(), sw)
	// 先手工切到备（绕过冷却，直接置状态）
	s.mu.Lock()
	s.state = StateFailedOver
	s.mu.Unlock()

	for i := 0; i < 10; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.state != StateActive {
		t.Fatalf("主恢复 10 次应切回，实际 state=%s", s.state)
	}
	if sw.lastContent != "a.workers.dev" || sw.lastType != "CNAME" {
		t.Fatalf("应 PATCH 为 CNAME→a.workers.dev，实际 %s→%s", sw.lastType, sw.lastContent)
	}
}

// 冷却：切换后冷却期内不反向切换
func TestCooldownBlocksReverse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	probe := defaultProbe()
	probe.Cooldown = 1
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(srv.URL, srv.URL, probe, sw)
	s.mu.Lock()
	s.state = StateFailedOver
	s.cooldownUntil = time.Now().Add(5 * time.Second) // 仍在冷却
	s.mu.Unlock()

	s.mu.Lock()
	s.tick()
	s.mu.Unlock()
	if s.state != StateFailedOver {
		t.Fatalf("冷却期内不应切回主，实际 state=%s", s.state)
	}
	if sw.patchCalls != 0 {
		t.Fatalf("冷却期内不应有 PATCH 调用")
	}
}

// 手动切换：进入手动模式，tick 不自动干预
func TestManualOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	defer up.Close()
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(srv.URL, up.URL, defaultProbe(), sw)

	if err := s.ManualSwitch("backup"); err != nil {
		t.Fatal(err)
	}
	if s.state != StateManual || s.manualTarget != "backup" {
		t.Fatalf("手动切换后应 manual+backup，实际 %s/%s", s.state, s.manualTarget)
	}
	if sw.lastContent != "1.2.3.4" {
		t.Fatalf("手动切换应 PATCH 到备，实际 %s", sw.lastContent)
	}
	// 主继续失败也不自动动
	for i := 0; i < 10; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.state != StateManual {
		t.Fatalf("手动模式下不应自动切换，实际 state=%s", s.state)
	}
}

// 恢复自动：按当前 DNS 实际指向恢复状态
func TestManualAuto(t *testing.T) {
	sw := &fakeSwitcher{record: dns.Record{Type: "A", Content: "1.2.3.4"}}
	s := mkSite("http://127.0.0.1:1", "http://127.0.0.1:1", defaultProbe(), sw)
	s.mu.Lock()
	s.state = StateManual
	s.manualTarget = "backup"
	s.mu.Unlock()

	if err := s.ManualAuto(); err != nil {
		t.Fatal(err)
	}
	if s.state != StateFailedOver {
		t.Fatalf("当前 DNS 指向备，恢复自动应为 failed_over，实际 %s", s.state)
	}
	if s.manualTarget != "" {
		t.Fatalf("恢复自动后 manualTarget 应清空")
	}
}

// BuildSites：无 failover 站点时返回空
func TestBuildSitesEmpty(t *testing.T) {
	cfg := &config.Config{Sites: []config.SiteConfig{{Domain: "a.test", Upstreams: []config.UpstreamConfig{{Name: "u", URL: "http://x"}}}}}
	cfg.DNS.Zone = "shenzjd.com"
	// 无 primary/backup → 不构建
	sites, err := BuildSites(cfg, nil)
	if err != nil {
		t.Fatalf("无 failover 站点不应报错: %v", err)
	}
	if len(sites) != 0 {
		t.Fatalf("应返回空，实际 %d 个", len(sites))
	}
}

// dry-run 监控模式：决策执行但不实际 PATCH
func TestDryRunDoesNotPatch(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer down.Close()
	defer up.Close()
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(down.URL, up.URL, defaultProbe(), sw)
	s.dryRun = true

	for i := 0; i < 3; i++ {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
	if s.state != StateFailedOver {
		t.Fatalf("dry-run 也应完成状态迁移，实际 state=%s", s.state)
	}
	if sw.patchCalls != 0 {
		t.Fatalf("dry-run 不应调用 PatchRecord，实际 %d 次", sw.patchCalls)
	}
}

// Snapshot 结构完整性（含事件）
func TestSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	sw := &fakeSwitcher{record: dns.Record{Type: "CNAME", Content: "a.workers.dev"}}
	s := mkSite(srv.URL, srv.URL, defaultProbe(), sw)
	s.mu.Lock()
	s.events = append(s.events, SwitchEvent{Time: "12:00:00", From: "worker", To: "server", Reason: "auto", Detail: "测试"})
	s.mu.Unlock()

	snap := s.Snapshot()
	if snap.Domain != "a.test" || len(snap.Events) != 1 {
		t.Fatalf("Snapshot 异常: %+v", snap)
	}
	if snap.Primary.Name != "worker" || snap.Backup.Name != "server" {
		t.Fatalf("Snapshot 主备缺失: %+v", snap)
	}
	_ = fmt.Sprint() // 保持 fmt import
}
