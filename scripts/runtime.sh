#!/usr/bin/env bash

NODE_VERSION="$(tr -d '[:space:]' < "$PROJECT_ROOT/.nvmrc")"
GO_VERSION="$(tr -d '[:space:]' < "$PROJECT_ROOT/.go-version")"

ensure_node() {
  local candidate version found=0

  # 从 PATH、nvm 和 Homebrew 中查找现有 Node 20。
  for candidate in "${NODE20_BIN:-}" "${NVM_DIR:-$HOME/.nvm}/versions/node/v$NODE_VERSION/bin/node" "$(command -v node || true)" /opt/homebrew/opt/node@20/bin/node /usr/local/opt/node@20/bin/node "${NVM_DIR:-$HOME/.nvm}"/versions/node/v20.*/bin/node; do
    [[ -n "$candidate" && -x "$candidate" ]] || continue
    version="$("$candidate" --version)"
    # 启动管理需要 Node 20.12 及以上的环境文件支持。
    if [[ "$version" =~ ^v20\.([0-9]+) && "${BASH_REMATCH[1]}" -ge 12 ]]; then
      NODE_BIN="$candidate"
      found=1
      break
    fi
  done

  # 只提示已有环境的配置方式，不自动安装 SDK。
  if [[ "$found" -eq 0 ]]; then
    printf '未找到 Node.js 20.12+，请设置 NODE20_BIN 为已有 Node 20 的完整路径。\n' >&2
    return 1
  fi
  export NODE_BIN
  export PATH="$(dirname "$NODE_BIN"):$PATH"
}

ensure_runtime() {
  local candidate version found=0
  ensure_node

  # 从 PATH、Go SDK 和 Homebrew 中复用现有 Go。
  for candidate in "${GO_BIN:-}" "$HOME/sdk/go$GO_VERSION/bin/go" "$(command -v go || true)" "${GOROOT:-/usr/local/go}/bin/go" /opt/homebrew/bin/go /usr/local/bin/go "$HOME"/sdk/go*/bin/go; do
    [[ -n "$candidate" && -x "$candidate" ]] || continue
    version="$("$candidate" version)"
    if [[ "$version" =~ go1\.([0-9]+) && "${BASH_REMATCH[1]}" -ge 24 ]]; then
      GO_BIN="$candidate"
      found=1
      break
    fi
  done

  # 未找到 Go 时要求配置路径，避免重复安装。
  if [[ "$found" -eq 0 ]]; then
    printf '未找到 Go 1.24+，请设置 GO_BIN 为已有 Go 的完整路径。\n' >&2
    return 1
  fi
  export GO_BIN
  export PATH="$(dirname "$GO_BIN"):$PATH"
}
