// Package admin 管理控制平面：面板 HTML、状态接口、配置 CRUD 与 Cloudflare 配额管理。
//
// 依赖注入：store（配置存储，可能为 nil = 文件模式，CRUD 返回 501）、
// balancer（数据平面状态快照）、cfg（当前配置，供 admin_path/token 路由与鉴权）、
// onChange（配置变更后的热加载回调，由上层注入以避免反向依赖）。
package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/wu529778790/edge-balancer/internal/cf"
	"github.com/wu529778790/edge-balancer/internal/config"
	"github.com/wu529778790/edge-balancer/internal/dataplane"
	"github.com/wu529778790/edge-balancer/internal/store"
)

// Handler 管理面板 HTTP 处理器
type Handler struct {
	store    *store.Store                       // 可能为 nil（文件模式）
	balancer func() *dataplane.Balancer         // 当前数据平面（每次 reload 后换新）
	cfg      func() *config.Config              // 当前配置（admin_path / admin_token 可能热更新）
	onChange func() error                       // 配置变更后热加载；nil 表示不触发
}

// New 构造管理面板处理器
func New(st *store.Store, balancer func() *dataplane.Balancer, cfg func() *config.Config, onChange func() error) *Handler {
	return &Handler{store: st, balancer: balancer, cfg: cfg, onChange: onChange}
}

// isAllowed 校验面板访问（admin_token 鉴权；空则不鉴权）
func (h *Handler) isAllowed(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	return r.URL.Query().Get("token") == token
}

// ServeHTTP 管理入口：HTML 面板 / 状态 JSON / 配置 CRUD。
// 同时承担数据平面「未匹配域名 → 渲染面板」的转交职责（任意非 API 路径都渲染 HTML）。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg()
	if !h.isAllowed(r, cfg.AdminToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 状态接口：GET {admin_path}/api
	if r.URL.Path == cfg.AdminPath+"/api" && r.Method == http.MethodGet {
		h.balancer().ServeStatusJSON(w)
		return
	}

	// 配置 CRUD 接口
	if strings.HasPrefix(r.URL.Path, cfg.AdminPath+"/api/") {
		h.serveConfigAPI(w, r, cfg)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(adminHTML)
}

// serveConfigAPI 配置管理 REST API（需数据库模式）
func (h *Handler) serveConfigAPI(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	if h.store == nil {
		http.Error(w, "configuration API requires database mode (EDGE_DB_URL)", http.StatusNotImplemented)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, cfg.AdminPath+"/api")
	seg := strings.Split(strings.Trim(path, "/"), "/")
	// seg: ["sites"] / ["sites", "{id}"] / ["sites", "{id}", "upstreams"] / ["upstreams", "{id}"] / ["settings"]

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// settings
	if len(seg) == 1 && seg[0] == "settings" {
		if r.Method == http.MethodGet {
			m, err := h.store.GetSettings()
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
				if err := h.store.SetSetting(k, v); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			if err := h.reload(); err != nil {
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
		h.handleSiteAPI(w, r, seg)
		return
	}

	// upstreams
	if len(seg) >= 1 && seg[0] == "upstreams" {
		h.handleUpstreamAPI(w, r, seg)
		return
	}

	// Cloudflare 配额
	if len(seg) >= 1 && seg[0] == "cf" {
		h.handleCFAPI(w, r, seg)
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

// reload 配置变更后热加载（回调由上层注入）
func (h *Handler) reload() error {
	if h.onChange == nil {
		return nil
	}
	return h.onChange()
}

func (h *Handler) handleSiteAPI(w http.ResponseWriter, r *http.Request, seg []string) {
	// GET /sites
	if len(seg) == 1 && r.Method == http.MethodGet {
		sites, err := h.store.ListSites()
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
		if in.Domain == "" {
			http.Error(w, "domain 不能为空", http.StatusBadRequest)
			return
		}
		id, err := h.store.CreateSite(in.Domain, in.Strategy, in.HealthPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := h.reload(); err != nil {
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
			if err := h.store.DeleteSite(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := h.reload(); err != nil {
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
			if in.Domain == "" {
				http.Error(w, "domain 不能为空", http.StatusBadRequest)
				return
			}
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if err := h.store.UpdateSite(id, in.Domain, in.Strategy, in.HealthPath, enabled); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := h.reload(); err != nil {
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
		if in.Name == "" || in.URL == "" {
			http.Error(w, "name 和 url 不能为空", http.StatusBadRequest)
			return
		}
		upID, err := h.store.CreateUpstream(id, in.Name, in.URL, in.Host, in.Weight, in.Priority, in.Health, in.CFAccount)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := h.reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]int64{"id": upID})
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (h *Handler) handleUpstreamAPI(w http.ResponseWriter, r *http.Request, seg []string) {
	// PUT/DELETE /upstreams/{id}
	if len(seg) == 2 {
		id, err := strconv.ParseInt(seg[1], 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodDelete {
			if err := h.store.DeleteUpstream(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := h.reload(); err != nil {
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
			if in.Name == "" || in.URL == "" {
				http.Error(w, "name 和 url 不能为空", http.StatusBadRequest)
				return
			}
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if err := h.store.UpdateUpstream(id, in.Name, in.URL, in.Host, in.Weight, in.Priority, in.Health, in.CFAccount, enabled); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// 手动启用时清除该上游的配额自动停用标记
			if enabled {
				h.store.ClearAutoOff(in.CFAccount, id)
			}
			if err := h.reload(); err != nil {
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
// GET  {admin_path}/api/cf        → 账号列表 + 各账号用量（实时查询）
// PUT  {admin_path}/api/cf        → 保存账号列表
// POST {admin_path}/api/cf/check  → 手动触发配额检查（自动停用/恢复），返回用量与操作结果
func (h *Handler) handleCFAPI(w http.ResponseWriter, r *http.Request, seg []string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// POST /cf/check
	if len(seg) == 2 && seg[1] == "check" && r.Method == http.MethodPost {
		result, err := h.CheckCFQuotas()
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
		accounts, err := h.store.GetCFAccounts()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		autoOff, _ := h.store.GetAutoOff()
		usages := cf.QueryAllUsages(accounts)
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
		var accounts []config.CFAccount
		if err := json.NewDecoder(r.Body).Decode(&accounts); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := h.store.SetCFAccounts(accounts); err != nil {
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
//   - 未超阈值 → 自动恢复此前被配额停用的上游（次日重置生效）
func (h *Handler) CheckCFQuotas() (map[string]interface{}, error) {
	result := map[string]interface{}{"changed": false}
	if h.store == nil {
		return result, nil
	}
	accounts, err := h.store.GetCFAccounts()
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		result["usages"] = []cf.Usage{}
		return result, nil
	}

	usages := cf.QueryAllUsages(accounts)
	autoOff, err := h.store.GetAutoOff()
	if err != nil {
		return nil, err
	}

	changed := false
	var actions []string
	for _, u := range usages {
		if u.Error != "" {
			continue
		}
		ups, err := h.store.ListUpstreamsByAccount(u.Name)
		if err != nil {
			continue
		}
		for _, up := range ups {
			if u.OverLimit && up.Enabled {
				// 超阈值：停用 + 标记
				h.store.UpdateUpstream(up.ID, up.Name, up.URL, up.Host, up.Weight, up.Priority, up.Health, up.CFAccount, false)
				h.store.AddAutoOff(u.Name, up.ID)
				changed = true
				actions = append(actions, fmt.Sprintf("账号 %s 使用率 %.1f%% → 自动停用上游 %s", u.Name, u.Percent, up.Name))
			} else if !u.OverLimit && !up.Enabled && containsID(autoOff[u.Name], up.ID) {
				// 配额恢复：重新启用（仅限自动停用标记内的）
				h.store.UpdateUpstream(up.ID, up.Name, up.URL, up.Host, up.Weight, up.Priority, up.Health, up.CFAccount, true)
				h.store.ClearAutoOff(u.Name, up.ID)
				changed = true
				actions = append(actions, fmt.Sprintf("账号 %s 使用率 %.1f%% 已恢复 → 重新启用上游 %s", u.Name, u.Percent, up.Name))
			}
		}
	}
	if changed {
		if err := h.reload(); err != nil {
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
