package exporter

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNodeBrowserRestartsDisconnectedWorker(t *testing.T) {
	node := os.Getenv("NODE_BIN")
	// 测试只复用本机 Node，不安装额外运行时。
	if node == "" {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			t.Skip("本机没有 Node")
		}
	}
	t.Setenv("NODE_BIN", node)
	// 覆盖单次恢复成功与连续失败的有界重试。
	for _, persistentFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "恢复成功", true: "连续失败"}[persistentFailure], func(t *testing.T) {
			root := t.TempDir()
			scripts := filepath.Join(root, "scripts")
			if err := os.Mkdir(scripts, 0700); err != nil {
				t.Fatal(err)
			}
			source := `import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { createInterface } from 'node:readline'
import path from 'node:path'
const marker = path.join(path.dirname(process.argv[1]), 'attempts')
const attempts = existsSync(marker) ? Number(readFileSync(marker, 'utf8')) + 1 : 1
writeFileSync(marker, String(attempts))
createInterface({ input: process.stdin }).on('line', (line) => {
  // 首次退出模拟助手断连，后续响应保持正常协议。
  if (attempts === 1 || PERSISTENT_FAILURE) process.exit(1)
  const request = JSON.parse(line)
  process.stdout.write(JSON.stringify({ id: request.id, result: { ready: true } }) + '\n')
})
`
			failureValue := "false"
			if persistentFailure {
				failureValue = "true"
			}
			source = "const PERSISTENT_FAILURE = " + failureValue + "\n" + source
			if err := os.WriteFile(filepath.Join(scripts, "browser-worker.mjs"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			browser := &NodeBrowser{root: root, data: root}
			defer browser.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			raw, err := browser.Call(ctx, "check", Connection{ID: newID()}, nil)
			if persistentFailure {
				if err == nil {
					t.Fatal("连续退出的助手不能返回成功")
				}
			} else {
				var result Check
				if err != nil || json.Unmarshal(raw, &result) != nil || !result.Ready {
					t.Fatalf("未自动恢复助手：%s，%v", raw, err)
				}
			}
			attempts, err := os.ReadFile(filepath.Join(scripts, "attempts"))
			if err != nil || string(attempts) != "2" {
				t.Fatalf("助手启动次数应为 2，实际 %s，%v", attempts, err)
			}
		})
	}
}
