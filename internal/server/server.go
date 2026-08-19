// Package server 应用组装层：持有数据平面与控制平面，
// 负责配置热加载（原子重建分流器与健康检查）与顶层 HTTP 路由。
package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wu529778790/edge-balancer/internal/admin"
	"github.com/wu529778790/edge-balancer/internal/config"
	"github.com/wu529778790/edge-balancer/internal/dataplane"
	"github.com/wu529778790/edge-balancer/internal/dns"
	"github.com/wu529778790/edge-balancer/internal/failover"
	"github.com/wu529778790/edge-balancer/internal/store"
)

// Server 应用运行时：统一入口，支持数据库配置 + 热加载（DB 模式）
// store 为 nil 时退化为纯文件模式（无热加载、无配置 CRUD）
type Server struct {
	store      *store.Store
	cfg        *config.Config
	ctx        context.Context
	configPath string

	balancer      atomic.Pointer[dataplane.Balancer]
	checkerCancel context.CancelFunc
	failover      *failover.Manager // DNS 故障切换（新架构）；无 failover 站点时为 nil
	dnsClient     *dns.Client
	admin         *admin.Handler
}

// New 构建应用并加载初始配置
func New(st *store.Store, cfg *config.Config, configPath string, ctx context.Context) (*Server, error) {
	s := &Server{store: st, cfg: cfg, configPath: configPath, ctx: ctx}
	// 新架构：构造 CF DNS 客户端（token 从环境变量读）。仅当配置了 DNS.zone 且有 failover 站点时使用
	if cfg.DNS.Zone != "" {
		if c, err := dns.New(cfg.DNS.TokenEnv); err == nil {
			s.dnsClient = c
		} else {
			return nil, err
		}
	}
	s.admin = admin.New(
		st,
		func() *dataplane.Balancer { return s.balancer.Load() },
		func() *failover.Manager { return s.failover },
		func() *config.Config { return s.cfg },
		s.Reload,
	)
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload 重新加载配置（DB 模式从数据库；文件模式从配置文件），
// 重建分流器与健康检查并原子切换。供定时同步与管理 API 调用。
// 注意：failover 站点只在启动时构建一次（重启生效），reload 不重建，
// 避免 DB 模式每 5s reload 反复调用 CF API 与重置状态。
func (s *Server) Reload() error { return s.reload() }

func (s *Server) reload() error {
	var cfg *config.Config
	var err error
	if s.store != nil {
		cfg, err = s.store.LoadConfig()
	} else {
		cfg, err = config.LoadConfig(s.configPath)
	}
	if err != nil {
		return err
	}
	s.cfg = cfg

	// 首次构建 failover（DNS 故障切换）站点
	if s.failover == nil && s.dnsClient != nil {
		sites, err := failover.BuildSites(cfg, s.dnsClient)
		if err != nil {
			log.Printf("failover 构建失败（继续以转发模式运行）: %v", err)
		} else if len(sites) > 0 {
			mgr := failover.NewManager(sites, 10)
			mgr.Start(s.ctx)
			s.failover = mgr
		}
	}

	// 收集旧上游（按 域名|上游名），reload 重建时保留熔断等运行态
	var oldUpstreams map[string]*dataplane.Upstream
	if old := s.balancer.Load(); old != nil {
		oldUpstreams = make(map[string]*dataplane.Upstream)
		for _, os := range old.Sites() {
			for _, ou := range os.Upstreams {
				oldUpstreams[os.Domain+"|"+ou.Name] = ou
			}
		}
	}

	sites := make([]*dataplane.Site, 0, len(cfg.Sites))
	var upstreams []*dataplane.Upstream
	for _, sc := range cfg.Sites {
		// failover 站点不进转发器（数据面直连，无转发）
		if sc.Primary.Name != "" || sc.Backup.Name != "" {
			continue
		}
		site := dataplane.NewSite(sc, cfg.Strategy, cfg.HealthPath, oldUpstreams)
		sites = append(sites, site)
		upstreams = append(upstreams, site.Upstreams...)
	}

	b := dataplane.NewBalancer(sites, s.admin, time.Duration(cfg.UpstreamTimeout)*time.Second, cfg.RequestLog == nil || *cfg.RequestLog)
	s.balancer.Store(b)

	// 重建健康检查
	if s.checkerCancel != nil {
		s.checkerCancel()
	}
	chCtx, chCancel := context.WithCancel(s.ctx)
	s.checkerCancel = chCancel
	checker := dataplane.NewHealthChecker(
		upstreams,
		time.Duration(cfg.HealthInterval)*time.Second,
		time.Duration(cfg.HealthTimeout)*time.Second,
		cfg.HealthPath,
	)
	checker.Start(chCtx)
	return nil
}

// CheckCFQuotas 透传控制平面的配额检查（定时任务入口）
func (s *Server) CheckCFQuotas() (map[string]interface{}, error) {
	return s.admin.CheckCFQuotas()
}

// ServeHTTP 统一入口：admin 路径走管理接口，其余按 Host 路由转发
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminPath != "" && (r.URL.Path == s.cfg.AdminPath || strings.HasPrefix(r.URL.Path, s.cfg.AdminPath+"/")) {
		s.admin.ServeHTTP(w, r)
		return
	}
	// failover 站点域名：数据面直连，不再转发；直接提示（正常用户流量不会到此处）
	if s.failover != nil && s.failover.Site(hostOnly(r.Host)) != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("此域名已启用 DNS 直连模式（edge-balancer 仅作故障切换控制面，不转发数据）。请确认 DNS 记录指向。"))
		return
	}
	s.balancer.Load().ServeHTTP(w, r)
}

func hostOnly(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}
