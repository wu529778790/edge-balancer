package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// UpstreamConfig 单个上游的静态配置
type UpstreamConfig struct {
	Name     string `yaml:"name"`     // 上游名称（站点内唯一标识）
	URL      string `yaml:"url"`      // 上游地址，如 https://xxx.workers.dev
	Host     string `yaml:"host"`     // 可选：转发时使用的 Host 头；为空则保留请求原始 Host（本地源站场景）；指向 CF Worker 等需校验 Host 的服务时，填上游域名
	Weight   int    `yaml:"weight"`   // 分流权重（灰度比例）
	Priority int    `yaml:"priority"` // 优先级，越小越优先；0 表示默认同一优先级（纯权重分流）
	Health   string `yaml:"health"`   // 可选：该上游的健康检查路径，覆盖全局 health_path
	Enabled  *bool  `yaml:"enabled"`  // 可选：是否参与分流；nil 视为 true（兼容旧配置），false 则展示但不转发
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
	AdminPath      string       `yaml:"admin_path"`      // 状态面板路径，默认 /admin
	AdminToken     string       `yaml:"admin_token"`     // 状态面板访问 token（可选，空则不鉴权）
	Sites          []SiteConfig `yaml:"sites"`           // 站点列表（按域名路由）
}

// LoadConfig 加载并校验配置文件
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置: %w", err)
	}

	// 默认值
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
	if cfg.AdminPath == "" {
		cfg.AdminPath = "/admin"
	}

	// 校验站点
	if len(cfg.Sites) == 0 {
		return nil, fmt.Errorf("至少配置一个 site（域名）")
	}
	seen := make(map[string]bool)
	for i := range cfg.Sites {
		site := &cfg.Sites[i]
		if site.Domain == "" {
			return nil, fmt.Errorf("site[%d] 缺少 domain", i)
		}
		if seen[site.Domain] {
			return nil, fmt.Errorf("site 域名重复: %s", site.Domain)
		}
		seen[site.Domain] = true

		if len(site.Upstreams) == 0 {
			return nil, fmt.Errorf("site %s 至少配置一个 upstream", site.Domain)
		}
		for j := range site.Upstreams {
			up := &site.Upstreams[j]
			if up.Weight <= 0 {
				up.Weight = 1
			}
			if up.Name == "" {
				return nil, fmt.Errorf("site %s 的 upstream[%d] 缺少 name", site.Domain, j)
			}
		}
	}
	return &cfg, nil
}
