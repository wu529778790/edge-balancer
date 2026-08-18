package main

import (
	"bufio"
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

// Balancer 多站点分流器：按 Host 头（域名）路由到对应站点的上游组，
// 未匹配到任何站点的域名（如管理入口）直接渲染状态面板。
type Balancer struct {
	sites  []*Site
	byHost map[string]*Site

	adminPath  string // 管理面板路径，如 /admin
	adminToken string // 管理面板访问 token（空则不鉴权）

	logMu  sync.Mutex
	reqLog []ReqEntry
	maxLog int
}

// NewBalancer 构造分流器，按域名建立路由表
func NewBalancer(sites []*Site, adminPath, adminToken string) *Balancer {
	b := &Balancer{
		sites:      sites,
		byHost:     make(map[string]*Site, len(sites)),
		adminPath:  adminPath,
		adminToken: adminToken,
		maxLog:     200,
	}
	for _, s := range sites {
		b.byHost[strings.ToLower(s.Domain)] = s
	}
	return b
}

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

	// 管理面板路由（避免与上游业务路径冲突）
	if b.adminPath != "" && (r.URL.Path == b.adminPath || strings.HasPrefix(r.URL.Path, b.adminPath+"/")) {
		b.serveAdmin(nc, r)
		return
	}

	site := b.matchSite(r.Host)
	if site == nil {
		// 未匹配到站点：视为管理入口，直接渲染面板
		if !b.isAdminAllowed(r) {
			http.Error(nc, "unauthorized", http.StatusUnauthorized)
			return
		}
		nc.Header().Set("Content-Type", "text/html; charset=utf-8")
		nc.Write([]byte(adminHTML))
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
	proxy.ServeHTTP(nc, r)
}

// noCacheWriter 强制响应带 no-store，阻止 nginx/CF 等中间层缓存动态内容
type noCacheWriter struct {
	http.ResponseWriter
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

// isAdminAllowed 校验管理面板访问（token 鉴权）
func (b *Balancer) isAdminAllowed(r *http.Request) bool {
	if b.adminToken == "" {
		return true
	}
	return r.URL.Query().Get("token") == b.adminToken
}

// serveAdmin 管理面板：/admin 返回 HTML，/admin/api 返回 JSON
func (b *Balancer) serveAdmin(w http.ResponseWriter, r *http.Request) {
	if !b.isAdminAllowed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/api") {
		b.serveAdminAPI(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}

// adminUpstream 面板用上游状态
type adminUpstream struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Healthy  bool   `json:"healthy"`
	Weight   int    `json:"weight"`
	Priority int    `json:"priority"`
	InFlight int64  `json:"inFlight"`
	Total    int64  `json:"total"`
}

type adminSite struct {
	Domain    string          `json:"domain"`
	Strategy  string          `json:"strategy"`
	Upstreams []adminUpstream `json:"upstreams"`
}

type adminStatus struct {
	Time     string      `json:"time"`
	Sites    []adminSite `json:"sites"`
	Log      []ReqEntry  `json:"log"`
}

func (b *Balancer) serveAdminAPI(w http.ResponseWriter) {
	b.logMu.Lock()
	logCopy := make([]ReqEntry, len(b.reqLog))
	copy(logCopy, b.reqLog)
	b.logMu.Unlock()

	st := adminStatus{
		Time: time.Now().Format("15:04:05"),
		Log:  logCopy,
	}
	for _, s := range b.sites {
		as := adminSite{Domain: s.Domain, Strategy: s.Strategy}
		for _, u := range s.Upstreams {
			as.Upstreams = append(as.Upstreams, adminUpstream{
				Name:     u.Name,
				URL:      u.URL,
				Healthy:  u.IsHealthy(),
				Weight:   u.Weight,
				Priority: u.Priority,
				InFlight: u.InFlight(),
				Total:    u.TotalRequests(),
			})
		}
		st.Sites = append(st.Sites, as)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(st); err != nil {
		log.Printf("admin api 序列化失败: %v", err)
	}
}

const adminHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>edge-balancer 状态面板</title>
<style>
body{font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;background:#f6f7f9;margin:0;padding:24px;color:#1f2328}
h1{font-size:18px;margin:0 0 4px}
.sub{color:#656d76;font-size:12px;margin-bottom:16px}
.card{background:#fff;border:1px solid #e4e6eb;border-radius:10px;padding:16px;margin-bottom:16px}
h2{font-size:14px;margin:0 0 8px;font-weight:500}
table{width:100%;border-collapse:collapse;font-size:13px}
th{text-align:left;color:#656d76;font-weight:500;padding:6px 8px;border-bottom:1px solid #e4e6eb}
td{padding:8px;border-bottom:1px solid #f0f1f3}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px;vertical-align:middle}
.ok{background:#1a7f37}.bad{background:#cf222e}
.code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:#57606a}
.log{font-size:12px;color:#57606a;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;padding:2px 0}
</style>
</head>
<body>
<h1>edge-balancer 状态面板</h1>
<div class="sub" id="meta">加载中...</div>
<div id="sites"></div>
<div class="card"><h2>最近请求（按域名分发记录）</h2><div id="log"></div></div>
<script>
function qs(k){return new URLSearchParams(location.search).get(k)}
function esc(s){return String(s).replace(/[&<>"]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]})}
function load(){
  var u='/admin/api';var t=qs('token');if(t)u+='?token='+encodeURIComponent(t);
  fetch(u).then(function(r){return r.json()}).then(function(d){
    document.getElementById('meta').textContent='更新于 '+d.time;
    var html='';
    d.sites.forEach(function(s){
      html+='<div class="card"><h2>'+esc(s.domain)+' <span class="sub" style="font-weight:400">· 策略 '+esc(s.strategy)+'</span></h2><table><thead><tr><th>上游</th><th>地址</th><th>健康</th><th>权重</th><th>优先级</th><th>在途</th><th>累计转发</th></tr></thead><tbody>';
      s.upstreams.forEach(function(x){
        html+='<tr><td>'+esc(x.name)+'</td><td class="code">'+esc(x.url)+'</td>'+
          '<td><span class="dot '+(x.healthy?'ok':'bad')+'"></span>'+(x.healthy?'健康':'不健康')+'</td>'+
          '<td>'+x.weight+'</td><td>'+x.priority+'</td><td>'+x.inFlight+'</td><td>'+x.total+'</td></tr>';
      });
      html+='</tbody></table></div>';
    });
    document.getElementById('sites').innerHTML=html;
    var logHtml='';
    d.log.forEach(function(e){logHtml+='<div class="log">'+esc(e.time)+'  '+esc(e.host)+'  '+esc(e.path)+'  →  '+esc(e.site)+' / '+esc(e.upstream)+'</div>'});
    document.getElementById('log').innerHTML=logHtml||'(暂无请求)';
  }).catch(function(e){document.getElementById('meta').textContent='加载失败: '+e});
}
load();setInterval(load,3000);
</script>
</body>
</html>`
