// Package config 配置模型与校验（单一真源）。
//
// 文件模式（config.yaml）与数据库模式（Turso）最终都构建出本包的 Config，
// 默认值（Normalize）与结构校验（Validate）只在这里实现一次。
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// UpstreamConfig 单个上游的静态配置
type UpstreamConfig struct {
	Name      string `yaml:"name"`       // 上游名称（站点内唯一标识）
	URL       string `yaml:"url"`        // 上游地址，如 https://xxx.workers.dev
	Host      string `yaml:"host"`       // 可选：转发时使用的 Host 头；为空则保留请求原始 Host（本地源站场景）；指向 CF Worker 等需校验 Host 的服务时，填上游域名
	Weight    int    `yaml:"weight"`     // 分流权重（灰度比例）
	Priority  int    `yaml:"priority"`   // 优先级，越小越优先；0 表示默认同一优先级（纯权重分流）
	Health    string `yaml:"health"`     // 可选：该上游的健康检查路径，覆盖全局 health_path
	CFAccount string `yaml:"cf_account"` // 可选：关联的 Cloudflare 账号名（配额监控自动切换用）
	Enabled   *bool  `yaml:"enabled"`    // 可选：是否参与分流；nil 视为 true（兼容旧配置），false 则展示但不转发
}

// SiteConfig 单个站点（域名）的配置：按 Host 头路由，每个域名独立一套上游
type SiteConfig struct {
	Domain     string           `yaml:"domain"`      // 站点域名，如 panhub.shenzjd.com（匹配 Host 头）
	Strategy   string           `yaml:"strategy"`    // 该站点分流策略（空则用全局 strategy）
	HealthPath string           `yaml:"health_path"` // 该站点健康检查路径（空则用全局 health_path）
	Upstreams  []UpstreamConfig `yaml:"upstreams"`   // 该站点的上游列表
}

// Config 全局配置
type Config struct {
	Listen         string       `yaml:"listen"`          // 监听地址，默认 :8080
	HealthInterval int          `yaml:"health_interval"` // 健康检查间隔（秒），默认 10
	HealthTimeout  int          `yaml:"health_timeout"`  // 健康检查超时（秒），默认 5
	HealthPath     string       `yaml:"health_path"`     // 默认健康检查路径
	Strategy       string       `yaml:"strategy"`        // 默认负载均衡策略：least-conn（最少连接）/ weighted（加权随机，默认）
	UpstreamTimeout int         `yaml:"upstream_timeout"` // 上游转发超时（秒），默认 10；超时计入熔断失败
	AdminPath      string       `yaml:"admin_path"`      // 状态面板路径，默认 /admin
	AdminToken     string       `yaml:"admin_token"`     // 状态面板访问 token（可选，空则不鉴权）
	Sites          []SiteConfig `yaml:"sites"`           // 站点列表（按域名路由）
}

// CFAccount Cloudflare 账号配额配置（settings.cf_accounts 的 JSON 载体）
type CFAccount struct {
	Name      string `json:"name"`
	Token     string `json:"token"`
	AccountID string `json:"account_id"`
	Quota     int64  `json:"quota"`     // 每月免费额度（请求数），默认 100000
	Threshold int    `json:"threshold"` // 使用率阈值 %，默认 90
}

// Normalize 填充默认值。文件模式与数据库模式共用。
func Normalize(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 10
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = 5
	}
	if cfg.HealthPath == "" {
		cfg.HealthPath = "/api/health"
	}
	if cfg.Strategy == "" {
		cfg.Strategy = "weighted"
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 10
	}
	if cfg.AdminPath == "" {
		cfg.AdminPath = "/admin"
	}
	for i := range cfg.Sites {
		site := &cfg.Sites[i]
		for j := range site.Upstreams {
			up := &site.Upstreams[j]
			if up.Weight <= 0 {
				up.Weight = 1
			}
		}
	}
}

// Validate 结构完整性校验（不含"至少一个站点"——数据库模式允许空配置起步）。
func Validate(cfg *Config) error {
	seen := make(map[string]bool)
	for i := range cfg.Sites {
		site := &cfg.Sites[i]
		if site.Domain == "" {
			return fmt.Errorf("site[%d] 缺少 domain", i)
		}
		if seen[site.Domain] {
			return fmt.Errorf("site 域名重复: %s", site.Domain)
		}
		seen[site.Domain] = true
		for j := range site.Upstreams {
			up := &site.Upstreams[j]
			if up.Name == "" {
				return fmt.Errorf("site %s 的 upstream[%d] 缺少 name", site.Domain, j)
			}
		}
	}
	return nil
}

// LoadConfig 加载并校验配置文件（文件模式）。
// 与数据库模式不同，文件模式要求至少一个站点。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置: %w", err)
	}

	Normalize(&cfg)
	if err := Validate(&cfg); err != nil {
		return nil, err
	}
	if len(cfg.Sites) == 0 {
		return nil, fmt.Errorf("至少配置一个 site（域名）")
	}
	return &cfg, nil
}
