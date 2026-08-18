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
	Enabled  bool   `json:"enabled"`
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
		Time:  time.Now().Format("15:04:05"),
		Sites: make([]adminSite, 0, len(b.sites)),
		Log:   logCopy,
	}
	for _, s := range b.sites {
		as := adminSite{Domain: s.Domain, Strategy: s.Strategy}
		for _, u := range s.Upstreams {
			as.Upstreams = append(as.Upstreams, adminUpstream{
				Name:     u.Name,
				URL:      u.URL,
				Healthy:  u.IsHealthy(),
				Enabled:  u.Enabled,
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
<title>edge-balancer 管理面板</title>
<style>
body{font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;background:#f6f7f9;margin:0;padding:24px;color:#1f2328}
h1{font-size:18px;margin:0 0 4px}
.sub{color:#656d76;font-size:12px;margin-bottom:16px}
.card{background:#fff;border:1px solid #e4e6eb;border-radius:10px;padding:16px;margin-bottom:16px}
h2{font-size:14px;margin:0 0 10px;font-weight:500}
table{width:100%;border-collapse:collapse;font-size:13px}
th{text-align:left;color:#656d76;font-weight:500;padding:6px 8px;border-bottom:1px solid #e4e6eb}
td{padding:7px 8px;border-bottom:1px solid #f0f1f3;vertical-align:top}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px;vertical-align:middle}
.ok{background:#1a7f37}.bad{background:#cf222e}.off{background:#888780}
.code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:#57606a}
.log{font-size:12px;color:#57606a;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;padding:2px 0}
input,select{font-size:12px;padding:5px 8px;border:1px solid #d4d7dd;border-radius:6px;background:#fff;color:#1f2328;margin:2px}
.btn{font-size:12px;padding:5px 12px;border:1px solid #d4d7dd;border-radius:6px;background:#fff;cursor:pointer;color:#1f2328;margin:2px}
.btn:hover{border-color:#185fa5;color:#185fa5}
.btn-p{background:#185fa5;border-color:#185fa5;color:#fff}
.btn-p:hover{background:#0c447c;color:#fff}
.btn-d{color:#cf222e}.btn-d:hover{border-color:#cf222e;color:#cf222e}
.row{display:flex;gap:6px;align-items:center;flex-wrap:wrap}
.site-head{display:flex;justify-content:space-between;align-items:center;margin-bottom:8px}
.site-head .tag{font-size:11px;color:#57606a;background:#f1f2f4;border-radius:10px;padding:2px 8px;margin-left:6px}
.frm{background:#f8f9fa;border:1px dashed #d4d7dd;border-radius:8px;padding:10px;margin-top:8px}
.frm label{font-size:12px;color:#57606a;display:block;margin:4px 0 2px}
.hidden{display:none}
.tabs{margin-bottom:14px;display:flex;gap:4px;border-bottom:1px solid #e4e6eb;padding-bottom:0}
.tab{padding:8px 16px;font-size:13px;cursor:pointer;color:#57606a;border-bottom:2px solid transparent}
.tab.on{color:#185fa5;border-bottom-color:#185fa5;font-weight:500}
.msg{font-size:12px;color:#1a7f37;margin-top:6px}
.err{font-size:12px;color:#cf222e;margin-top:6px}
</style>
</head>
<body>
<h1>edge-balancer 管理面板</h1>
<div class="sub" id="meta">加载中...</div>

<div class="tabs">
  <div class="tab on" id="tab-status" onclick="showTab('status')">运行状态</div>
  <div class="tab" id="tab-config" onclick="showTab('config')">配置管理</div>
</div>

<div id="panel-status">
  <div id="sites"></div>
  <div class="card"><h2>最近请求（按域名分发记录）</h2><div id="log"></div></div>
</div>

<div id="panel-config" class="hidden">
  <div class="card"><h2>全局设置</h2>
    <div class="row" style="align-items:flex-end">
      <div><label>admin_token</label><input id="set-admin-token" size="24" placeholder="面板访问 token（留空不鉴权）"></div>
      <div><label>health_interval(秒)</label><input id="set-hi" size="6" type="number" value="10"></div>
      <div><label>health_timeout(秒)</label><input id="set-ht" size="6" type="number" value="5"></div>
      <div><label>health_path</label><input id="set-hp" size="14" value="/api/health"></div>
      <div><label>默认策略</label><select id="set-strategy"><option value="least-conn">least-conn</option><option value="weighted">weighted</option></select></div>
      <button class="btn btn-p" onclick="saveSettings()">保存全局设置</button>
    </div>
    <div id="msg-settings"></div>
  </div>

  <div class="card"><h2>Cloudflare 配额（多账号）</h2>
    <div id="cf-list"></div>
    <div class="frm"><div class="row" style="align-items:flex-end">
      <div><label>账号名</label><input id="cf-name" size="10" placeholder="账号A"></div>
      <div><label>API Token（只读）</label><input id="cf-token" size="34" placeholder="CF API Token"></div>
      <div><label>Account ID</label><input id="cf-accid" size="24" placeholder="CF Account ID"></div>
      <div><label>免费额度</label><input id="cf-quota" size="8" type="number" value="100000"></div>
      <div><label>阈值%</label><input id="cf-th" size="5" type="number" value="90"></div>
      <button class="btn" onclick="addCFRow()">加入列表</button>
    </div></div>
    <div class="row" style="margin-top:8px">
      <button class="btn btn-p" onclick="saveCF()">保存账号</button>
      <button class="btn" onclick="checkCF()">立即检查配额并切换</button>
    </div>
    <div id="msg-cf"></div>
  </div>

  <div class="card"><h2>站点</h2>
    <div class="frm" id="frm-new-site">
      <div class="row" style="align-items:flex-end">
        <div><label>域名（匹配 Host 头）</label><input id="ns-domain" size="28" placeholder="example.shenzjd.com"></div>
        <div><label>策略（留空用默认）</label><select id="ns-strategy"><option value="">默认</option><option value="least-conn">least-conn</option><option value="weighted">weighted</option></select></div>
        <div><label>health_path（可选）</label><input id="ns-hp" size="14" placeholder="/api/health"></div>
        <button class="btn btn-p" onclick="createSite()">添加站点</button>
      </div>
    </div>
    <div id="sites-cfg"></div>
    <div id="msg-sites"></div>
  </div>
</div>

<script>
function qs(k){return new URLSearchParams(location.search).get(k)}
function esc(s){return String(s==null?'':s).replace(/[&<>"]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]})}
function api(path,opts){var u=path;var t=qs('token');if(t)u+=(u.indexOf('?')<0?'?':'&')+'token='+encodeURIComponent(t);return fetch(u,opts).then(function(r){return r.json().catch(function(){return {}})})}
function showTab(n){document.getElementById('tab-status').className='tab'+(n==='status'?' on':'');document.getElementById('tab-config').className='tab'+(n==='config'?' on':'');document.getElementById('panel-status').style.display=n==='status'?'block':'none';document.getElementById('panel-config').style.display=n==='config'?'block':'none';if(n==='config'){loadConfig();loadCF()}}

function loadStatus(){
  api('/admin/api').then(function(d){
    document.getElementById('meta').textContent='更新于 '+d.time+' · '+d.sites.length+' 个站点';
    var html='';
    d.sites.forEach(function(s){
      html+='<div class="card"><h2>'+esc(s.domain)+' <span class="tag">策略 '+esc(s.strategy)+'</span></h2><table><thead><tr><th>上游</th><th>地址</th><th>健康</th><th>权重</th><th>优先级</th><th>在途</th><th>累计转发</th></tr></thead><tbody>';
      s.upstreams.forEach(function(x){
        var st = x.enabled ? (x.healthy ? '健康' : '不健康') : '已停用';
        var cls = x.enabled ? (x.healthy ? 'ok' : 'bad') : 'off';
        html+='<tr><td>'+esc(x.name)+'</td><td class="code">'+esc(x.url)+'</td>'+
          '<td><span class="dot '+cls+'"></span>'+st+'</td>'+
          '<td>'+x.weight+'</td><td>'+x.priority+'</td><td>'+x.inFlight+'</td><td>'+x.total+'</td></tr>';
      });
      html+='</tbody></table></div>';
    });
    document.getElementById('sites').innerHTML=html||'(暂无站点)';
    var logHtml='';
    d.log.forEach(function(e){logHtml+='<div class="log">'+esc(e.time)+'  '+esc(e.host)+'  '+esc(e.path)+'  →  '+esc(e.site)+' / '+esc(e.upstream)+'</div>'});
    document.getElementById('log').innerHTML=logHtml||'(暂无请求)';
  }).catch(function(e){document.getElementById('meta').textContent='加载失败: '+e});
}

var gSites=[];
function loadConfig(){
  api('/admin/api/sites').then(function(sites){
    gSites=sites;
    var html='';
    sites.forEach(function(s){
      html+='<div class="card"><div class="site-head"><strong>'+esc(s.domain)+'</strong><span>'+
        '<label style="font-size:12px;color:#57606a"><input type="checkbox" '+(s.enabled?'checked':'')+' onchange="toggleSite('+s.id+',this.checked)"> 启用</label> '+
        '<button class="btn btn-d" onclick="delSite('+s.id+')">删除</button></span></div>'+
        '<table><thead><tr><th>上游</th><th>地址</th><th>host</th><th>权重</th><th>优先级</th><th>健康路径</th><th>启用</th><th></th></tr></thead><tbody>';
      s.upstreams.forEach(function(u){
        var rowCls = u.enabled ? '' : ' style="opacity:0.55"';
        var statusLbl = u.enabled ? '已启用' : '已停用';
        html+='<tr'+rowCls+'><td>'+esc(u.name)+'</td><td class="code">'+esc(u.url)+'</td><td class="code">'+esc(u.host||'-')+'</td><td>'+u.weight+'</td><td>'+u.priority+'</td><td>'+esc(u.health||'-')+'</td>'+
          '<td><label style="font-size:12px"><input type="checkbox" '+(u.enabled?'checked':'')+' onchange="toggleUp('+u.id+',this.checked)"> '+statusLbl+'</label></td>'+
          '<td><button class="btn" onclick="editUpForm('+u.id+','+s.id+')">编辑</button> '+
          '<button class="btn btn-d" onclick="delUp('+u.id+')">删除</button></td></tr>';
        html+='<tr class="hidden" id="upedit-'+u.id+'"><td colspan="8"><div class="frm"><div class="row" style="align-items:flex-end">'+
          '<div><label>name</label><input id="ue-'+u.id+'-name" value="'+esc(u.name)+'" size="10"></div>'+
          '<div><label>url</label><input id="ue-'+u.id+'-url" value="'+esc(u.url)+'" size="30"></div>'+
          '<div><label>host</label><input id="ue-'+u.id+'-host" value="'+esc(u.host||'')+'" size="22" placeholder="CF Worker 填 workers.dev 域名"></div>'+
          '<div><label>weight</label><input id="ue-'+u.id+'-weight" type="number" value="'+u.weight+'" size="5"></div>'+
          '<div><label>priority</label><input id="ue-'+u.id+'-priority" type="number" value="'+u.priority+'" size="5"></div>'+
          '<div><label>health</label><input id="ue-'+u.id+'-health" value="'+esc(u.health||'')+'" size="10" placeholder="可选"></div>'+
          '<div><label>CF账号</label><input id="ue-'+u.id+'-cf" value="'+esc(u.cf_account||'')+'" size="10" placeholder="配额账号名"></div>'+
          '<button class="btn btn-p" onclick="saveUp('+u.id+')">保存</button> '+
          '<button class="btn" onclick="editUpForm('+u.id+')">取消</button></div></div></td></tr>';
      });
      html+='</tbody></table>'+
        '<div class="frm"><div class="row" style="align-items:flex-end">'+
        '<div><label>新增上游 name</label><input id="nu-'+s.id+'-name" size="10" placeholder="cf-worker"></div>'+
        '<div><label>url</label><input id="nu-'+s.id+'-url" size="30" placeholder="https://xxx.workers.dev/"></div>'+
        '<div><label>host</label><input id="nu-'+s.id+'-host" size="22" placeholder="CF Worker 填域名，本地源站留空"></div>'+
        '<div><label>weight</label><input id="nu-'+s.id+'-weight" type="number" value="1" size="5"></div>'+
        '<div><label>priority</label><input id="nu-'+s.id+'-priority" type="number" value="1" size="5"></div>'+
        '<div><label>CF账号</label><input id="nu-'+s.id+'-cf" size="10" placeholder="配额账号名"></div>'+
        '<button class="btn btn-p" onclick="addUp('+s.id+')">添加上游</button></div></div>'+
        '</div>';
    });
    document.getElementById('sites-cfg').innerHTML=html||'(暂无站点，先添加一个)';
    if(sites.length)loadSettings();
  }).catch(function(e){document.getElementById('msg-sites').className='err';document.getElementById('msg-sites').textContent='加载配置失败: '+e});
}

function loadSettings(){
  api('/admin/api/settings').then(function(m){
    if(m['admin_token']!=null)document.getElementById('set-admin-token').value=m['admin_token'];
    if(m['health_interval'])document.getElementById('set-hi').value=m['health_interval'];
    if(m['health_timeout'])document.getElementById('set-ht').value=m['health_timeout'];
    if(m['health_path'])document.getElementById('set-hp').value=m['health_path'];
    if(m['strategy'])document.getElementById('set-strategy').value=m['strategy'];
  });
}

function saveSettings(){
  var body={};
  body['admin_token']=document.getElementById('set-admin-token').value;
  body['health_interval']=document.getElementById('set-hi').value;
  body['health_timeout']=document.getElementById('set-ht').value;
  body['health_path']=document.getElementById('set-hp').value;
  body['strategy']=document.getElementById('set-strategy').value;
  api('/admin/api/settings',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(function(){
    document.getElementById('msg-settings').className='msg';document.getElementById('msg-settings').textContent='全局设置已保存并生效';
  }).catch(function(e){document.getElementById('msg-settings').className='err';document.getElementById('msg-settings').textContent='保存失败: '+e});
}

function createSite(){
  api('/admin/api/sites',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({
    domain:document.getElementById('ns-domain').value,
    strategy:document.getElementById('ns-strategy').value,
    health_path:document.getElementById('ns-hp').value
  })}).then(function(){document.getElementById('ns-domain').value='';loadConfig()});
}
function toggleSite(id,on){api('/admin/api/sites/'+id,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:on})}).then(loadConfig)}
function toggleUp(id,on){api('/admin/api/upstreams/'+id,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:on})}).then(loadConfig)}
function delSite(id){var s=gSites.find(function(x){return x.id===id});if(!s)return;if(!confirm('删除站点 '+s.domain+' 及其全部上游？'))return;api('/admin/api/sites/'+id,{method:'DELETE'}).then(loadConfig)}
function addUp(sid){
  api('/admin/api/sites/'+sid+'/upstreams',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({
    name:document.getElementById('nu-'+sid+'-name').value,
    url:document.getElementById('nu-'+sid+'-url').value,
    host:document.getElementById('nu-'+sid+'-host').value,
    weight:+document.getElementById('nu-'+sid+'-weight').value||1,
    priority:+document.getElementById('nu-'+sid+'-priority').value||0,
    cf_account:document.getElementById('nu-'+sid+'-cf').value
  })}).then(loadConfig);
}
function editUpForm(id){toggle('upedit-'+id)}
function saveUp(id){
  api('/admin/api/upstreams/'+id,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({
    name:document.getElementById('ue-'+id+'-name').value,
    url:document.getElementById('ue-'+id+'-url').value,
    host:document.getElementById('ue-'+id+'-host').value,
    weight:+document.getElementById('ue-'+id+'-weight').value||1,
    priority:+document.getElementById('ue-'+id+'-priority').value||0,
    health:document.getElementById('ue-'+id+'-health').value,
    cf_account:document.getElementById('ue-'+id+'-cf').value
  })}).then(function(){toggle('upedit-'+id);loadConfig()});
}
function delUp(id){if(!confirm('删除该上游？'))return;api('/admin/api/upstreams/'+id,{method:'DELETE'}).then(loadConfig)}
function toggle(id){document.getElementById(id).classList.toggle('hidden')}

var gCF=[];
function addCFRow(){
  gCF.push({
    name:document.getElementById('cf-name').value,
    token:document.getElementById('cf-token').value,
    account_id:document.getElementById('cf-accid').value,
    quota:+document.getElementById('cf-quota').value||100000,
    threshold:+document.getElementById('cf-th').value||90
  });
  document.getElementById('cf-name').value='';document.getElementById('cf-token').value='';document.getElementById('cf-accid').value='';
  renderCF();
}
function delCFRow(i){gCF.splice(i,1);renderCF()}
function renderCF(){
  var html='<table><thead><tr><th>账号</th><th>Account ID</th><th>额度</th><th>阈值%</th><th></th></tr></thead><tbody>';
  gCF.forEach(function(a,i){
    html+='<tr><td>'+esc(a.name)+'</td><td class="code">'+esc(a.account_id)+'</td><td>'+a.quota+'</td><td>'+a.threshold+'</td>'+
      '<td><button class="btn btn-d" onclick="delCFRow('+i+')">移除</button></td></tr>';
  });
  html+='</tbody></table>';
  document.getElementById('cf-list').innerHTML=html||'(未配置账号)';
}
function saveCF(){
  api('/admin/api/cf',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(gCF)}).then(function(){
    document.getElementById('msg-cf').className='msg';document.getElementById('msg-cf').textContent='账号已保存';
    loadCF();
  }).catch(function(e){document.getElementById('msg-cf').className='err';document.getElementById('msg-cf').textContent='保存失败: '+e});
}
function checkCF(){
  api('/admin/api/cf/check',{method:'POST'}).then(function(d){
    var txt='';
    if(d.actions&&d.actions.length){txt='执行: '+d.actions.join('；')}
    else{txt='配额正常，无切换动作'}
    document.getElementById('msg-cf').className='msg';document.getElementById('msg-cf').textContent=txt;
    renderCFUsage(d.usages);
    loadConfig();
  }).catch(function(e){document.getElementById('msg-cf').className='err';document.getElementById('msg-cf').textContent='检查失败: '+e});
}
function renderCFUsage(usages){
  if(!usages||!usages.length)return;
  var html='<h2 style="font-size:13px;margin:10px 0 6px">用量</h2><table><thead><tr><th>账号</th><th>已用请求</th><th>额度</th><th>使用率</th><th>状态</th></tr></thead><tbody>';
  usages.forEach(function(u){
    var st = u.error ? '查询失败' : (u.over_limit ? '超阈值' : (u.auto_off ? '自动停用中' : '正常'));
    var cls = u.error ? 'bad' : (u.over_limit ? 'bad' : 'ok');
    html+='<tr><td>'+esc(u.name)+'</td><td>'+(u.error?'-':u.used)+'</td><td>'+(u.quota||'-')+'</td><td>'+(u.error?'-':u.percent.toFixed(1)+'%')+'</td>'+
      '<td><span class="dot '+cls+'"></span>'+st+'</td></tr>';
  });
  html+='</tbody></table>';
  var el=document.getElementById('cf-list');
  el.innerHTML=el.innerHTML+html;
}
function loadCF(){
  api('/admin/api/cf').then(function(d){
    gCF=d.accounts||[];
    renderCF();
    renderCFUsage(d.usages);
  });
}

loadStatus();setInterval(loadStatus,3000);
</script>
</body>
</html>`
