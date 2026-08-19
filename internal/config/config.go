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
	Upstreams  []UpstreamConfig `yaml:"upstreams"`   // 该站点的上游列表（旧转发模型）

	// DNS 故障切换模型（新架构）：配置了 Primary/Backup 的站点走 failover，不参与转发。
	// 数据面用户直连 DNS 指向的目标；本程序只做探测 + 切换 DNS 记录。
	Primary TargetConfig `yaml:"primary"` // 主目标：DNS 平时指向它
	Backup  TargetConfig `yaml:"backup"`  // 备目标：主挂时切换指向它
	Probe   ProbeConfig  `yaml:"probe"`   // 探测参数（缺省用全局默认）
}

// TargetConfig DNS 故障切换的目标：一条 DNS 记录在「主/备」之间切换指向
type TargetConfig struct {
	Name       string `yaml:"name"`        // 目标名称（面板展示）
	RecordType string `yaml:"record_type"` // 切换后记录类型：主通常 CNAME、备通常 A
	DNSContent string `yaml:"dns_content"` // 切换后记录的 content：CNAME → 目标域名；A → IP
	URL        string `yaml:"url"`         // 探测用 URL（备目标通常本地源站 http://127.0.0.1:<port>）
	Health     string `yaml:"health"`      // 探测路径（默认用全局 health_path）
}

// ProbeConfig 探测与切换防抖参数
type ProbeConfig struct {
	Mode             string `yaml:"mode"`                  // server（服务器侧探测，当前支持）/ external（外部探活，预留）
	Interval         int    `yaml:"interval"`              // 探测间隔秒，默认 10
	Timeout          int    `yaml:"timeout"`               // 单次探测超时秒，默认 10
	FailThreshold    int    `yaml:"fail_threshold"`        // 判挂：连续失败次数，默认 3
	RecoverThreshold int    `yaml:"recover_threshold"`     // 判恢复：连续成功次数，默认 10
	Cooldown         int    `yaml:"cooldown"`              // 一次切换后冷却秒（防抖），默认 120
	LatencyThreshold int    `yaml:"latency_threshold_ms"`  // 慢阈值 ms，默认 0=不启用；>0 时连续超阈判慢挂
}

// DNSConfig 全局 DNS 切换配置
type DNSConfig struct {
	Zone     string `yaml:"zone"`      // 域名 zone，如 shenzjd.com
	TTL      int    `yaml:"ttl"`       // 记录 TTL 秒；proxied=True 时 CF 强制自动（忽略）；默认 60
	TokenEnv string `yaml:"token_env"` // 读取 API token 的环境变量名，默认 CF_API_TOKEN
	DryRun   bool   `yaml:"dry_run"`   // true=监控模式：只探测+决策+记录，不实际调用 CF API 切换（上线观察期用）
}

// Config 全局配置
type Config struct {
	Listen         string       `yaml:"listen"`          // 监听地址，默认 :8080
	HealthInterval int          `yaml:"health_interval"` // 健康检查间隔（秒），默认 10
	HealthTimeout  int          `yaml:"health_timeout"`  // 健康检查超时（秒），默认 5
	HealthPath     string       `yaml:"health_path"`     // 默认健康检查路径
	Strategy       string       `yaml:"strategy"`        // 默认负载均衡策略：least-conn（最少连接）/ weighted（加权随机，默认）
	UpstreamTimeout int         `yaml:"upstream_timeout"` // 上游转发超时（秒），默认 10；超时计入熔断失败
	RequestLog     *bool        `yaml:"request_log"`     // 是否记录最近请求（面板「请求日志」）；nil 视为 true（默认开）
	AdminPath      string       `yaml:"admin_path"`      // 状态面板路径，默认 /admin
	AdminToken     string       `yaml:"admin_token"`     // 状态面板访问 token（可选，空则不鉴权）
	DNS            DNSConfig    `yaml:"dns"`             // DNS 切换全局配置（新架构）
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
	if cfg.RequestLog == nil {
		v := true
		cfg.RequestLog = &v
	}
	if cfg.DNS.TokenEnv == "" {
		cfg.DNS.TokenEnv = "CF_API_TOKEN"
	}
	if cfg.DNS.TTL <= 0 {
		cfg.DNS.TTL = 60
	}
	for i := range cfg.Sites {
		site := &cfg.Sites[i]
		// 新架构（failover）站点默认值
		if site.Primary.Name != "" || site.Backup.Name != "" {
			if site.Probe.Mode == "" {
				site.Probe.Mode = "server"
			}
			if site.Probe.Interval <= 0 {
				site.Probe.Interval = 10
			}
			if site.Probe.Timeout <= 0 {
				site.Probe.Timeout = 10
			}
			if site.Probe.FailThreshold <= 0 {
				site.Probe.FailThreshold = 3
			}
			if site.Probe.RecoverThreshold <= 0 {
				site.Probe.RecoverThreshold = 10
			}
			if site.Probe.Cooldown <= 0 {
				site.Probe.Cooldown = 120
			}
			if site.Primary.Health == "" {
				site.Primary.Health = site.HealthPath
			}
			if site.Backup.Health == "" {
				site.Backup.Health = site.HealthPath
			}
			if site.Primary.Health == "" {
				site.Primary.Health = cfg.HealthPath
			}
			if site.Backup.Health == "" {
				site.Backup.Health = cfg.HealthPath
			}
			if site.Primary.RecordType == "" {
				site.Primary.RecordType = "CNAME"
			}
			if site.Backup.RecordType == "" {
				site.Backup.RecordType = "A"
			}
		}
		// 旧架构（转发）站点默认值
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
		// failover 站点校验：主目标必填名称与 DNS 指向；主备互斥于旧 upstreams
		if site.Primary.Name != "" || site.Backup.Name != "" {
			if site.Primary.Name == "" || site.Primary.DNSContent == "" {
				return fmt.Errorf("site %s 的 primary 需要 name 和 dns_content", site.Domain)
			}
			if site.Backup.Name == "" || site.Backup.DNSContent == "" {
				return fmt.Errorf("site %s 的 backup 需要 name 和 dns_content", site.Domain)
			}
			if len(site.Upstreams) > 0 {
				return fmt.Errorf("site %s 不能同时配置 upstreams 与 primary/backup（二选一）", site.Domain)
			}
		}
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
