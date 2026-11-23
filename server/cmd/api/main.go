package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jinda27/apaas-migration-tool/server/internal/config"
	"github.com/jinda27/apaas-migration-tool/server/internal/exporter"
	"github.com/jinda27/apaas-migration-tool/server/internal/httpapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	// 配置错误时立即退出并输出原因。
	if err != nil {
		logger.Error("invalid config", "error", err)
		os.Exit(1)
	}

	exports, err := exporter.New(cfg.Root, cfg.DataDir, cfg.Exports)
	if err != nil {
		logger.Error("exporter initialization failed", "error", err)
		os.Exit(1)
	}
	defer exports.Close()
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           httpapi.NewHandler(cfg.Name, logger, exports),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 收到退出信号后等待正在处理的请求完成。
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			logger.Error("shutdown failed", "error", err)
		}
	}()

	logger.Info("server started", "address", cfg.Address)
	// 正常关闭不作为启动失败处理。
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
