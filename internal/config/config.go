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

	// DNS 配额轮换队列（新架构）：配置了 Targets 的站点走 failover，不参与转发。
	// 有序队列从前到后消费：配额用尽或故障 → 切下一个目标；配额每日重置 → 切回最早可用。
	// 数据面用户直连 DNS 指向的目标；本程序只做探测 + 配额监控 + 切换 DNS 记录。
	Targets []TargetConfig `yaml:"targets"` // 目标队列（有序）
	Probe   ProbeConfig    `yaml:"probe"`   // 探测参数（缺省用全局默认）
	// RoutePattern 站点级 CF Worker Route pattern（如 parse.shenzjd.com/*）。
	// 切换 = 操作这条 route：指向 worker script → 流量走 worker；删除 route → 回源 DNS A 记录（服务器兜底）。
	RoutePattern string `yaml:"route_pattern"`

	// 兼容旧配置：primary/backup 在 Normalize 时转换为 targets[0]/[1]（新配置请直接用 targets）
	Primary TargetConfig `yaml:"primary"`
	Backup  TargetConfig `yaml:"backup"`
}

// TargetConfig DNS 配额轮换队列中的一个目标。
// QuotaAccount 非空时该目标受 CF 免费配额约束（引 cf_accounts 账号），配额超限自动切下一个；
// 为空 = 无限额度兜底（如服务器 IP），只受健康约束。
//
// 切换语义（route 方案）：Script 非空的目标 = CF Worker（切换时把站点 RoutePattern 的 route 指向该 script，
// 流量被 route 接管）；Script 为空的目标 = 服务器兜底（切换时删除 route，流量回源 DNS A 记录 → 服务器）。
type TargetConfig struct {
	Name         string `yaml:"name"`          // 目标名称（面板展示）
	RecordType   string `yaml:"record_type"`   // 展示用：worker 通常 CNAME、服务器通常 A（不再 PATCH）
	DNSContent   string `yaml:"dns_content"`   // 展示用：DNS 记录内容（route 方案下固定服务器 A 记录）
	URL          string `yaml:"url"`           // 探测用 URL（服务器目标通常本地 http://127.0.0.1:<port>）
	Health       string `yaml:"health"`        // 探测路径（默认用全局 health_path）
	QuotaAccount string `yaml:"quota_account"` // 配额信号：引 cf_accounts 账号名；空=无限额度兜底
	Script       string `yaml:"script"`        // CF Worker 名（非空 = worker 目标；空 = 服务器兜底）
}

// ProbeConfig 探测与切换防抖参数
type ProbeConfig struct {
	Mode             string `yaml:"mode"`                  // server（服务器侧探测，当前支持）/ external（外部探活，预留）
	Interval         int    `yaml:"interval"`              // 探测间隔秒，默认 10
	Timeout          int    `yaml:"timeout"`               // 单次探测超时秒，默认 10
	FailThreshold    int    `yaml:"fail_threshold"`        // 判挂：连续失败次数，默认 3
	RecoverThreshold int    `yaml:"recover_threshold"`     // 判恢复：连续成功次数，默认 10
	Cooldown         int    `yaml:"cooldown"`              // 一次切换后冷却秒（防抖），默认 120
	QuotaInterval    int    `yaml:"quota_interval"`        // 配额查询间隔秒（节流 CF API），默认 300
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

// CFAccount Cloudflare 账号配额配置（settings.cf_accounts 的 JSON 载体）。
// Token 建议用 TokenEnv 从环境变量读取（不落盘）；Token 字段兼容旧数据（明文，弃用）。
type CFAccount struct {
	Name      string `json:"name"`
	Token     string `json:"token"`     // 兼容旧字段（明文存储，弃用）；优先 token_env
	TokenEnv  string `json:"token_env"` // 新：该账号配额查询 token 的环境变量名（如 CF_TOKEN_<NAME>）
	AccountID string `json:"account_id"`
	Quota     int64  `json:"quota"`     // 每日免费额度（请求数），默认 100000
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
		// 新架构（failover）站点默认值；旧 primary/backup 配置兼容转换为 targets 队列
		if site.Primary.Name != "" || site.Backup.Name != "" || len(site.Targets) > 0 {
			if len(site.Targets) == 0 {
				if site.Primary.Name != "" {
					site.Targets = append(site.Targets, site.Primary)
				}
				if site.Backup.Name != "" {
					site.Targets = append(site.Targets, site.Backup)
				}
			}
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
			if site.Probe.QuotaInterval <= 0 {
				site.Probe.QuotaInterval = 300
			}
			for j := range site.Targets {
				t := &site.Targets[j]
				if t.Health == "" {
					t.Health = site.HealthPath
				}
				if t.Health == "" {
					t.Health = cfg.HealthPath
				}
				if t.RecordType == "" {
					t.RecordType = "CNAME"
				}
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
		// failover 站点校验：目标队列至少 2 个；与旧 upstreams 互斥
		if len(site.Targets) > 0 {
			if len(site.Targets) < 2 {
				return fmt.Errorf("site %s 至少配置 2 个 targets（否则无切换意义）", site.Domain)
			}
			for j, t := range site.Targets {
				if t.Name == "" {
					return fmt.Errorf("site %s 的 targets[%d] 缺少 name", site.Domain, j)
				}
				if t.DNSContent == "" {
					return fmt.Errorf("site %s 的 targets[%d] 缺少 dns_content", site.Domain, j)
				}
				if t.QuotaAccount != "" && t.RecordType == "" {
					return fmt.Errorf("site %s 的 targets[%d] 缺少 record_type", site.Domain, j)
				}
			}
			if len(site.Upstreams) > 0 {
				return fmt.Errorf("site %s 不能同时配置 upstreams 与 targets（二选一）", site.Domain)
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
