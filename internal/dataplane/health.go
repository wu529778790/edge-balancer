package dataplane

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream 运行时的上游，携带健康状态和在途请求数
type Upstream struct {
	Name      string
	URL       string
	Weight    int
	Priority  int
	Health    string // 健康检查路径（空则用全局默认）
	Site      string // 所属站点域名
	CFAccount string // 关联的 Cloudflare 账号名（配额监控）
	Enabled   bool   // 是否参与分流（false 仅展示，不转发）

	healthy       atomic.Bool
	inFlight      atomic.Int64
	totalRequests atomic.Int64

	healthPath string // 最终健康检查路径（上游 > 站点 > 全局）
}

// IsHealthy 是否健康
func (u *Upstream) IsHealthy() bool { return u.healthy.Load() }

// InFlight 当前在途请求数（用于 least-conn 策略）
func (u *Upstream) InFlight() int64 { return u.inFlight.Load() }

// TotalRequests 累计转发请求数（用于状态面板）
func (u *Upstream) TotalRequests() int64 { return u.totalRequests.Load() }

// Enter 请求进入（在途 +1）
func (u *Upstream) Enter() { u.inFlight.Add(1) }

// Leave 请求完成（在途 -1）
func (u *Upstream) Leave() { u.inFlight.Add(-1) }

// AddRequest 转发成功计数
func (u *Upstream) AddRequest() { u.totalRequests.Add(1) }

// HealthChecker 定时探测上游健康状态
type HealthChecker struct {
	upstreams   []*Upstream
	interval    time.Duration
	client      *http.Client
	defaultPath string
}

// NewHealthChecker 构造健康检查器
func NewHealthChecker(upstreams []*Upstream, interval, timeout time.Duration, defaultPath string) *HealthChecker {
	return &HealthChecker{
		upstreams:   upstreams,
		interval:    interval,
		client:      &http.Client{Timeout: timeout},
		defaultPath: defaultPath,
	}
}

// Start 启动健康检查循环。初始乐观认为所有上游健康（首个请求会立即探测）。
func (h *HealthChecker) Start(ctx context.Context) {
	for _, u := range h.upstreams {
		u.healthy.Store(true)
	}
	go h.loop(ctx)
}

func (h *HealthChecker) loop(ctx context.Context) {
	// 初始乐观认为健康，第一个 interval 后才做首次探测，
	// 避免「edge-balancer 与上游同时启动、上游尚未就绪」时被误判为不健康
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.checkAll()
		}
	}
}

// checkAll 并发探测所有上游
func (h *HealthChecker) checkAll() {
	var wg sync.WaitGroup
	for _, u := range h.upstreams {
		wg.Add(1)
		go func(u *Upstream) {
			defer wg.Done()
			h.checkOne(u)
		}(u)
	}
	wg.Wait()
}

// checkOne 探测单个上游，2xx/3xx 视为健康
func (h *HealthChecker) checkOne(u *Upstream) {
	path := u.healthPath
	if path == "" {
		path = h.defaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := strings.TrimRight(u.URL, "/") + path

	resp, err := h.client.Get(target)
	healthy := false
	if err == nil {
		resp.Body.Close()
		healthy = resp.StatusCode >= 200 && resp.StatusCode < 400
	}
	if u.healthy.Load() != healthy {
		log.Printf("上游 %s/%s 健康状态变化: %v (%s)", u.Site, u.Name, healthy, target)
	}
	u.healthy.Store(healthy)
}
