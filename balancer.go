package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ReqEntry 一条最近请求记录（用于状态面板）
type ReqEntry struct {
	Time     string `json:"time"`
	Path     string `json:"path"`
	Upstream string `json:"upstream"`
}

// Balancer 分流器：
//  1. 先按 priority 选「最高优先级的健康组」（实现逐级兜底）
//  2. 组内按 strategy 选上游（least-conn 最少连接 / weighted 加权随机）
//
// MVP 阶段 upstreams 列表在启动后视为只读，后续支持动态增删需加锁。
type Balancer struct {
	upstreams []*Upstream
	proxies   map[string]*httputil.ReverseProxy
	strategy  string

	adminPath  string // 管理面板路径，如 /admin
	adminToken string // 管理面板访问 token（空则不鉴权）

	logMu   sync.Mutex
	reqLog  []ReqEntry
	maxLog  int
}

// NewBalancer 构造分流器，并为每个上游预建反向代理
func NewBalancer(upstreams []*Upstream, strategy, adminPath, adminToken string) *Balancer {
	b := &Balancer{
		upstreams:  upstreams,
		proxies:    make(map[string]*httputil.ReverseProxy, len(upstreams)),
		strategy:   strategy,
		adminPath:  adminPath,
		adminToken: adminToken,
		maxLog:     200,
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
	// 管理面板路由（避免与上游业务路径冲突）
	if b.adminPath != "" && (r.URL.Path == b.adminPath || strings.HasPrefix(r.URL.Path, b.adminPath+"/")) {
		b.serveAdmin(w, r)
		return
	}

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
	up.AddRequest()
	b.logRequest(r, up.Name)
	defer up.Leave()
	proxy.ServeHTTP(w, r)
}

// logRequest 记录最近一条转发记录（环形缓冲）
func (b *Balancer) logRequest(r *http.Request, name string) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	entry := ReqEntry{
		Time:     time.Now().Format("15:04:05"),
		Path:     r.URL.Path,
		Upstream: name,
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

type adminStatus struct {
	Time      string          `json:"time"`
	Strategy  string          `json:"strategy"`
	Upstreams []adminUpstream `json:"upstreams"`
	Log       []ReqEntry      `json:"log"`
}

func (b *Balancer) serveAdminAPI(w http.ResponseWriter) {
	b.logMu.Lock()
	logCopy := make([]ReqEntry, len(b.reqLog))
	copy(logCopy, b.reqLog)
	b.logMu.Unlock()

	st := adminStatus{
		Time:     time.Now().Format("15:04:05"),
		Strategy: b.strategy,
		Log:      logCopy,
	}
	for _, u := range b.upstreams {
		st.Upstreams = append(st.Upstreams, adminUpstream{
			Name:     u.Name,
			URL:      u.URL,
			Healthy:  u.IsHealthy(),
			Weight:   u.Weight,
			Priority: u.Priority,
			InFlight: u.InFlight(),
			Total:    u.TotalRequests(),
		})
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
<title>edge-balancer 状态</title>
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
<div class="card"><table>
<thead><tr><th>上游</th><th>地址</th><th>健康</th><th>权重</th><th>优先级</th><th>在途</th><th>累计转发</th></tr></thead>
<tbody id="rows"></tbody>
</table></div>
<div class="card"><h2>最近请求（转发到哪个上游）</h2><div id="log"></div></div>
<script>
function qs(k){return new URLSearchParams(location.search).get(k)}
function esc(s){return String(s).replace(/[&<>"]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]})}
function load(){
  var u='/admin/api';var t=qs('token');if(t)u+='?token='+encodeURIComponent(t);
  fetch(u).then(function(r){return r.json()}).then(function(d){
    document.getElementById('meta').textContent='更新于 '+d.time+' · 策略 '+d.strategy;
    var html='';
    d.upstreams.forEach(function(x){
      html+='<tr><td>'+esc(x.name)+'</td><td class="code">'+esc(x.url)+'</td>'+
        '<td><span class="dot '+(x.healthy?'ok':'bad')+'"></span>'+(x.healthy?'健康':'不健康')+'</td>'+
        '<td>'+x.weight+'</td><td>'+x.priority+'</td><td>'+x.inFlight+'</td><td>'+x.total+'</td></tr>';
    });
    document.getElementById('rows').innerHTML=html;
    var logHtml='';
    d.log.forEach(function(e){logHtml+='<div class="log">'+esc(e.time)+'  '+esc(e.path)+'  →  '+esc(e.upstream)+'</div>'});
    document.getElementById('log').innerHTML=logHtml||'(暂无请求)';
  }).catch(function(e){document.getElementById('meta').textContent='加载失败: '+e});
}
load();setInterval(load,3000);
</script>
</body>
</html>`
