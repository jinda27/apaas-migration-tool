import { spawn } from 'node:child_process'
import { mkdirSync, renameSync, rmSync, watch } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const cwd = path.join(root, 'server')
const binary = path.join(root, '.runtime/backend-api')
const staging = `${binary}.next`
let running
let compiler
let timer
let queued = true
let building = false
let stopping = false

mkdirSync(path.dirname(binary), { recursive: true })

async function stopServer() {
  const previous = running
  running = null
  // 服务尚未启动或已经退出时无需等待。
  if (!previous || previous.exitCode !== null || previous.signalCode !== null) return
  const exited = new Promise((resolve) => previous.once('exit', resolve))
  previous.kill('SIGINT')
  const timeout = setTimeout(() => previous.kill('SIGKILL'), 5500)
  await exited
  clearTimeout(timeout)
}

function build() {
  return new Promise((resolve) => {
    compiler = spawn(process.env.GO_BIN || 'go', ['build', '-o', staging, './cmd/api'], { cwd, stdio: 'inherit' })
    compiler.once('error', (error) => {
      console.error(error.message)
      compiler = null
      resolve(false)
    })
    compiler.once('exit', (code) => {
      compiler = null
      resolve(code === 0)
    })
  })
}

async function rebuild() {
  // 连续修改合并处理，避免并发编译覆盖产物。
  if (building || stopping) return
  building = true
  try {
    // 编译过程中再次修改时追加一次构建。
    while (queued && !stopping) {
      queued = false
      console.log('正在编译 Go 服务…')
      const success = await build()
      if (stopping) break
      // 编译失败保留上一次正常服务，等待下一次保存。
      if (!success) {
        console.error('编译失败，请修正后保存；详细错误见 .runtime/backend.log')
        continue
      }
      await stopServer()
      if (stopping) break
      renameSync(staging, binary)
      const child = spawn(binary, [], { cwd, stdio: 'inherit' })
      running = child
      child.once('error', (error) => {
        console.error(error.message)
        void shutdown(1)
      })
      child.once('exit', (code) => {
        // 重编译替换进程时忽略旧服务的正常退出。
        if (!stopping && running === child) {
          console.error(`Go 服务退出（${code}）`)
          void shutdown(1)
        }
      })
    }
  } catch (error) {
    console.error(error.message)
    await shutdown(1)
  } finally {
    building = false
  }
}

const watcher = watch(cwd, { recursive: true }, (_event, filename) => {
  // 只关注源码和依赖清单，忽略构建产物与测试文件。
  if (!filename || /^(bin|tmp|vendor)([/\\]|$)/.test(filename) || filename.endsWith('_test.go')) return
  if (!filename.endsWith('.go') && filename !== 'go.mod' && filename !== 'go.sum') return
  queued = true
  clearTimeout(timer)
  timer = setTimeout(() => void rebuild(), 250)
})

async function shutdown(code = 0) {
  // 信号触发后关闭监听并停止所有子进程。
  if (stopping) return
  stopping = true
  clearTimeout(timer)
  watcher.close()
  compiler?.kill('SIGTERM')
  await stopServer()
  rmSync(staging, { force: true })
  process.exit(code)
}

watcher.once('error', (error) => {
  console.error(error.message)
  void shutdown(1)
})
process.on('SIGINT', () => void shutdown())
process.on('SIGTERM', () => void shutdown())
void rebuild()
