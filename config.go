package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// UpstreamConfig 单个上游的静态配置
type UpstreamConfig struct {
	Name     string `yaml:"name"`     // 上游名称（唯一标识）
	URL      string `yaml:"url"`      // 上游地址，如 https://xxx.workers.dev
	Weight   int    `yaml:"weight"`   // 分流权重（灰度比例）
	Priority int    `yaml:"priority"` // 优先级，越小越优先；0 表示默认同一优先级（纯权重分流）
	Health   string `yaml:"health"`   // 可选：该上游的健康检查路径，覆盖全局 health_path
}

// Config 全局配置
type Config struct {
	Listen         string           `yaml:"listen"`          // 监听地址，默认 :8080
	HealthInterval int              `yaml:"health_interval"` // 健康检查间隔（秒），默认 10
	HealthTimeout  int              `yaml:"health_timeout"`  // 健康检查超时（秒），默认 5
	HealthPath     string           `yaml:"health_path"`     // 默认健康检查路径
	Strategy       string           `yaml:"strategy"`        // 负载均衡策略：least-conn（最少连接）/ weighted（加权随机，默认）
	AdminPath      string           `yaml:"admin_path"`      // 状态面板路径，默认 /admin
	AdminToken     string           `yaml:"admin_token"`     // 状态面板访问 token（可选，空则不鉴权）
	Upstreams      []UpstreamConfig `yaml:"upstreams"`       // 上游列表
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

	// 校验上游
	if len(cfg.Upstreams) == 0 {
		return nil, fmt.Errorf("至少配置一个 upstream")
	}
	total := 0
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Weight <= 0 {
			cfg.Upstreams[i].Weight = 1
		}
		total += cfg.Upstreams[i].Weight
	}
	if total <= 0 {
		return nil, fmt.Errorf("上游总权重必须大于 0")
	}
	return &cfg, nil
}
