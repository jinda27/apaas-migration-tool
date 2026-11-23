import { spawn, spawnSync } from 'node:child_process'
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'
import { ensureDependencies } from './dependencies.mjs'
import { releasePort } from './ports.mjs'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const runtime = path.join(root, '.runtime')
const stateFile = path.join(runtime, 'processes.json')
const lockFile = path.join(runtime, 'startup.lock')
const children = new Set()
let stopping = false
let hasLock = false

mkdirSync(runtime, { recursive: true })

function isRunning(pid) {
  // 检查进程存在性，忽略已经退出的进程。
  if (!Number.isInteger(pid) || pid <= 1) return false
  try {
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}

function terminate(pid, signal = 'SIGTERM') {
  // 启动失败时可能还没有分配 PID。
  if (!Number.isInteger(pid) || pid <= 1) return
  try {
    process.kill(-pid, signal)
  } catch (error) {
    // 已结束的进程组无需重复处理。
    if (error.code !== 'ESRCH') throw error
  }
}

function belongsToProject(pid) {
  const result = spawnSync('ps', ['-p', String(pid), '-o', 'command='], { encoding: 'utf8' })
  return result.status === 0 && result.stdout.includes(root)
}

async function stopPrevious() {
  // 没有历史记录时直接开始启动。
  if (!existsSync(stateFile)) return
  const previous = JSON.parse(readFileSync(stateFile, 'utf8'))
  if (previous.root !== root || !Array.isArray(previous.groups)) throw new Error('进程记录与当前项目不匹配')
  const groups = previous.groups.filter((pid) => belongsToProject(pid))

  // 先终止旧管理进程，再清理其前后端进程组。
  console.log('[启动] 正在停止旧项目进程…')
  if (isRunning(previous.supervisor) && belongsToProject(previous.supervisor)) process.kill(previous.supervisor, 'SIGTERM')
  for (const pid of groups) terminate(pid)
  const deadline = Date.now() + 5000

  // 给 Go 服务和开发服务器时间正常退出。
  while ((groups.some(isRunning) || (isRunning(previous.supervisor) && belongsToProject(previous.supervisor))) && Date.now() < deadline) await delay(100)
  for (const pid of groups) terminate(pid, 'SIGKILL')
  rmSync(stateFile, { force: true })
}

function loadEnv() {
  const filename = path.join(root, '.env')
  // 没有本地配置时使用默认值。
  if (existsSync(filename)) process.loadEnvFile(filename)

  // 对端口进行校验，避免启动时传入无效值。
  for (const [key, fallback] of [['FRONTEND_PORT', '10020'], ['BACKEND_PORT', '8081']]) {
    const value = process.env[key] || fallback
    if (!/^\d+$/.test(value) || Number(value) < 1 || Number(value) > 65535) {
      throw new Error(`${key} 必须是 1～65535 的整数`)
    }
    process.env[key] = value
  }
  process.env.FRONTEND_HOST ||= '127.0.0.1'
  process.env.BACKEND_HOST ||= '127.0.0.1'
  process.env.APP_NAME ||= 'apaas-migration-tool'
  process.env.PROJECT_ROOT = root
  if (process.env.FRONTEND_PORT === process.env.BACKEND_PORT) throw new Error('前后端端口不能相同')
}

function run(command, args, cwd = root, extraEnv = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd,
      stdio: 'inherit',
      detached: true,
      env: { ...process.env, ...extraEnv },
    })
    children.add(child)
    child.once('error', reject)
    child.once('exit', (code, signal) => {
      children.delete(child)
      // 安装或检查失败时阻止启动。
      if (code !== 0) reject(new Error(`${command} 执行失败（${signal || code}）`))
      else resolve()
    })
  })
}

async function prepare() {
  await ensureDependencies(root, (args) => run('npm', args))
}

function start(name, command, args, cwd) {
  const log = openSync(path.join(runtime, `${name}.log`), 'w')
  const child = spawn(command, args, {
    cwd,
    detached: true,
    stdio: ['ignore', 'pipe', 'pipe'],
    env: process.env,
  })
  children.add(child)

  // 将输出同时写入终端与独立日志文件。
  for (const [stream, output] of [[child.stdout, process.stdout], [child.stderr, process.stderr]]) {
    stream.on('data', (chunk) => {
      output.write(`[${name}] ${chunk}`)
      writeFileSync(log, chunk)
    })
  }
  child.once('error', (error) => {
    console.error(`[${name}] ${error.message}`)
    void shutdown(1)
  })
  child.once('exit', (code) => {
    children.delete(child)
    closeSync(log)
    // 任一服务意外退出时清理另一个服务。
    if (!stopping) {
      console.error(`[${name}] 服务已退出（${code}）`)
      void shutdown(1)
    }
  })
  return child.pid
}

async function waitReady(url) {
  const deadline = Date.now() + 60000
  // 同时等待前后端响应，避免提前提示启动成功。
  while (Date.now() < deadline && !stopping) {
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(1000) })
      if (response.ok) return
    } catch {
      // 编译期间连接尚未建立，继续等待。
    }
    await delay(200)
  }
  throw new Error(`服务启动超时：${url}，请查看 .runtime 日志`)
}

async function shutdown(code = 0) {
  // 防止信号和进程退出触发重复清理。
  if (stopping) return
  stopping = true
  for (const child of children) terminate(child.pid)
  const deadline = Date.now() + 5000
  while (children.size > 0 && Date.now() < deadline) await delay(100)
  for (const child of children) terminate(child.pid, 'SIGKILL')
  // 仅删除自己写入的状态，避免影响另一个启动命令。
  if (existsSync(stateFile) && JSON.parse(readFileSync(stateFile, 'utf8')).supervisor === process.pid) {
    rmSync(stateFile, { force: true })
  }
  if (hasLock && existsSync(lockFile) && Number(readFileSync(lockFile, 'utf8')) === process.pid) {
    rmSync(lockFile, { force: true })
  }
  process.exit(code)
}

// Ctrl+C 和脚本重启都会停止整个进程组。
process.on('SIGINT', () => void shutdown())
process.on('SIGTERM', () => void shutdown())

try {
  const action = process.argv[2]
  if (action && action !== '--stop') throw new Error('用法：./script/restart.sh [--stop]')
  // 停止模式不安装依赖或启动服务。
  if (action === '--stop') {
    // 停止依赖安装或编译中的启动进程。
    if (existsSync(lockFile)) {
      const pid = Number(readFileSync(lockFile, 'utf8'))
      if (isRunning(pid) && belongsToProject(pid)) {
        process.kill(pid, 'SIGTERM')
        const deadline = Date.now() + 5000
        while (isRunning(pid) && Date.now() < deadline) await delay(100)
      }
    }
    await stopPrevious()
    console.log('项目已停止')
    process.exit(0)
  }

  // 在终止已有服务之前验证配置，错误端口不会触发进程清理。
  loadEnv()
  // 排除并发启动，自动清理已退出进程遗留的锁。
  if (existsSync(lockFile)) {
    const pid = Number(readFileSync(lockFile, 'utf8'))
    if (isRunning(pid)) throw new Error('项目正在启动，请稍后重试')
    rmSync(lockFile, { force: true })
  }
  const lock = openSync(lockFile, 'wx')
  writeFileSync(lock, String(process.pid))
  closeSync(lock)
  hasLock = true
  console.log(`[启动] 项目：${root}\n[启动] Node：${process.version}\n[启动] 正在检查依赖…`)
  await prepare()
  await stopPrevious()
  await Promise.all([
    releasePort(process.env.FRONTEND_HOST, process.env.FRONTEND_PORT, '前端'),
    releasePort(process.env.BACKEND_HOST, process.env.BACKEND_PORT, '后端'),
  ])
  console.log('[启动] 正在启动前后端服务…')
  const backend = start('backend', process.execPath, [path.join(root, 'scripts/go-dev.mjs')], path.join(root, 'server'))
  const frontend = start('frontend', process.execPath, [path.join(root, 'node_modules/vite/bin/vite.js')], path.join(root, 'client'))
  writeFileSync(stateFile, JSON.stringify({ root, supervisor: process.pid, groups: [backend, frontend] }))
  rmSync(lockFile, { force: true })
  hasLock = false
  const frontendUrl = `http://${process.env.FRONTEND_HOST}:${process.env.FRONTEND_PORT}`
  const backendUrl = `http://${process.env.BACKEND_HOST}:${process.env.BACKEND_PORT}`
  await Promise.all([waitReady(frontendUrl), waitReady(`${backendUrl}/api/health`)])
  console.log(`\n启动完成\n前端：${frontendUrl}\n后端：${backendUrl}/api/health\n日志：${runtime}\n停止：./script/restart.sh --stop\n修改代码自动更新。\n`)
} catch (error) {
  // 主动停止时不显示依赖安装被中断的错误。
  if (!stopping) console.error(error.message)
  await shutdown(1)
}
