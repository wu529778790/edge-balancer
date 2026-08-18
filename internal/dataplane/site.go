package dataplane

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/wu529778790/edge-balancer/internal/config"
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
func NewSite(cfg config.SiteConfig, defaultStrategy, defaultHealthPath string) *Site {
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
		enabled := true
		if uc.Enabled != nil {
			enabled = *uc.Enabled
		}
		u := &Upstream{
			Name:       uc.Name,
			URL:        uc.URL,
			Weight:     uc.Weight,
			Priority:   uc.Priority,
			Health:     uc.Health,
			Site:       cfg.Domain,
			CFAccount:  uc.CFAccount,
			Enabled:    enabled,
			healthPath: uc.Health,
		}
		if u.healthPath == "" {
			u.healthPath = s.HealthPath
		}
		s.Upstreams = append(s.Upstreams, u)
		if target, err := url.Parse(uc.URL); err == nil {
			proxy := httputil.NewSingleHostReverseProxy(target)
			// 客户端主动取消（context canceled）是正常现象（刷新/切换/超时等），
			// 静默处理避免刷日志；其余转发错误才记日志并返回 502
			proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
				if errors.Is(err, context.Canceled) {
					return
				}
				log.Printf("转发 %s/%s 失败: %v", s.Domain, u.Name, err)
				http.Error(w, "bad gateway", http.StatusBadGateway)
			}
			if uc.Host != "" {
				// 指定了 host 字段：转发时重写 Host 头（CF Worker 等校验 Host 的服务必需）
				host := uc.Host
				defaultDirector := proxy.Director
				proxy.Director = func(req *http.Request) {
					defaultDirector(req)
					req.Host = host
				}
			}
			s.proxies[u.Name] = proxy
		}
	}
	return s
}

// Proxy 取上游对应的反向代理
func (s *Site) Proxy(name string) *httputil.ReverseProxy { return s.proxies[name] }

// Pick 选择上游：先按优先级取最高优先级的健康组，组内再按策略选
// 已停用（enabled=false）的上游不参与分流，但仍展示在状态面板中
func (s *Site) Pick() *Upstream {
	// 找最高优先级（数值最小）的健康组
	bestPriority := int(^uint(0) >> 1)
	var group []*Upstream
	for _, u := range s.Upstreams {
		if !u.Enabled || !u.IsHealthy() {
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
