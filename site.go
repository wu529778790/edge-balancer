package main

import (
	"math/rand"
	"net/http/httputil"
	"net/url"
)

// Site 运行时的站点：按 Host 头（域名）路由到一组上游
type Site struct {
	Domain     string
	Strategy   string
	HealthPath string
	Upstreams  []*Upstream

	proxies map[string]*httputil.ReverseProxy
}

// NewSite 由静态配置构建运行时站点，并预建每个上游的反向代理
func NewSite(cfg SiteConfig, defaultStrategy, defaultHealthPath string) *Site {
	s := &Site{
		Domain:     cfg.Domain,
		Strategy:   cfg.Strategy,
		HealthPath: cfg.HealthPath,
		proxies:    make(map[string]*httputil.ReverseProxy, len(cfg.Upstreams)),
	}
	if s.Strategy == "" {
		s.Strategy = defaultStrategy
	}
	if s.HealthPath == "" {
		s.HealthPath = defaultHealthPath
	}
	for _, uc := range cfg.Upstreams {
		u := &Upstream{
			Name:       uc.Name,
			URL:        uc.URL,
			Weight:     uc.Weight,
			Priority:   uc.Priority,
			Health:     uc.Health,
			Site:       cfg.Domain,
			healthPath: uc.Health,
		}
		if u.healthPath == "" {
			u.healthPath = s.HealthPath
		}
		s.Upstreams = append(s.Upstreams, u)
		if target, err := url.Parse(uc.URL); err == nil {
			s.proxies[u.Name] = httputil.NewSingleHostReverseProxy(target)
		}
	}
	return s
}

// Proxy 取上游对应的反向代理
func (s *Site) Proxy(name string) *httputil.ReverseProxy { return s.proxies[name] }

// Pick 选择上游：先按优先级取最高优先级的健康组，组内再按策略选
func (s *Site) Pick() *Upstream {
	// 找最高优先级（数值最小）的健康组
	bestPriority := int(^uint(0) >> 1)
	var group []*Upstream
	for _, u := range s.Upstreams {
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

	if s.Strategy == "least-conn" {
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
