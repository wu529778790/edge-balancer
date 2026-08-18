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
