// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /admin - Web-based administration panel
package main

import (
	"context"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	// 配置文件路径，支持环境变量覆盖
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	// 确保数据目录存在
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// 加载配置
	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize log level: LOG_LEVEL env var takes priority over config, defaulting to "info".
	logger.Init(config.GetLogLevel())

	// 环境变量覆盖密码
	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	// Surface weak-configuration warnings before serving. Runs after the
	// ADMIN_PASSWORD override so an env-set password clears that warning.
	for _, warn := range config.EvaluateSecurityWarnings() {
		logger.Warnf("SECURITY: %s", warn.Msg)
	}

	// Hard-fail on an exposed insecure posture (public bind + default password or
	// disabled auth), unless the operator explicitly opted in via
	// ALLOW_INSECURE_PUBLIC_BIND. Runs after the ADMIN_PASSWORD override so an
	// env-set password satisfies the gate.
	if warning, err := config.CheckStartupSafety(); err != nil {
		logger.Fatalf("%v", err)
	} else if warning != "" {
		logger.Warnf("SECURITY: %s", warning)
	}

	// 初始化账号池
	pool.GetPool()

	// 创建 HTTP 处理器（包含后台刷新任务）
	handler := proxy.NewHandler()

	// 启动服务器
	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())
	scheme := "http"
	if config.IsTLSEnabled() {
		scheme = "https"
	}
	logger.Infof("Kiro-Go starting on %s://%s (log level: %s)", scheme, addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: %s://%s/admin", scheme, addr)
	logger.Infof("Claude API: %s://%s/v1/messages", scheme, addr)
	logger.Infof("OpenAI API: %s://%s/v1/chat/completions", scheme, addr)

	// WriteTimeout intentionally 0: SSE streams can run for minutes while the
	// upstream model produces tokens. ReadHeaderTimeout + ReadTimeout still
	// guard against slowloris-style header/body stalls.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Serve in the background so main can wait for a shutdown signal. A clean
	// shutdown (SIGINT/SIGTERM) drains in-flight requests within a deadline,
	// stops background workers, and flushes dirty token/stats state; ungraceful
	// exit is reserved for a genuine listen failure.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		if cert, key := config.GetTLSFiles(); cert != "" && key != "" {
			// Fail fast rather than silently downgrading to cleartext on a box the
			// operator has marked TLS-on.
			if _, err := os.Stat(cert); err != nil {
				logger.Fatalf("TLS cert not readable: %v", err)
			}
			if _, err := os.Stat(key); err != nil {
				logger.Fatalf("TLS key not readable: %v", err)
			}
			logger.Infof("TLS enabled; serving https://%s", addr)
			serveErr <- srv.ListenAndServeTLS(cert, key)
		} else {
			serveErr <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			logger.Fatalf("Server failed: %v", err)
		}
	case <-ctx.Done():
		stop() // restore default signal handling so a second signal force-quits
		logger.Infof("Shutdown signal received; draining in-flight requests...")

		// Stop background workers and flush dirty state first.
		handler.Shutdown()

		// Drain in-flight requests (including SSE) within a bounded deadline.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warnf("Graceful shutdown timed out: %v (forcing close)", err)
			_ = srv.Close()
		} else {
			logger.Infof("Shutdown complete.")
		}
	}
}
