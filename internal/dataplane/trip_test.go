package dataplane

import (
	"testing"
	"time"

	"github.com/wu529778790/edge-balancer/internal/config"
)

func testSite() *Site {
	cfg := config.SiteConfig{
		Domain: "a.test",
		Upstreams: []config.UpstreamConfig{
			{Name: "u", URL: "http://127.0.0.1:1"},
		},
	}
	return NewSite(cfg, "weighted", "/api/health", nil)
}

// 连续 3 次转发失败应触发熔断
func TestTripAfterThreeFailures(t *testing.T) {
	u := testSite().Upstreams[0]
	for i := 0; i < 3; i++ {
		u.Fail()
	}
	if !u.Tripped() {
		t.Fatalf("连续 3 次失败后应处于熔断状态")
	}
}

// 未达阈值不熔断
func TestNotTrippedBelowThreshold(t *testing.T) {
	u := testSite().Upstreams[0]
	u.Fail()
	u.Fail()
	if u.Tripped() {
		t.Fatalf("仅 2 次失败不应熔断")
	}
}

// 热加载重建后熔断状态必须保留（DB 模式每 5s reload 重建，否则熔断永远不触发）
func TestTripStatePreservedAcrossReload(t *testing.T) {
	cfg := config.SiteConfig{
		Domain: "a.test",
		Upstreams: []config.UpstreamConfig{
			{Name: "u", URL: "http://127.0.0.1:1"},
		},
	}
	first := NewSite(cfg, "weighted", "/api/health", nil)
	first.Upstreams[0].failCount.Store(2)
	first.Upstreams[0].tripUntil.Store(time.Now().Add(time.Minute).Unix())

	// 模拟 reload：传入旧上游映射重建
	reloaded := NewSite(cfg, "weighted", "/api/health", map[string]*Upstream{
		"a.test|u": first.Upstreams[0],
	})
	u2 := reloaded.Upstreams[0]
	if !u2.Tripped() {
		t.Fatalf("reload 后熔断状态应保留，实际 Tripped()=false")
	}
	// 且存量失败计数应继续累积：2 + 1 = 3 次即熔断
	u2.Fail()
	if !u2.Tripped() {
		t.Fatalf("保留的失败计数应继续累计并触发熔断")
	}
}

// 内网上游同样参与熔断（内网也会挂，不熔断会 502 死循环）
func TestInternalUpstreamAlsoTrips(t *testing.T) {
	internal := &Upstream{Name: "docker", URL: "http://127.0.0.1:5253"}
	for i := 0; i < 3; i++ {
		internal.Fail()
	}
	if !internal.Tripped() {
		t.Fatalf("内网上游连续失败 3 次也应熔断")
	}
}

// 全部健康上游熔断时：Pick 降级，选"最先恢复"（熔断截止最早）的上游顶上，避免站点 503
func TestPickDegradesWhenAllTripped(t *testing.T) {
	mk := func(name string, url string, priority int, tripIn time.Duration) *Upstream {
		u := &Upstream{Name: name, URL: url, Priority: priority, Enabled: true}
		u.healthy.Store(true)
		u.tripUntil.Store(time.Now().Add(tripIn).Unix())
		return u
	}
	s := &Site{
		Domain:   "a.test",
		Strategy: "weighted",
		Upstreams: []*Upstream{
			mk("w1", "https://a.workers.dev", 1, 5*time.Second),   // 5s 后恢复
			mk("docker", "http://127.0.0.1:1", 2, 50*time.Second), // 50s 后恢复
			mk("w2", "https://b.workers.dev", 3, 20*time.Second),  // 20s 后恢复
		},
	}
	picked := s.Pick()
	if picked == nil {
		t.Fatalf("全部熔断时 Pick 不应返回 nil（应降级）")
	}
	if picked.Name != "w1" {
		t.Fatalf("降级应选最先恢复的上游 w1(5s)，实际选 %s", picked.Name)
	}
}

// 只有独苗上游时不熔断（hasSpare=false，Balancer 不调 Fail）
func TestNoTripWhenSingleUpstream(t *testing.T) {
	s := testSite() // 单上游
	if s.hasSpare() {
		t.Fatalf("单上游站点 hasSpare 应为 false")
	}
}

// 配置多个但只启用 1 个：视为独苗，同样不熔断
func TestNoTripWhenOnlyOneEnabled(t *testing.T) {
	cfg := config.SiteConfig{
		Domain: "a.test",
		Upstreams: []config.UpstreamConfig{
			{Name: "a", URL: "http://127.0.0.1:1"},
			{Name: "b", URL: "http://127.0.0.1:2"},
		},
	}
	s := NewSite(cfg, "weighted", "/api/health", nil)
	s.Upstreams[1].Enabled = false
	if s.hasSpare() {
		t.Fatalf("仅 1 个启用时 hasSpare 应为 false")
	}
}

// 有备胎（enabled > 1）时才熔断
func TestHasSpareWithTwoEnabled(t *testing.T) {
	cfg := config.SiteConfig{
		Domain: "a.test",
		Upstreams: []config.UpstreamConfig{
			{Name: "a", URL: "http://127.0.0.1:1"},
			{Name: "b", URL: "http://127.0.0.1:2"},
		},
	}
	s := NewSite(cfg, "weighted", "/api/health", nil)
	if !s.hasSpare() {
		t.Fatalf("双上游 hasSpare 应为 true")
	}
}

// 有未熔断候选时不降级：熔断中的高优先级上游被跳过，选中未熔断的
func TestPickPrefersNonTripped(t *testing.T) {
	mk := func(name string, url string, priority int, tripped bool) *Upstream {
		u := &Upstream{Name: name, URL: url, Priority: priority, Enabled: true}
		u.healthy.Store(true)
		if tripped {
			u.tripUntil.Store(time.Now().Add(time.Minute).Unix())
		}
		return u
	}
	s := &Site{
		Domain:   "a.test",
		Strategy: "weighted",
		Upstreams: []*Upstream{
			mk("w1", "https://a.workers.dev", 1, true),  // 熔断中
			mk("docker", "http://127.0.0.1:1", 2, false), // 未熔断
		},
	}
	picked := s.Pick()
	if picked == nil || picked.Name != "docker" {
		t.Fatalf("应跳过熔断中的 w1 选 docker，实际 %v", picked)
	}
}
