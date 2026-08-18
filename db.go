package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
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

// migrate 初始化表结构
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
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_upstreams_site ON upstreams(site_id);
`
	_, err := s.db.Exec(schema)
	return err
}

// SiteRecord 站点及上游的数据库记录
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
	ID       int64  `json:"id"`
	SiteID   int64  `json:"site_id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Host     string `json:"host"`
	Weight   int    `json:"weight"`
	Priority int    `json:"priority"`
	Health   string `json:"health"`
	Enabled  bool   `json:"enabled"`
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
	rows, err := s.db.Query(`SELECT id, site_id, name, url, host, weight, priority, health, enabled FROM upstreams WHERE site_id = ? ORDER BY id`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ups []UpstreamRecord
	for rows.Next() {
		var u UpstreamRecord
		var enabled int
		if err := rows.Scan(&u.ID, &u.SiteID, &u.Name, &u.URL, &u.Host, &u.Weight, &u.Priority, &u.Health, &enabled); err != nil {
			return nil, err
		}
		u.Enabled = enabled != 0
		ups = append(ups, u)
	}
	return ups, rows.Err()
}

// LoadConfig 从数据库构建运行时配置（仅启用项）
func (s *Store) LoadConfig() (*Config, error) {
	cfg := &Config{
		Listen:         os.Getenv("EDGE_LISTEN"),
		HealthInterval: 10,
		HealthTimeout:  5,
		HealthPath:     "/api/health",
		Strategy:       "weighted",
		AdminPath:      "/admin",
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
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
		sc := SiteConfig{Domain: st.Domain, Strategy: st.Strategy, HealthPath: st.HealthPath}
		for _, u := range st.Upstreams {
			enabled := u.Enabled
			sc.Upstreams = append(sc.Upstreams, UpstreamConfig{
				Name:     u.Name,
				URL:      u.URL,
				Host:     u.Host,
				Weight:   u.Weight,
				Priority: u.Priority,
				Health:   u.Health,
				Enabled:  &enabled,
			})
		}
		if len(sc.Upstreams) == 0 {
			continue // 没有上游的站点跳过
		}
		cfg.Sites = append(cfg.Sites, sc)
	}
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
func (s *Store) CreateSite(domain, strategy, healthPath string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO sites(domain, strategy, health_path) VALUES(?, ?, ?)`, domain, strategy, healthPath)
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

// DeleteSite 删除站点（上游级联删除）
func (s *Store) DeleteSite(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sites WHERE id=?`, id)
	return err
}

// CreateUpstream 添加上游
func (s *Store) CreateUpstream(siteID int64, name, url, host string, weight, priority int, health string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO upstreams(site_id, name, url, host, weight, priority, health) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		siteID, name, url, host, weight, priority, health)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateUpstream 更新上游
func (s *Store) UpdateUpstream(id int64, name, url, host string, weight, priority int, health string, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.Exec(`UPDATE upstreams SET name=?, url=?, host=?, weight=?, priority=?, health=?, enabled=? WHERE id=?`,
		name, url, host, weight, priority, health, e, id)
	return err
}

// DeleteUpstream 删除上游
func (s *Store) DeleteUpstream(id int64) error {
	_, err := s.db.Exec(`DELETE FROM upstreams WHERE id=?`, id)
	return err
}
