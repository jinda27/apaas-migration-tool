package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Runner interface {
	Run(context.Context, string, string, []string, []byte) ([]byte, error)
}

type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, dir, name string, args []string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// 所有参数直接传入进程，不使用 shell 拼接。
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %s", name, redact(message))
	}
	return stdout.Bytes(), nil
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
var bearer = regexp.MustCompile(`(?i)(?:T:|Bearer\s+)[^\s"']+`)
var secrets = regexp.MustCompile(`(?i)("(?:clientSecret|accessToken|refreshToken|appSecret|device_code)"\s*:\s*")[^"]+`)

func redact(value string) string {
	value = ansi.ReplaceAllString(value, "")
	value = bearer.ReplaceAllString(value, "[凭证已隐藏]")
	value = secrets.ReplaceAllString(value, `${1}[已隐藏]`)
	// 日志保留可读错误，不累计完整命令输出。
	if len(value) > 2000 {
		value = value[:2000]
	}
	return value
}

func parseJSON(raw []byte, target any) error {
	clean := strings.TrimSpace(ansi.ReplaceAllString(string(raw), ""))
	start := strings.Index(clean, "{")
	end := strings.LastIndex(clean, "}")
	// 兼容 CLI 输出前后的提示行。
	if start < 0 || end < start {
		return errors.New("命令未返回有效 JSON")
	}
	return json.Unmarshal([]byte(clean[start:end+1]), target)
}

func (s *Service) lark(ctx context.Context, args []string, input any) (map[string]any, error) {
	return s.larkAt(ctx, s.root, args, input)
}

func (s *Service) larkAt(ctx context.Context, dir string, args []string, input any) (map[string]any, error) {
	var body []byte
	// 大批量数据写入私有临时文件，避免 shell 转义与命令行长度限制。
	if input != nil {
		body, _ = json.Marshal(input)
		file, err := os.CreateTemp(s.dataDir, "request-*.json")
		if err != nil {
			return nil, err
		}
		defer os.Remove(file.Name())
		if _, err := file.Write(body); err != nil {
			_ = file.Close()
			return nil, err
		}
		_ = file.Close()
		args = append(args, "--json", "@"+filepath.Base(file.Name()))
		dir = s.dataDir
		body = nil
	}
	args = append(args, "--as", "user")
	// 限流和并发冲突采用有界退避，其他错误直接返回。
	for attempt := 0; attempt < 4; attempt++ {
		raw, err := s.runner.Run(ctx, dir, "lark-cli", args, body)
		var envelope map[string]any
		parseErr := parseJSON(raw, &envelope)
		if err == nil && parseErr == nil && envelope["ok"] == true {
			data, _ := envelope["data"].(map[string]any)
			return data, nil
		}
		// 新版记录导出直接返回 manifest，而非通用 ok/data 包装。
		if err == nil && parseErr == nil && len(args) > 1 && args[1] == "+record-list" &&
			envelope["manifest_version"] != nil && envelope["record_file"] != nil {
			return envelope, nil
		}
		message := "飞书操作失败"
		if err != nil {
			message = err.Error()
		} else if detail, ok := envelope["error"].(map[string]any); ok {
			message = fmt.Sprint(detail["message"])
		} else if parseErr != nil {
			message = parseErr.Error()
		}
		retry := strings.Contains(message, "limited") || strings.Contains(message, "1254291") || strings.Contains(message, "429") || strings.Contains(message, "800004135")
		if !retry || attempt == 3 {
			return nil, errors.New(redact(message))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
	return nil, errors.New("飞书重试次数已用尽")
}
