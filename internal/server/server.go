// Package server 应用组装层：持有数据平面与控制平面，
// 负责配置热加载（原子重建分流器与健康检查）与顶层 HTTP 路由。
package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wu529778790/edge-balancer/internal/admin"
	"github.com/wu529778790/edge-balancer/internal/config"
	"github.com/wu529778790/edge-balancer/internal/dataplane"
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
	admin         *admin.Handler
}

// New 构建应用并加载初始配置
func New(st *store.Store, cfg *config.Config, configPath string, ctx context.Context) (*Server, error) {
	s := &Server{store: st, cfg: cfg, configPath: configPath, ctx: ctx}
	s.admin = admin.New(
		st,
		func() *dataplane.Balancer { return s.balancer.Load() },
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

	sites := make([]*dataplane.Site, 0, len(cfg.Sites))
	var upstreams []*dataplane.Upstream
	for _, sc := range cfg.Sites {
		site := dataplane.NewSite(sc, cfg.Strategy, cfg.HealthPath)
		sites = append(sites, site)
		upstreams = append(upstreams, site.Upstreams...)
	}

	b := dataplane.NewBalancer(sites, s.admin, time.Duration(cfg.UpstreamTimeout)*time.Second)
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
	s.balancer.Load().ServeHTTP(w, r)
}
