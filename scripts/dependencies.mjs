import { createHash } from 'node:crypto'
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

export async function ensureDependencies(root, install) {
  const runtime = path.join(root, '.runtime')
  mkdirSync(runtime, { recursive: true })
  const lock = path.join(runtime, 'dependencies.lock')
  const marker = path.join(root, 'node_modules/.scaffold-dependencies')
  const fingerprint = () => {
    const hash = createHash('sha256')
    for (const file of ['package.json', 'client/package.json', 'package-lock.json']) {
      const filename = path.join(root, file)
      if (existsSync(filename)) hash.update(readFileSync(filename))
    }
    return hash.digest('hex')
  }
  const ready = () => existsSync(marker) && readFileSync(marker, 'utf8') === fingerprint() &&
    ['vite', 'eslint', 'playwright-core'].every((name) => existsSync(path.join(root, 'node_modules', name, 'package.json')))
  const deadline = Date.now() + 120000
  // 启动、检查与构建共用安装锁，避免并发清理 node_modules。
  for (;;) {
    if (ready()) return
    try {
      const file = openSync(lock, 'wx')
      writeFileSync(file, String(process.pid))
      closeSync(file)
      break
    } catch (error) {
      if (error.code !== 'EEXIST') throw error
      const pid = Number(readFileSync(lock, 'utf8'))
      try {
        process.kill(pid, 0)
      } catch (error) {
        // 清理安装进程退出后的遗留锁。
        if (error.code === 'ESRCH') { rmSync(lock, { force: true }); continue }
        throw error
      }
      if (Date.now() >= deadline) throw new Error('依赖正在安装，请在完成后重试')
      await delay(100)
    }
  }
  try {
    if (!ready()) {
      await install(['ci', '--no-audit', '--no-fund'])
      writeFileSync(marker, fingerprint())
    }
  } finally {
    rmSync(lock, { force: true })
  }
}
