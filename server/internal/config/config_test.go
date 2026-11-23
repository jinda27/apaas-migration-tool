package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("BACKEND_HOST", "")
	t.Setenv("BACKEND_PORT", "")
	t.Setenv("APP_NAME", "")
	cfg, err := Load()
	// 默认配置应可直接启动本地服务。
	if err != nil || cfg.Address != "127.0.0.1:8081" || cfg.Name != "apaas-migration-tool" {
		t.Fatalf("unexpected config: %+v, error: %v", cfg, err)
	}
}

func TestLoadCustomConfig(t *testing.T) {
	t.Setenv("BACKEND_HOST", "::1")
	t.Setenv("BACKEND_PORT", "9000")
	t.Setenv("APP_NAME", "custom-app")
	cfg, err := Load()
	// IPv6 地址必须正确拼接，配置名称应保持原值。
	if err != nil || cfg.Address != "[::1]:9000" || cfg.Name != "custom-app" {
		t.Fatalf("unexpected config: %+v, error: %v", cfg, err)
	}
}

func TestLoadRejectsInvalidPort(t *testing.T) {
	// 覆盖格式错误和上下边界。
	for _, port := range []string{"abc", "-1", "0", "65536", "1.5"} {
		t.Run(port, func(t *testing.T) {
			t.Setenv("BACKEND_PORT", port)
			if _, err := Load(); err == nil {
				t.Fatalf("expected invalid port error for %q", port)
			}
		})
	}
}
