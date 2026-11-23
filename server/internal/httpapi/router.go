package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/jinda27/apaas-migration-tool/server/internal/exporter"
)

const Version = "0.2.0"

func NewHandler(name string, logger *slog.Logger, services ...*exporter.Service) http.Handler {
	mux := http.NewServeMux()
	// 测试可单独构造健康检查，实际服务注册完整导出 API。
	if len(services) > 0 {
		registerExport(mux, services[0])
	}
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "ok",
			"time":   time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"name":    name,
			"version": Version,
			"runtime": runtime.Version(),
		})
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "接口不存在"})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		defer func() {
			// 隔离请求异常，避免单次请求导致服务崩溃。
			if cause := recover(); cause != nil {
				logger.Error("request panic", "error", cause)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "服务内部错误"})
			}
			logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		// 本机写接口要求自定义请求头，阻止其他网站提交跨站请求。
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.URL.Path != "/api/health" && r.Header.Get("X-Requested-With") != "apaas-migration-tool" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "请求来源无效"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
