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

	// 构建运行时上游
	upstreams := make([]*Upstream, 0, len(cfg.Upstreams))
	for _, uc := range cfg.Upstreams {
		upstreams = append(upstreams, &Upstream{
			Name:     uc.Name,
			URL:      uc.URL,
			Weight:   uc.Weight,
			Priority: uc.Priority,
			Health:   uc.Health,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 健康检查
	checker := NewHealthChecker(
		upstreams,
		time.Duration(cfg.HealthInterval)*time.Second,
		time.Duration(cfg.HealthTimeout)*time.Second,
	)
	checker.Start(ctx)

	// 分流器
	balancer := NewBalancer(upstreams, cfg.Strategy)

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: balancer,
	}

	go func() {
		log.Printf("edge-balancer 启动，监听 %s，上游 %d 个", cfg.Listen, len(upstreams))
		for _, u := range upstreams {
			log.Printf("  上游: %-16s -> %s（权重 %d）", u.Name, u.URL, u.Weight)
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
