package dataplane

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ReqEntry 一条最近请求记录（用于状态面板）
type ReqEntry struct {
	Time     string `json:"time"`
	Host     string `json:"host"`
	Site     string `json:"site"`
	Path     string `json:"path"`
	Upstream string `json:"upstream"`
}

// UpstreamStatus 面板用上游状态
type UpstreamStatus struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Healthy  bool   `json:"healthy"`
	Enabled  bool   `json:"enabled"`
	Tripped  bool   `json:"tripped"` // 请求级熔断中（连续转发失败临时剔除）
	Weight   int    `json:"weight"`
	Priority int    `json:"priority"`
	InFlight int64  `json:"inFlight"`
	Total    int64  `json:"total"`
}

// SiteStatus 面板用站点状态
type SiteStatus struct {
	Domain    string           `json:"domain"`
	Strategy  string           `json:"strategy"`
	Upstreams []UpstreamStatus `json:"upstreams"`
}

// Status 面板状态快照
type Status struct {
	Time  string       `json:"time"`
	Sites []SiteStatus `json:"sites"`
	Log   []ReqEntry   `json:"log"`
}

// Balancer 多站点分流器：按 Host 头（域名）路由到对应站点的上游组。
// 未匹配到任何站点的域名（如管理入口）转交给 unmatched（通常渲染状态面板）。
type Balancer struct {
	sites  []*Site
	byHost map[string]*Site

	unmatched http.Handler // 未匹配域名时的处理器（管理面板），需自行鉴权
	timeout   time.Duration // 单次转发超时；超时计入上游熔断失败

	logMu  sync.Mutex
	reqLog []ReqEntry
	maxLog int
}

// NewBalancer 构造分流器，按域名建立路由表。
// unmatched 处理未匹配域名的请求（管理面板入口），其内部负责 token 鉴权与渲染；
// timeout 为单次转发超时（超时 → 502 + 上游熔断计数）。
func NewBalancer(sites []*Site, unmatched http.Handler, timeout time.Duration) *Balancer {
	b := &Balancer{
		sites:     sites,
		byHost:    make(map[string]*Site, len(sites)),
		unmatched: unmatched,
		timeout:   timeout,
		maxLog:    200,
	}
	for _, s := range sites {
		b.byHost[strings.ToLower(s.Domain)] = s
	}
	return b
}

// Sites 返回当前全部站点（供 reload 时迁移熔断等运行态）
func (b *Balancer) Sites() []*Site { return b.sites }

// matchSite 按 Host 头匹配站点（忽略大小写与端口）
func (b *Balancer) matchSite(host string) *Site {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return b.byHost[host]
}

// ServeHTTP 实现 http.Handler
func (b *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 禁用一切缓存：edge-balancer 是动态代理，任何一层 nginx/CF 缓存都会导致
	// 不同域名的内容互相污染（本次线上事故根因：openresty proxy_cache 缓存 key
	// 不区分域名，panhub 内容被所有域名共享命中）
	nc := &noCacheWriter{ResponseWriter: w}

	site := b.matchSite(r.Host)
	if site == nil {
		// 未匹配到站点：视为管理入口，转交面板处理器（内部负责鉴权与渲染）
		b.unmatched.ServeHTTP(nc, r)
		return
	}

	up := site.Pick()
	if up == nil {
		http.Error(nc, "no healthy upstream available", http.StatusServiceUnavailable)
		return
	}
	proxy := site.Proxy(up.Name)
	if proxy == nil {
		http.Error(nc, "upstream proxy not found", http.StatusInternalServerError)
		return
	}
	up.Enter()
	up.AddRequest()
	b.logRequest(r, site.Domain, up.Name)
	defer up.Leave()
	// 单次转发超时：超时返回 502 并计入上游熔断失败，后续请求自动切到其它上游
	ctx, cancel := context.WithTimeout(r.Context(), b.timeout)
	defer cancel()
	sw := &statusWriter{ResponseWriter: nc}
	proxy.ServeHTTP(sw, r.WithContext(ctx))
	// 响应 >=500（含上游 5xx 与转发层 502）视为访问失败，计入熔断
	if sw.status >= 500 {
		up.Fail()
	}
}

// noCacheWriter 强制响应带 no-store，阻止 nginx/CF 等中间层缓存动态内容
type noCacheWriter struct {
	http.ResponseWriter
}

// statusWriter 记录首次响应状态码，用于按响应判定上游是否"访问成功"
// （>=500 视为失败计入熔断：覆盖上游返回 5xx 与转发层错误两种形态）
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *noCacheWriter) WriteHeader(code int) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.ResponseWriter.WriteHeader(code)
}

func (w *noCacheWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *noCacheWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// logRequest 记录最近一条转发记录（环形缓冲）
func (b *Balancer) logRequest(r *http.Request, siteName, upstream string) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	entry := ReqEntry{
		Time:     time.Now().Format("15:04:05"),
		Host:     r.Host,
		Site:     siteName,
		Path:     r.URL.Path,
		Upstream: upstream,
	}
	b.reqLog = append(b.reqLog, entry)
	if len(b.reqLog) > b.maxLog {
		b.reqLog = b.reqLog[len(b.reqLog)-b.maxLog:]
	}
}

// Status 导出状态快照（供管理面板 /admin/api 使用）
func (b *Balancer) Status() Status {
	b.logMu.Lock()
	logCopy := make([]ReqEntry, len(b.reqLog))
	copy(logCopy, b.reqLog)
	b.logMu.Unlock()

	st := Status{
		Time:  time.Now().Format("15:04:05"),
		Sites: make([]SiteStatus, 0, len(b.sites)),
		Log:   logCopy,
	}
	for _, s := range b.sites {
		as := SiteStatus{Domain: s.Domain, Strategy: s.Strategy}
		for _, u := range s.Upstreams {
			as.Upstreams = append(as.Upstreams, UpstreamStatus{
				Name:     u.Name,
				URL:      u.URL,
				Healthy:  u.IsHealthy(),
				Enabled:  u.Enabled,
				Tripped:  u.Tripped(),
				Weight:   u.Weight,
				Priority: u.Priority,
				InFlight: u.InFlight(),
				Total:    u.TotalRequests(),
			})
		}
		st.Sites = append(st.Sites, as)
	}
	return st
}

// ServeStatusJSON 序列化状态快照（管理面板状态接口）
func (b *Balancer) ServeStatusJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(b.Status()); err != nil {
		log.Printf("admin api 序列化失败: %v", err)
	}
}
