package store

import (
	"database/sql"
	"os"
	"testing"

	"github.com/wu529778790/edge-balancer/internal/config"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
	_ "modernc.org/sqlite" // file: 协议的本地 sqlite 驱动（测试用）
)

// testStore 用本地 file: 库（无 token），验证迁移 + failover 站点 CRUD + LoadConfig
func testStore(t *testing.T) *Store {
	t.Helper()
	path := "/tmp/fo-store-test.db"
	os.Remove(path)
	db, err := sql.Open("libsql", "file:"+path)
	if err != nil {
		t.Fatalf("打开本地库: %v", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close(); os.Remove(path) })
	return s
}

func sampleTargets() []config.TargetConfig {
	return []config.TargetConfig{
		{Name: "worker-a", RecordType: "CNAME", DNSContent: "parse-shenzjd-com.shenzjd.workers.dev", URL: "https://parse-shenzjd-com.shenzjd.workers.dev", Health: "/api/health", QuotaAccount: "shenzjd"},
		{Name: "worker-b", RecordType: "CNAME", DNSContent: "parse-shenzjd-com.2509818162.workers.dev", URL: "https://parse-shenzjd-com.2509818162.workers.dev", Health: "/api/health", QuotaAccount: "2509818162"},
		{Name: "server", RecordType: "A", DNSContent: "43.128.70.75", URL: "http://127.0.0.1:5269", Health: "/api/health"},
	}
}

// failover 站点 CRUD 往返（targets 队列新模型）
func TestFailoverSitesCRUD(t *testing.T) {
	s := testStore(t)

	r := FailoverSiteRecord{
		Domain:  "parse.shenzjd.com",
		Targets: sampleTargets(),
		ProbeMode: "server", ProbeInterval: 10, ProbeTimeout: 10,
		ProbeFailThreshold: 3, ProbeCooldown: 120, ProbeQuotaInterval: 300,
	}
	id, err := s.CreateFailoverSite(r)
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id 异常: %d", id)
	}

	list, err := s.ListFailoverSites()
	if err != nil {
		t.Fatalf("列表: %v", err)
	}
	if len(list) != 1 || list[0].Domain != "parse.shenzjd.com" || len(list[0].Targets) != 3 {
		t.Fatalf("列表内容异常: %+v", list)
	}
	if list[0].Targets[0].DNSContent != "parse-shenzjd-com.shenzjd.workers.dev" || list[0].Targets[0].QuotaAccount != "shenzjd" {
		t.Fatalf("targets 序列化往返异常: %+v", list[0].Targets)
	}

	// 更新
	r.ID = id
	r.Targets[0].DNSContent = "changed.workers.dev"
	if err := s.UpdateFailoverSite(r); err != nil {
		t.Fatalf("更新: %v", err)
	}
	list2, _ := s.ListFailoverSites()
	if list2[0].Targets[0].DNSContent != "changed.workers.dev" {
		t.Fatalf("更新未生效: %+v", list2[0].Targets)
	}

	// 删除
	if err := s.DeleteFailoverSite(id); err != nil {
		t.Fatalf("删除: %v", err)
	}
	list3, _ := s.ListFailoverSites()
	if len(list3) != 0 {
		t.Fatalf("删除后应为空: %+v", list3)
	}
}

// LoadConfig 从库构建 failover（targets 队列）与 dns 全局配置
func TestLoadConfigBuildsFailover(t *testing.T) {
	s := testStore(t)
	if _, err := s.CreateFailoverSite(FailoverSiteRecord{
		Domain:  "parse.shenzjd.com",
		Targets: sampleTargets(),
		ProbeMode: "server", ProbeInterval: 10, ProbeTimeout: 10,
		ProbeFailThreshold: 3, ProbeCooldown: 120, ProbeQuotaInterval: 300,
	}); err != nil {
		t.Fatal(err)
	}
	s.SetSetting("dns_zone", "shenzjd.com")
	s.SetSetting("dns_ttl", "60")
	s.SetSetting("dns_dry_run", "1")
	// 普通转发站点
	if _, err := s.CreateSite("panhub.shenzjd.com", "least-conn", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUpstream(1, "docker", "http://127.0.0.1:5253", "", 1, 1, "", "", true); err != nil {
		t.Fatal(err)
	}

	cfg, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DNS.Zone != "shenzjd.com" || cfg.DNS.TTL != 60 || !cfg.DNS.DryRun {
		t.Fatalf("dns 全局配置未解析: %+v", cfg.DNS)
	}
	var fo, fwd int
	for _, sc := range cfg.Sites {
		if len(sc.Targets) > 0 {
			fo++
			if len(sc.Targets) != 3 || sc.Targets[0].DNSContent != "parse-shenzjd-com.shenzjd.workers.dev" || sc.Targets[2].DNSContent != "43.128.70.75" {
				t.Fatalf("targets 解析异常: %+v", sc.Targets)
			}
			if sc.Targets[0].QuotaAccount != "shenzjd" || sc.Targets[2].QuotaAccount != "" {
				t.Fatalf("quota_account 解析异常: %+v", sc.Targets)
			}
			if sc.Probe.FailThreshold != 3 || sc.Probe.Cooldown != 120 || sc.Probe.QuotaInterval != 300 {
				t.Fatalf("probe 解析异常: %+v", sc.Probe)
			}
		} else if len(sc.Upstreams) > 0 {
			fwd++
		}
	}
	if fo != 1 || fwd != 1 {
		t.Fatalf("应 1 个 failover + 1 个转发站点，实际 %d/%d", fo, fwd)
	}
}

// 旧 primary/backup 字段兼容：无 targets 时回退构造
func TestLoadConfigFallbackLegacyFields(t *testing.T) {
	s := testStore(t)
	if _, err := s.CreateFailoverSite(FailoverSiteRecord{
		Domain: "parse.shenzjd.com",
		PrimaryName: "cf-worker", PrimaryRecordType: "CNAME", PrimaryDNSContent: "parse-shenzjd-com.shenzjd.workers.dev",
		PrimaryURL: "https://parse-shenzjd-com.shenzjd.workers.dev",
		BackupName: "server", BackupRecordType: "A", BackupDNSContent: "43.128.70.75",
		BackupURL: "http://127.0.0.1:5269",
		ProbeMode: "server", ProbeInterval: 10, ProbeTimeout: 10,
		ProbeFailThreshold: 3, ProbeCooldown: 120,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Sites) != 1 || len(cfg.Sites[0].Targets) != 2 {
		t.Fatalf("旧字段应回退构造 2 个 targets，实际 %+v", cfg.Sites)
	}
	if cfg.Sites[0].Targets[0].Name != "cf-worker" || cfg.Sites[0].Targets[1].Name != "server" {
		t.Fatalf("回退构造顺序异常: %+v", cfg.Sites[0].Targets)
	}
}
