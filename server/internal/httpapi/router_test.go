package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIContract(t *testing.T) {
	handler := NewHandler("test-app", slog.New(slog.NewTextHandler(io.Discard, nil)))
	cases := []struct {
		method string
		path   string
		status int
		key    string
		value  string
	}{
		{http.MethodGet, "/api/health", http.StatusOK, "status", "ok"},
		{http.MethodGet, "/api/info", http.StatusOK, "name", "test-app"},
		{http.MethodGet, "/api/missing", http.StatusNotFound, "error", "接口不存在"},
	}

	// 验证前端依赖的状态码、JSON 字段和响应头。
	for _, item := range cases {
		t.Run(item.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(item.method, item.path, nil))
			if response.Code != item.status {
				t.Fatalf("status = %d, want %d", response.Code, item.status)
			}
			if response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("missing JSON content type")
			}
			var payload map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload[item.key] != item.value {
				t.Fatalf("payload = %v", payload)
			}
		})
	}
}

func TestHealthRejectsPost(t *testing.T) {
	handler := NewHandler("test-app", slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/health", nil))
	// 健康检查接口只允许查询。
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}
