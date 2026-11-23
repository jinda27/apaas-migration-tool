package exporter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type BrowserClient interface {
	Call(context.Context, string, Connection, any) (json.RawMessage, error)
	Close()
}

var errBrowserDisconnected = errors.New("浏览器助手连接已断开")

type NodeBrowser struct {
	mu     sync.Mutex
	root   string
	data   string
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
}

func (b *NodeBrowser) Call(ctx context.Context, action string, c Connection, payload any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 助手退出后自动重启一次，恢复已保存的浏览器登录态。
	for attempt := 0; attempt < 2; attempt++ {
		result, err := b.call(ctx, action, c, payload)
		if !errors.Is(err, errBrowserDisconnected) {
			return result, err
		}
	}
	return nil, errors.New("浏览器助手暂不可用，请重试")
}

func (b *NodeBrowser) call(ctx context.Context, action string, c Connection, payload any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 浏览器请求排队，避免并发改变同一个编辑器页面。
	if b.cmd == nil {
		node := os.Getenv("NODE_BIN")
		if node == "" {
			node = "node"
		}
		b.cmd = exec.Command(node, filepath.Join(b.root, "scripts", "browser-worker.mjs"))
		b.cmd.Dir = b.root
		b.cmd.Stderr = io.Discard
		var err error
		b.input, err = b.cmd.StdinPipe()
		if err != nil {
			b.cmd = nil
			return nil, err
		}
		output, err := b.cmd.StdoutPipe()
		if err != nil {
			b.cmd = nil
			return nil, err
		}
		if err := b.cmd.Start(); err != nil {
			b.cmd = nil
			return nil, errors.New("无法启动浏览器助手，请检查 Node 20 和 playwright-core")
		}
		b.output = bufio.NewScanner(output)
		b.output.Buffer(make([]byte, 64<<10), 20<<20)
	}
	request := map[string]any{"id": newID(), "action": action, "connection": c, "payload": payload, "profileRoot": filepath.Join(b.data, "browser")}
	// 仅发送公开连接信息，API 密钥不会交给浏览器助手。
	if err := json.NewEncoder(b.input).Encode(request); err != nil {
		b.stop()
		return nil, errBrowserDisconnected
	}
	type reply struct {
		Result       json.RawMessage `json:"result"`
		Error        string          `json:"error"`
		Disconnected bool            `json:"-"`
	}
	done := make(chan reply, 1)
	scanner := b.output
	go func() {
		// 每条 RPC 对应一条标准输出 JSON。
		if !scanner.Scan() {
			done <- reply{Disconnected: true}
			return
		}
		var response reply
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			response.Error = "浏览器助手返回无效响应"
		}
		done <- response
	}()
	select {
	case <-ctx.Done():
		b.stop()
		return nil, ctx.Err()
	case response := <-done:
		// 清理失效管道，下一次尝试启动新的助手。
		if response.Disconnected {
			b.stop()
			return nil, errBrowserDisconnected
		}
		if response.Error != "" {
			return nil, fmt.Errorf("浏览器：%s", redact(response.Error))
		}
		return response.Result, nil
	}
}

func (b *NodeBrowser) stop() {
	// 终止助手后清理进程资源，下一次操作可重新启动。
	if b.cmd != nil {
		_ = b.input.Close()
		finished := make(chan struct{})
		go func() { _ = b.cmd.Wait(); close(finished) }()
		// 留出时间让助手关闭自身 Chrome，超时再终止进程。
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			_ = b.cmd.Process.Kill()
			<-finished
		}
		b.cmd, b.input, b.output = nil, nil, nil
	}
}

func (b *NodeBrowser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stop()
}
