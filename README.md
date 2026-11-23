# aPaaS 迁移工具

本机 React 19 + Go 工作台，将 aPaaS 页面、流程、云函数、自定义组件、对象元数据导出到飞书多维表格，并保留原始文件与本地 ZIP。

## 使用

1. 点击左侧「添加实例」，填写应用地址和所需的 Client ID、Client Secret。可填写已有多维表格的地址，或留空让首次导出采集到物料后创建「应用名物料数据」。新地址会保存到实例，后续导出复用，并可从实例标题下打开目标多维表格。
2. 在「实例设置」中检查授权。页面和流程使用 Chrome 登录，云函数和组件使用 ae 授权。对象与按 API 名称采集的流程使用当前环境的 Client ID 和 Client Secret。多维表格使用当前 `lark-cli` 用户身份，可在实例设置里发起飞书授权。
3. 在右侧按类型查看物料，点击「导入文件」或「发起导出」。导出时表格切换为进度，结束后自动返回物料清单。
4. 每次成功导出后，清单按所选类型替换为本次结果。导出失败时可直接继续导出，最近的本地归档可在物料清单工具栏下载。

也可选择“使用导入文件”：分别导入页面/流程/对象的 JSON，或函数/组件源码包后导出。多文件源码推荐整包 ZIP；每个上传文件对应一条物料记录，相同类型、API 名称使用最新导入版本。

单物料源码包中的 `index.meta.json` 会恢复名称、API 名称、工程路径及运行环境；对象 JSON 支持编辑器的嵌套 `object` 定义，原始文件字节保持不变。

| 物料 | 在线采集方式 | 目标数据表 |
| --- | --- | --- |
| 页面 | 接口全量读取桌面/移动目录及完整原始定义，保留原始响应和 DSL 版本 | 页面 |
| 流程 | 全量读取自动化目录，调用已与编辑器下载比对的导出接口；填写 API 名称时使用公开详情接口 | 流程 |
| 云函数 | `ae project clone`，按函数目录打包并携带公共文件 | 云函数 |
| 自定义组件 | 同一工程按组件目录打包并携带公共文件 | 自定义组件 |
| 对象元数据 | Go 使用浏览器授权请求全量目录，或使用填写的 API 名称，再调用公开 API 读取完整元数据 | 对象元数据 |

每张表保存名称、来源、API 名称、环境、幂等键、SHA256 和原始附件。页面和流程复用 Chrome 登录会话，Go 逐项直接请求目录和定义接口、保存原始物料和续传断点。不同表并行处理，同一张表串行上传，每 10 个附件批量回写哈希与摘要；重试保留已创建记录与附件 token，已校验文件直接复用。文件校验失败会停止写入，避免上传内容与哈希不一致。

页面附件保留原始接口响应和 DSL 版本，不执行编辑器的旧 DSL 升级，尚未验证通过编辑器重新导入。流程附件保留导出接口的原始 `data`。

详见[图文使用说明](docs/apaas-user-guide.docx)。

## 启动

```bash
./script/restart.sh
```

启动时直接复用本机的 Node.js 20 和 Go，会检查 PATH、nvm、`~/sdk` 和 Homebrew 安装目录；不会自动安装 Node/Go 或额外热更新工具。首次运行仅安装项目 npm 依赖。

- 前端：http://127.0.0.1:10020
- 后端健康检查：http://127.0.0.1:8081/api/health
- 服务信息：http://127.0.0.1:8081/api/info

脚本提交后台启动后立即返回并输出可点击的访问链接，不自动打开浏览器；启动进度写入 `.runtime/startup.log`。再次运行会清理本项目旧进程并重新启动。启动前按 `.env` 或环境变量检查前后端端口；占用时通过本机 `lsof` 获取监听 PID，先发送 `SIGTERM`，3 秒内未释放则发送 `SIGKILL`。启动日志包含占用端口、进程名、PID、清理结果及服务地址，详细前后端日志保存在 `.runtime/`。

```bash
./script/restart.sh --stop  # 在另一个终端停止项目
./restart.sh               # 兼容原入口
tail -f .runtime/startup.log # 查看异步启动进度
npm run dev         # 同一启动入口
npm run restart     # 重启前后端
npm run stop        # 停止前后端
npm run check       # ESLint、go vet、Go 竞态测试
npm run build       # 构建前后端
```

构建产物为 `client/dist` 和 `server/bin/api`。前端静态文件和 Go API 分别部署，生产环境需将 `/api` 反向代理到 Go 服务。浏览器授权需要在服务端提供 `scripts/browser-worker.mjs`、Node.js、`playwright-core` 和 Chrome。

## 本地配置

```bash
cp .env.example .env
```

修改 `.env` 后运行 `./script/restart.sh`。环境变量优先于 `.env`；前端通过 Vite 代理访问 `/api`，无需额外配置开发环境 CORS。

| 配置 | 默认值 |
| --- | --- |
| `FRONTEND_HOST` | `127.0.0.1` |
| `FRONTEND_PORT` | `10020` |
| `BACKEND_HOST` | `127.0.0.1` |
| `BACKEND_PORT` | `8081` |
| `APP_NAME` | `apaas-migration-tool` |
| `EXPORT_STATE_DIR` | macOS：`~/Library/Application Support/apaas-migration-tool` |

`NODE20_BIN` 和 `GO_BIN` 可指定已有工具链的完整路径。要求 Node.js 20.12+ 和 Go 1.24+，支持 macOS/Linux。在线采集还需本机 Google Chrome、`ae` 和 `lark-cli`；工作台复用它们，不自动安装。

Client ID / Secret 与应用 token 仅在 Go 进程内存中使用，不写入连接文件，服务端热更新或重启后需要重新填写。任务状态和专用 Chrome 会话保存在 `EXPORT_STATE_DIR`；原始物料、工程与归档在项目 `exports/` 中，均不纳入 Git。浏览器使用独立会话目录，不读取日常浏览器配置。已验证的登录 Cookie（含会话 Cookie）以 `0600` 权限保存在该会话目录，重启或关闭窗口后自动补回仍有效的 Cookie；授权头在后台重新获取，不写入任务或日志。检查会实际验证开发后台接口，网络或权限错误不会自动打开授权窗口。

## 目录

```text
client/
  src/api/          # API 请求
  src/App.jsx       # 导出工作台
  vite.config.js    # 热更新与 API 代理
server/
  cmd/api/          # 后端启动入口
  internal/config/  # 服务配置
  internal/httpapi/ # HTTP 路由
  internal/exporter/# 采集、认证、任务状态与 Base 写入
script/restart.sh   # 一键启动/重启/停止
scripts/            # 工具链、端口与进程管理、构建检查
restart.sh          # 兼容原启动入口
.runtime/           # 运行记录与前后端日志（Git 忽略）
exports/            # 原始物料、工程与任务归档（Git 忽略）
```

前端修改 `client/src` 即时更新，后端修改 Go 文件触发重编译。Go 编译失败会保留上一次正常服务，修正代码后保存即可恢复更新。后端日志位于 `.runtime/backend.log`，前端日志位于 `.runtime/frontend.log`。

Git 默认开发分支为 `develop`，远程地址为 `git@github.com:jinda27/apaas-migration-tool.git`。
