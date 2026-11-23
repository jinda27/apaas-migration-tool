#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(dirname "$(dirname "$(realpath "$0")")")"
export PROJECT_ROOT
source "$PROJECT_ROOT/scripts/runtime.sh"

# 停止项目同步执行，确保命令返回时端口已经释放。
if [[ "${1:-}" == "--stop" ]]; then
  ensure_node
  exec "$NODE_BIN" "$PROJECT_ROOT/scripts/dev.mjs" --stop
fi

# 仅接受无参数启动，避免后台进程静默忽略无效参数。
if [[ "$#" -gt 0 ]]; then
  printf '用法：./script/restart.sh [--stop]\n' >&2
  exit 1
fi

ensure_runtime
ACCESS_URL="$("$NODE_BIN" "$PROJECT_ROOT/scripts/start-info.mjs")"
RUNTIME_DIR="$PROJECT_ROOT/.runtime"
STARTUP_LOG="$RUNTIME_DIR/startup.log"
mkdir -p "$RUNTIME_DIR"
chmod 700 "$RUNTIME_DIR"
touch "$STARTUP_LOG"
chmod 600 "$STARTUP_LOG"

# 创建独立进程组，终端退出后服务仍持续运行。
STARTUP_PID="$("$NODE_BIN" "$PROJECT_ROOT/scripts/start-background.mjs")"

printf '已提交后台启动，PID：%s\n' "$STARTUP_PID"
printf '访问地址：%s\n' "$ACCESS_URL"
printf '启动日志：%s\n' "$STARTUP_LOG"
printf '查看进度：tail -f "%s"\n' "$STARTUP_LOG"
