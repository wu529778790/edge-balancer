package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 构建运行时站点（按域名路由）
	sites := make([]*Site, 0, len(cfg.Sites))
	var upstreams []*Upstream
	for _, sc := range cfg.Sites {
		site := NewSite(sc, cfg.Strategy, cfg.HealthPath)
		sites = append(sites, site)
		upstreams = append(upstreams, site.Upstreams...)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 健康检查（覆盖所有站点的所有上游）
	checker := NewHealthChecker(
		upstreams,
		time.Duration(cfg.HealthInterval)*time.Second,
		time.Duration(cfg.HealthTimeout)*time.Second,
		cfg.HealthPath,
	)
	checker.Start(ctx)

	// 分流器（多站点按 Host 路由）
	balancer := NewBalancer(sites, cfg.AdminPath, cfg.AdminToken)

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: balancer,
	}

	go func() {
		log.Printf("edge-balancer 启动，监听 %s，站点 %d 个", cfg.Listen, len(sites))
		for _, s := range sites {
			log.Printf("  站点: %-32s 策略 %-10s 上游 %d 个", s.Domain, s.Strategy, len(s.Upstreams))
			for _, u := range s.Upstreams {
				log.Printf("    上游: %-16s -> %s（权重 %d）", u.Name, u.URL, u.Weight)
			}
		}
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	// 优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("收到退出信号，正在关闭...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("关闭失败: %v", err)
	}
	log.Println("已退出")
}
