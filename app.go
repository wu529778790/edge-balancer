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

	b := NewBalancer(sites, cfg.AdminPath, cfg.AdminToken)
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
	w.Write([]byte(adminHTML))
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
			Name     string `json:"name"`
			URL      string `json:"url"`
			Host     string `json:"host"`
			Weight   int    `json:"weight"`
			Priority int    `json:"priority"`
			Health   string `json:"health"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		upID, err := a.store.CreateUpstream(id, in.Name, in.URL, in.Host, in.Weight, in.Priority, in.Health)
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
				Name     string `json:"name"`
				URL      string `json:"url"`
				Host     string `json:"host"`
				Weight   int    `json:"weight"`
				Priority int    `json:"priority"`
				Health   string `json:"health"`
				Enabled  *bool  `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
				return
			}
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if err := a.store.UpdateUpstream(id, in.Name, in.URL, in.Host, in.Weight, in.Priority, in.Health, enabled); err != nil {
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
	http.Error(w, "not found", http.StatusNotFound)
}

var _ = fmt.Sprintf // keep fmt import if needed
