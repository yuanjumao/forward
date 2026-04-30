package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"ai-proxy/internal/config"
	"ai-proxy/internal/healthcheck"
	"ai-proxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("[启动失败] 加载配置文件失败: %v", err)
	}

	if len(cfg.Backends) == 0 {
		log.Fatal("[启动失败] 未配置任何后端模型")
	}

	log.Printf("[启动] 加载了 %d 个后端模型配置", len(cfg.Backends))

	// 创建健康检查器
	checker := healthcheck.New(cfg)

	// 启动前先做一次全量健康检查
	log.Println("[启动] 正在执行初始健康检查...")
	checker.CheckAll()

	if !checker.HasAnyHealthy() {
		log.Fatal("[启动失败] 初始健康检查未通过，没有可用的后端模型")
	}
	healthyModels := checker.AllHealthyModels()
	log.Printf("[启动] 初始健康检查通过，%d 个模型可用: %v", len(healthyModels), healthyModels)

	// 启动定时健康检查
	checker.Start()
	defer checker.Stop()

	// 创建代理处理器
	handler := proxy.NewHandler(cfg, checker)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handler.ChatCompletions)
	mux.HandleFunc("/v1/models", handler.ListModels)
	mux.HandleFunc("/status", handler.Status)
	mux.HandleFunc("/health", handler.Health)

	server := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: mux,
	}

	// 优雅退出
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("[关闭] 正在关闭服务...")
		checker.Stop()
		server.Close()
	}()

	log.Printf("[启动] 代理服务监听在 %s", cfg.Server.Listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[错误] 服务启动失败: %v", err)
	}
	log.Println("[关闭] 服务已停止")
}
