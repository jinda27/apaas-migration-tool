package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Name    string
	Address string
	Root    string
	DataDir string
	Exports string
}

func Load() (Config, error) {
	port := env("BACKEND_PORT", "8081")
	number, err := strconv.Atoi(port)
	// 拒绝无效端口，避免服务以错误配置运行。
	if err != nil || number < 1 || number > 65535 {
		return Config{}, fmt.Errorf("BACKEND_PORT 必须是 1～65535 的整数")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}
	root := env("PROJECT_ROOT", cwd)
	// 从 server 目录启动时定位项目根目录。
	if _, err := os.Stat(filepath.Join(root, "client", "package.json")); err != nil {
		root = filepath.Dir(root)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return Config{}, err
	}
	return Config{
		Name:    env("APP_NAME", "apaas-migration-tool"),
		Address: net.JoinHostPort(env("BACKEND_HOST", "127.0.0.1"), port),
		Root:    root,
		DataDir: env("EXPORT_STATE_DIR", filepath.Join(configDir, "apaas-migration-tool")),
		Exports: filepath.Join(root, "exports"),
	}, nil
}

func env(key, fallback string) string {
	// 空配置使用默认值。
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
