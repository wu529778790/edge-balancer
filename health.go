package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream 运行时的上游，携带健康状态
type Upstream struct {
	Name   string
	URL    string
	Weight int
	Health string // 健康检查路径（空则用全局默认）

	healthy atomic.Bool
}

// IsHealthy 是否健康
func (u *Upstream) IsHealthy() bool { return u.healthy.Load() }

// HealthChecker 定时探测上游健康状态
type HealthChecker struct {
	upstreams []*Upstream
	interval  time.Duration
	client    *http.Client
}

// NewHealthChecker 构造健康检查器
func NewHealthChecker(upstreams []*Upstream, interval, timeout time.Duration) *HealthChecker {
	return &HealthChecker{
		upstreams: upstreams,
		interval:  interval,
		client:    &http.Client{Timeout: timeout},
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
	// 启动后立即探测一轮，避免「先打满流量再发现挂了」
	h.checkAll()

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
	path := u.Health
	if path == "" {
		path = "/api/health"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := strings.TrimRight(u.URL, "/") + path

	resp, err := h.client.Get(target)
	if err != nil {
		u.healthy.Store(false)
		return
	}
	resp.Body.Close()
	u.healthy.Store(resp.StatusCode >= 200 && resp.StatusCode < 400)
}
