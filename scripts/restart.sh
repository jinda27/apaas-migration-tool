#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(dirname "$(realpath "$0")")"
# 兼容原启动命令，统一使用 script 下的入口。
exec bash "$PROJECT_ROOT/script/restart.sh" "$@"
