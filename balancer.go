package main

import (
	"math/rand"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Balancer 分流器：加权随机选择一个健康上游，反向代理转发。
// 灰度/切量靠「每个请求独立按权重随机决策」实现，无需外部系统。
type Balancer struct {
	upstreams []*Upstream
	proxies   map[string]*httputil.ReverseProxy
}

// NewBalancer 构造分流器，并为每个上游预建反向代理。
// 注意：MVP 阶段 upstreams 列表在启动后视为只读，若后续支持动态增删需加锁。
func NewBalancer(upstreams []*Upstream) *Balancer {
	b := &Balancer{
		upstreams: upstreams,
		proxies:   make(map[string]*httputil.ReverseProxy, len(upstreams)),
	}
	for _, u := range upstreams {
		target, err := url.Parse(u.URL)
		if err != nil {
			continue
		}
		b.proxies[u.Name] = httputil.NewSingleHostReverseProxy(target)
	}
	return b
}

// pick 从健康上游中按权重随机选一个，全部不健康返回 nil
func (b *Balancer) pick() *Upstream {
	var healthy []*Upstream
	total := 0
	for _, u := range b.upstreams {
		if u.IsHealthy() {
			healthy = append(healthy, u)
			total += u.Weight
		}
	}
	if total == 0 {
		return nil
	}

	r := rand.Intn(total)
	for _, u := range healthy {
		r -= u.Weight
		if r < 0 {
			return u
		}
	}
	return healthy[len(healthy)-1]
}

// ServeHTTP 实现 http.Handler
func (b *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	up := b.pick()
	if up == nil {
		http.Error(w, "no healthy upstream available", http.StatusServiceUnavailable)
		return
	}
	proxy, ok := b.proxies[up.Name]
	if !ok {
		http.Error(w, "upstream proxy not found", http.StatusInternalServerError)
		return
	}
	proxy.ServeHTTP(w, r)
}
