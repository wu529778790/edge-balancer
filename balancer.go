package main

import (
	"math/rand"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Balancer 分流器：
//  1. 先按 priority 选「最高优先级的健康组」（实现逐级兜底）
//  2. 组内按 strategy 选上游（least-conn 最少连接 / weighted 加权随机）
//
// MVP 阶段 upstreams 列表在启动后视为只读，后续支持动态增删需加锁。
type Balancer struct {
	upstreams []*Upstream
	proxies   map[string]*httputil.ReverseProxy
	strategy  string
}

// NewBalancer 构造分流器，并为每个上游预建反向代理
func NewBalancer(upstreams []*Upstream, strategy string) *Balancer {
	b := &Balancer{
		upstreams: upstreams,
		proxies:   make(map[string]*httputil.ReverseProxy, len(upstreams)),
		strategy:  strategy,
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

// pick 选择上游：先按优先级取最高优先级的健康组，组内再按策略选
func (b *Balancer) pick() *Upstream {
	// 找最高优先级（数值最小）的健康组
	bestPriority := int(^uint(0) >> 1)
	var group []*Upstream
	for _, u := range b.upstreams {
		if !u.IsHealthy() {
			continue
		}
		if u.Priority < bestPriority {
			bestPriority = u.Priority
			group = []*Upstream{u}
		} else if u.Priority == bestPriority {
			group = append(group, u)
		}
	}
	if len(group) == 0 {
		return nil
	}

	if b.strategy == "least-conn" {
		return pickLeastConn(group)
	}
	return pickWeighted(group)
}

// pickWeighted 组内加权随机
func pickWeighted(group []*Upstream) *Upstream {
	total := 0
	for _, u := range group {
		total += u.Weight
	}
	if total <= 0 {
		return group[0]
	}
	r := rand.Intn(total)
	for _, u := range group {
		r -= u.Weight
		if r < 0 {
			return u
		}
	}
	return group[len(group)-1]
}

// pickLeastConn 组内选最少在途请求的上游，避免某一台被压垮
func pickLeastConn(group []*Upstream) *Upstream {
	best := group[0]
	for _, u := range group[1:] {
		if u.InFlight() < best.InFlight() {
			best = u
		}
	}
	return best
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
	up.Enter()
	defer up.Leave()
	proxy.ServeHTTP(w, r)
}
