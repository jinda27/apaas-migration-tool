#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(dirname "$(dirname "$(realpath "$0")")")"
export PROJECT_ROOT
source "$PROJECT_ROOT/scripts/runtime.sh"
ensure_runtime

"$NODE_BIN" "$PROJECT_ROOT/scripts/tasks.mjs" "${1:-check}"
