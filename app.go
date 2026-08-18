package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// App 应用运行时：统一入口，支持数据库配置 + 热加载（DB 模式）
// store 为 nil 时退化为纯文件模式（无热加载、无配置 CRUD）
type App struct {
	store     *Store
	cfg       *Config
	ctx       context.Context
	configPath string

	balancer      atomic.Pointer[Balancer]
	checkerCancel context.CancelFunc
	adminPath     string
	adminToken    string
}

// NewApp 构建应用并加载初始配置
func NewApp(store *Store, cfg *Config, configPath string, ctx context.Context) (*App, error) {
	a := &App{store: store, cfg: cfg, configPath: configPath, ctx: ctx}
	if err := a.reload(); err != nil {
		return nil, err
	}
	return a, nil
}

// Reload 重新加载配置（DB 模式从数据库；文件模式从配置文件），
// 重建分流器与健康检查并原子切换。供定时同步与管理 API 调用。
func (a *App) Reload() error { return a.reload() }

func (a *App) reload() error {
	var cfg *Config
	var err error
	if a.store != nil {
		cfg, err = a.store.LoadConfig()
	} else {
		cfg, err = LoadConfig(a.configPath)
	}
	if err != nil {
		return err
	}
	a.cfg = cfg
	a.adminPath = cfg.AdminPath
	a.adminToken = cfg.AdminToken

	sites := make([]*Site, 0, len(cfg.Sites))
	var upstreams []*Upstream
	for _, sc := range cfg.Sites {
		site := NewSite(sc, cfg.Strategy, cfg.HealthPath)
		sites = append(sites, site)
		upstreams = append(upstreams, site.Upstreams...)
	}

	b := NewBalancer(sites, cfg.AdminToken)
	a.balancer.Store(b)

	// 重建健康检查
	if a.checkerCancel != nil {
		a.checkerCancel()
	}
	chCtx, chCancel := context.WithCancel(a.ctx)
	a.checkerCancel = chCancel
	checker := NewHealthChecker(
		upstreams,
		time.Duration(cfg.HealthInterval)*time.Second,
		time.Duration(cfg.HealthTimeout)*time.Second,
		cfg.HealthPath,
	)
	checker.Start(chCtx)
	return nil
}

// ServeHTTP 统一入口：admin 路径走管理接口，其余按 Host 路由转发
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.adminPath != "" && (r.URL.Path == a.adminPath || strings.HasPrefix(r.URL.Path, a.adminPath+"/")) {
		a.serveAdmin(w, r)
		return
	}
	a.balancer.Load().ServeHTTP(w, r)
}

func (a *App) isAdminAllowed(r *http.Request) bool {
	if a.adminToken == "" {
		return true
	}
	return r.URL.Query().Get("token") == a.adminToken
}

// serveAdmin 管理入口：HTML 面板 / 状态 JSON / 配置 CRUD
func (a *App) serveAdmin(w http.ResponseWriter, r *http.Request) {
	if !a.isAdminAllowed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 状态接口：GET /admin/api
	if r.URL.Path == a.adminPath+"/api" && r.Method == http.MethodGet {
		a.balancer.Load().serveAdminAPI(w)
		return
	}

	// 配置 CRUD 接口
	if strings.HasPrefix(r.URL.Path, a.adminPath+"/api/") {
		a.serveConfigAPI(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(adminHTML)
}

// serveConfigAPI 配置管理 REST API（需数据库模式）
func (a *App) serveConfigAPI(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		http.Error(w, "configuration API requires database mode (EDGE_DB_URL)", http.StatusNotImplemented)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, a.adminPath+"/api")
	seg := strings.Split(strings.Trim(path, "/"), "/")
	// seg: ["sites"] / ["sites", "{id}"] / ["sites", "{id}", "upstreams"] / ["upstreams", "{id}"] / ["settings"]

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// settings
	if len(seg) == 1 && seg[0] == "settings" {
		if r.Method == http.MethodGet {
			m, err := a.store.GetSettings()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(m)
			return
		}
		if r.Method == http.MethodPut {
			var m map[string]string
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
				return
			}
			for k, v := range m {
				if err := a.store.SetSetting(k, v); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			if err := a.reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// sites
	if len(seg) >= 1 && seg[0] == "sites" {
		a.handleSiteAPI(w, r, seg)
		return
	}

	// upstreams
	if len(seg) >= 1 && seg[0] == "upstreams" {
		a.handleUpstreamAPI(w, r, seg)
		return
	}

	// Cloudflare 配额
	if len(seg) >= 1 && seg[0] == "cf" {
		a.handleCFAPI(w, r, seg)
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (a *App) handleSiteAPI(w http.ResponseWriter, r *http.Request, seg []string) {
	// GET /sites
	if len(seg) == 1 && r.Method == http.MethodGet {
		sites, err := a.store.ListSites()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(sites)
		return
	}

	// POST /sites 新建
	if len(seg) == 1 && r.Method == http.MethodPost {
		var in struct {
			Domain     string `json:"domain"`
			Strategy   string `json:"strategy"`
			HealthPath string `json:"health_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		id, err := a.store.CreateSite(in.Domain, in.Strategy, in.HealthPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := a.reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]int64{"id": id})
		return
	}

	// PUT/DELETE /sites/{id}
	if len(seg) == 2 {
		id, err := strconv.ParseInt(seg[1], 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodDelete {
			if err := a.store.DeleteSite(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := a.reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		if r.Method == http.MethodPut {
			var in struct {
				Domain     string `json:"domain"`
				Strategy   string `json:"strategy"`
				HealthPath string `json:"health_path"`
				Enabled    *bool  `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
				return
			}
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if err := a.store.UpdateSite(id, in.Domain, in.Strategy, in.HealthPath, enabled); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := a.reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
	}

	// POST /sites/{id}/upstreams 添加上游
	if len(seg) == 3 && seg[2] == "upstreams" && r.Method == http.MethodPost {
		id, err := strconv.ParseInt(seg[1], 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		var in struct {
			Name      string `json:"name"`
			URL       string `json:"url"`
			Host      string `json:"host"`
			Weight    int    `json:"weight"`
			Priority  int    `json:"priority"`
			Health    string `json:"health"`
			CFAccount string `json:"cf_account"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		upID, err := a.store.CreateUpstream(id, in.Name, in.URL, in.Host, in.Weight, in.Priority, in.Health, in.CFAccount)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := a.reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]int64{"id": upID})
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (a *App) handleUpstreamAPI(w http.ResponseWriter, r *http.Request, seg []string) {
	// PUT/DELETE /upstreams/{id}
	if len(seg) == 2 {
		id, err := strconv.ParseInt(seg[1], 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodDelete {
			if err := a.store.DeleteUpstream(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := a.reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		if r.Method == http.MethodPut {
			var in struct {
				Name      string `json:"name"`
				URL       string `json:"url"`
				Host      string `json:"host"`
				Weight    int    `json:"weight"`
				Priority  int    `json:"priority"`
				Health    string `json:"health"`
				CFAccount string `json:"cf_account"`
				Enabled   *bool  `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
				return
			}
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if err := a.store.UpdateUpstream(id, in.Name, in.URL, in.Host, in.Weight, in.Priority, in.Health, in.CFAccount, enabled); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// 手动启用时清除该上游的配额自动停用标记
			if enabled {
				a.store.ClearAutoOff(in.CFAccount, id)
			}
			if err := a.reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// handleCFAPI Cloudflare 配额管理接口
// GET  /admin/api/cf        → 账号列表 + 各账号用量（实时查询）
// PUT  /admin/api/cf        → 保存账号列表
// POST /admin/api/cf/check  → 手动触发配额检查（自动停用/恢复），返回用量与操作结果
func (a *App) handleCFAPI(w http.ResponseWriter, r *http.Request, seg []string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// POST /cf/check
	if len(seg) == 2 && seg[1] == "check" && r.Method == http.MethodPost {
		result, err := a.CheckCFQuotas()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(result)
		return
	}

	if len(seg) != 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if r.Method == http.MethodGet {
		accounts, err := a.store.GetCFAccounts()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		autoOff, _ := a.store.GetAutoOff()
		usages := QueryAllCFUsages(accounts)
		for i := range usages {
			usages[i].AutoOff = len(autoOff[usages[i].Name]) > 0
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accounts": accounts,
			"usages":   usages,
		})
		return
	}

	if r.Method == http.MethodPut {
		var accounts []CFAccount
		if err := json.NewDecoder(r.Body).Decode(&accounts); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.store.SetCFAccounts(accounts); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}

	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// CheckCFQuotas 检查所有 Cloudflare 账号用量：
//   - 超阈值 → 自动停用该账号关联的上游（记录 auto_off 标记）
//   - 未超阈值 → 自动恢复此前被配额停用的上游（下月重置生效）
func (a *App) CheckCFQuotas() (map[string]interface{}, error) {
	result := map[string]interface{}{"changed": false}
	if a.store == nil {
		return result, nil
	}
	accounts, err := a.store.GetCFAccounts()
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		result["usages"] = []CFUsage{}
		return result, nil
	}

	usages := QueryAllCFUsages(accounts)
	autoOff, err := a.store.GetAutoOff()
	if err != nil {
		return nil, err
	}

	changed := false
	var actions []string
	for _, u := range usages {
		if u.Error != "" {
			continue
		}
		ups, err := a.store.ListUpstreamsByAccount(u.Name)
		if err != nil {
			continue
		}
		for _, up := range ups {
			if u.OverLimit && up.Enabled {
				// 超阈值：停用 + 标记
				a.store.UpdateUpstream(up.ID, up.Name, up.URL, up.Host, up.Weight, up.Priority, up.Health, up.CFAccount, false)
				a.store.AddAutoOff(u.Name, up.ID)
				changed = true
				actions = append(actions, fmt.Sprintf("账号 %s 使用率 %.1f%% → 自动停用上游 %s", u.Name, u.Percent, up.Name))
			} else if !u.OverLimit && !up.Enabled && containsID(autoOff[u.Name], up.ID) {
				// 配额恢复：重新启用（仅限自动停用标记内的）
				a.store.UpdateUpstream(up.ID, up.Name, up.URL, up.Host, up.Weight, up.Priority, up.Health, up.CFAccount, true)
				a.store.ClearAutoOff(u.Name, up.ID)
				changed = true
				actions = append(actions, fmt.Sprintf("账号 %s 使用率 %.1f%% 已恢复 → 重新启用上游 %s", u.Name, u.Percent, up.Name))
			}
		}
	}
	if changed {
		if err := a.reload(); err != nil {
			return nil, err
		}
	}
	result["changed"] = changed
	result["actions"] = actions
	result["usages"] = usages
	return result, nil
}

func containsID(list []int64, id int64) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

var _ = fmt.Sprintf // keep fmt import if needed
