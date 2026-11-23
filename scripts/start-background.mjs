import { appendFileSync, closeSync, openSync } from 'node:fs'
import { spawn } from 'node:child_process'
import path from 'node:path'

const root = process.env.PROJECT_ROOT
const logPath = path.join(root, '.runtime', 'startup.log')

// 启动记录先落盘，便于后台进程失败时直接定位。
appendFileSync(logPath, `\n[${new Date().toLocaleString('zh-CN', { hour12: false })}] 开始异步重启\n`)
const log = openSync(logPath, 'a', 0o600)
const child = spawn(process.execPath, [path.join(root, 'scripts', 'dev.mjs')], {
  cwd: root,
  detached: true,
  env: process.env,
  stdio: ['ignore', log, log],
})
child.unref()
closeSync(log)

process.stdout.write(String(child.pid))
