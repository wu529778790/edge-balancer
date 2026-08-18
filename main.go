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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 数据库模式优先：配置了 EDGE_DB_URL / EDGE_DB_TOKEN 时从 Turso 读取配置并支持热加载
	store, _ := OpenStore()
	var cfg *Config
	var err error
	if store != nil {
		log.Println("配置模式：数据库（Turso），支持页面配置 + 热加载")
		cfg, err = store.LoadConfig()
	} else {
		log.Println("配置模式：本地文件（未检测到 EDGE_DB_URL）")
		cfg, err = LoadConfig(*configPath)
	}
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	app, err := NewApp(store, cfg, *configPath, ctx)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	// DB 模式：定时同步配置，页面改动自动生效（无需重启）
	if store != nil {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := app.Reload(); err != nil {
						log.Printf("配置重载失败: %v", err)
					}
				}
			}
		}()
	}

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: app,
	}

	go func() {
		log.Printf("edge-balancer 启动，监听 %s，站点 %d 个", cfg.Listen, len(cfg.Sites))
		for _, s := range cfg.Sites {
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
