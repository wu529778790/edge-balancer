// Package store Turso(libSQL) 配置存储。
//
// 职责：连接、迁移、站点/上游/设置/CF 账号的 CRUD，以及从数据库构建运行配置。
// 从数据库构建的配置只做默认值归一（config.Normalize），不做严格校验
// （数据库模式允许空配置起步，且历史数据可能宽松——保持与文件模式不同的语义）。
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	_ "github.com/tursodatabase/libsql-client-go/libsql"

	"github.com/wu529778790/edge-balancer/internal/config"
)

// Store Turso(libSQL) 配置存储
type Store struct {
	db *sql.DB
}

// OpenStore 从环境变量连接 Turso 数据库
// EDGE_DB_URL   如 libsql://xxx.turso.io
// EDGE_DB_TOKEN 数据库鉴权 token
func OpenStore() (*Store, error) {
	url := os.Getenv("EDGE_DB_URL")
	token := os.Getenv("EDGE_DB_TOKEN")
	if url == "" || token == "" {
		return nil, fmt.Errorf("环境变量 EDGE_DB_URL / EDGE_DB_TOKEN 未设置")
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	dsn := url + sep + "authToken=" + token
	db, err := sql.Open("libsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// migrate 初始化表结构（含旧表升级）
func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sites (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  domain TEXT NOT NULL UNIQUE,
  strategy TEXT NOT NULL DEFAULT 'least-conn',
  health_path TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS upstreams (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  url TEXT NOT NULL,
  host TEXT NOT NULL DEFAULT '',
  weight INTEGER NOT NULL DEFAULT 1,
  priority INTEGER NOT NULL DEFAULT 0,
  health TEXT NOT NULL DEFAULT '',
  cf_account TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_upstreams_site ON upstreams(site_id);
`
	_, err := s.db.Exec(schema)
	if err != nil {
		return err
	}
	// 旧表升级：补 cf_account 列
	has, err := s.columnExists("upstreams", "cf_account")
	if err != nil {
		return err
	}
	if !has {
		_, err = s.db.Exec(`ALTER TABLE upstreams ADD COLUMN cf_account TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return err
		}
	}
	return nil
}

// columnExists 检查表是否已有某列（SQLite 无 IF NOT EXISTS ADD COLUMN）
func (s *Store) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// SiteRecord 站点及上游的数据库记录（供管理 API 序列化）
type SiteRecord struct {
	ID         int64            `json:"id"`
	Domain     string           `json:"domain"`
	Strategy   string           `json:"strategy"`
	HealthPath string           `json:"health_path"`
	Enabled    bool             `json:"enabled"`
	Upstreams  []UpstreamRecord `json:"upstreams"`
}

// UpstreamRecord 上游数据库记录
type UpstreamRecord struct {
	ID        int64  `json:"id"`
	SiteID    int64  `json:"site_id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Host      string `json:"host"`
	Weight    int    `json:"weight"`
	Priority  int    `json:"priority"`
	Health    string `json:"health"`
	CFAccount string `json:"cf_account"`
	Enabled   bool   `json:"enabled"`
}

// ListSites 读取全部站点（含启用/停用的上游）
func (s *Store) ListSites() ([]SiteRecord, error) {
	rows, err := s.db.Query(`SELECT id, domain, strategy, health_path, enabled FROM sites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sites = make([]SiteRecord, 0)
	for rows.Next() {
		var st SiteRecord
		var enabled int
		if err := rows.Scan(&st.ID, &st.Domain, &st.Strategy, &st.HealthPath, &enabled); err != nil {
			return nil, err
		}
		st.Enabled = enabled != 0
		sites = append(sites, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range sites {
		ups, err := s.listUpstreams(sites[i].ID)
		if err != nil {
			return nil, err
		}
		sites[i].Upstreams = ups
	}
	return sites, nil
}

func (s *Store) listUpstreams(siteID int64) ([]UpstreamRecord, error) {
	rows, err := s.db.Query(`SELECT id, site_id, name, url, host, weight, priority, health, cf_account, enabled FROM upstreams WHERE site_id = ? ORDER BY id`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ups []UpstreamRecord
	for rows.Next() {
		var u UpstreamRecord
		var enabled int
		if err := rows.Scan(&u.ID, &u.SiteID, &u.Name, &u.URL, &u.Host, &u.Weight, &u.Priority, &u.Health, &u.CFAccount, &enabled); err != nil {
			return nil, err
		}
		u.Enabled = enabled != 0
		ups = append(ups, u)
	}
	return ups, rows.Err()
}

// LoadConfig 从数据库构建运行时配置（仅启用项）。
// 只做默认值归一（config.Normalize），不做严格校验：允许空配置起步，
// 且无上游的站点跳过（不进入运行时）。
func (s *Store) LoadConfig() (*config.Config, error) {
	cfg := &config.Config{
		Listen: os.Getenv("EDGE_LISTEN"),
	}

	settings, err := s.GetSettings()
	if err != nil {
		return nil, err
	}
	if v := settings["admin_path"]; v != "" {
		cfg.AdminPath = v
	}
	cfg.AdminToken = settings["admin_token"]
	if v := settings["health_path"]; v != "" {
		cfg.HealthPath = v
	}
	if v := settings["strategy"]; v != "" {
		cfg.Strategy = v
	}
	if v := settings["upstream_timeout"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.UpstreamTimeout = n
		}
	}
	if v := settings["health_interval"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HealthInterval = n
		}
	}
	if v := settings["health_timeout"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HealthTimeout = n
		}
	}

	sites, err := s.ListSites()
	if err != nil {
		return nil, err
	}
	for _, st := range sites {
		if !st.Enabled {
			continue
		}
		sc := config.SiteConfig{Domain: st.Domain, Strategy: st.Strategy, HealthPath: st.HealthPath}
		for _, u := range st.Upstreams {
			enabled := u.Enabled
			sc.Upstreams = append(sc.Upstreams, config.UpstreamConfig{
				Name:      u.Name,
				URL:       u.URL,
				Host:      u.Host,
				Weight:    u.Weight,
				Priority:  u.Priority,
				Health:    u.Health,
				CFAccount: u.CFAccount,
				Enabled:   &enabled,
			})
		}
		if len(sc.Upstreams) == 0 {
			continue // 没有上游的站点跳过
		}
		cfg.Sites = append(cfg.Sites, sc)
	}

	config.Normalize(cfg)
	return cfg, nil
}

// GetSettings 读取全部全局设置
func (s *Store) GetSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// SetSetting 写入全局设置（upsert）
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// CreateSite 新建站点
func (s *Store) CreateSite(domain, strategy, healthPath string, enabled bool) (int64, error) {
	e := 0
	if enabled {
		e = 1
	}
	res, err := s.db.Exec(`INSERT INTO sites(domain, strategy, health_path, enabled) VALUES(?, ?, ?, ?)`, domain, strategy, healthPath, e)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSite 更新站点
func (s *Store) UpdateSite(id int64, domain, strategy, healthPath string, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.Exec(`UPDATE sites SET domain=?, strategy=?, health_path=?, enabled=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`,
		domain, strategy, healthPath, e, id)
	return err
}

// UpdateSiteEnabled 仅切换站点启用状态（不触碰其它字段，供开关使用）
func (s *Store) UpdateSiteEnabled(id int64, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.Exec(`UPDATE sites SET enabled=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, e, id)
	return err
}

// DeleteSite 删除站点（上游级联删除）
func (s *Store) DeleteSite(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sites WHERE id=?`, id)
	return err
}

// CreateUpstream 添加上游
func (s *Store) CreateUpstream(siteID int64, name, url, host string, weight, priority int, health, cfAccount string, enabled bool) (int64, error) {
	e := 0
	if enabled {
		e = 1
	}
	res, err := s.db.Exec(`INSERT INTO upstreams(site_id, name, url, host, weight, priority, health, cf_account, enabled) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		siteID, name, url, host, weight, priority, health, cfAccount, e)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateUpstream 更新上游
func (s *Store) UpdateUpstream(id int64, name, url, host string, weight, priority int, health, cfAccount string, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.Exec(`UPDATE upstreams SET name=?, url=?, host=?, weight=?, priority=?, health=?, cf_account=?, enabled=? WHERE id=?`,
		name, url, host, weight, priority, health, cfAccount, e, id)
	return err
}

// UpdateUpstreamEnabled 仅切换上游启用状态（不触碰其它字段，供开关使用）
func (s *Store) UpdateUpstreamEnabled(id int64, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.Exec(`UPDATE upstreams SET enabled=? WHERE id=?`, e, id)
	return err
}

// DeleteUpstream 删除上游
func (s *Store) DeleteUpstream(id int64) error {
	_, err := s.db.Exec(`DELETE FROM upstreams WHERE id=?`, id)
	return err
}

// SetCFAccounts 保存 Cloudflare 账号列表（settings.cf_accounts，JSON）
func (s *Store) SetCFAccounts(accounts []config.CFAccount) error {
	data, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	return s.SetSetting("cf_accounts", string(data))
}

// GetCFAccounts 读取 Cloudflare 账号列表
func (s *Store) GetCFAccounts() ([]config.CFAccount, error) {
	settings, err := s.GetSettings()
	if err != nil {
		return nil, err
	}
	raw := settings["cf_accounts"]
	if raw == "" {
		return []config.CFAccount{}, nil
	}
	var accounts []config.CFAccount
	if err := json.Unmarshal([]byte(raw), &accounts); err != nil {
		return nil, err
	}
	return accounts, nil
}

// ListUpstreamsByAccount 返回关联指定 CF 账号的上游（用于自动切换）
func (s *Store) ListUpstreamsByAccount(account string) ([]UpstreamRecord, error) {
	rows, err := s.db.Query(`SELECT id, site_id, name, url, host, weight, priority, health, cf_account, enabled FROM upstreams WHERE cf_account = ? ORDER BY id`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ups []UpstreamRecord
	for rows.Next() {
		var u UpstreamRecord
		var enabled int
		if err := rows.Scan(&u.ID, &u.SiteID, &u.Name, &u.URL, &u.Host, &u.Weight, &u.Priority, &u.Health, &u.CFAccount, &enabled); err != nil {
			return nil, err
		}
		u.Enabled = enabled != 0
		ups = append(ups, u)
	}
	return ups, rows.Err()
}

// GetAutoOff 读取配额自动停用的上游（map[账号] -> 上游 id 列表）
func (s *Store) GetAutoOff() (map[string][]int64, error) {
	settings, err := s.GetSettings()
	if err != nil {
		return nil, err
	}
	raw := settings["cf_auto_off"]
	if raw == "" {
		return map[string][]int64{}, nil
	}
	m := map[string][]int64{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// SetAutoOff 保存配额自动停用标记
func (s *Store) SetAutoOff(m map[string][]int64) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.SetSetting("cf_auto_off", string(data))
}

// AddAutoOff 把某账号下的上游加入自动停用标记
func (s *Store) AddAutoOff(account string, ids ...int64) error {
	m, err := s.GetAutoOff()
	if err != nil {
		return err
	}
	m[account] = append(m[account], ids...)
	return s.SetAutoOff(m)
}

// ClearAutoOff 移除某账号下的自动停用标记（手动启用/配额恢复时清理）
func (s *Store) ClearAutoOff(account string, ids ...int64) error {
	m, err := s.GetAutoOff()
	if err != nil {
		return err
	}
	cur := m[account]
	if len(ids) == 0 {
		delete(m, account)
		return s.SetAutoOff(m)
	}
	keep := cur[:0]
	for _, id := range cur {
		remove := false
		for _, r := range ids {
			if id == r {
				remove = true
				break
			}
		}
		if !remove {
			keep = append(keep, id)
		}
	}
	m[account] = keep
	return s.SetAutoOff(m)
}
