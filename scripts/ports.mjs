import { spawnSync } from 'node:child_process'
import net from 'node:net'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

function portFree(host, port) {
  return new Promise((resolve, reject) => {
    const probe = net.createServer()
    probe.once('error', (error) => {
      // 只把地址占用作为可清理情况，其它配置错误直接终止。
      if (error.code === 'EADDRINUSE') resolve(false)
      else reject(new Error(`无法检查端口 ${host}:${port}：${error.message}`))
    })
    probe.listen(Number(port), host, () => probe.close(() => resolve(true)))
  })
}

function listeners(port) {
  const result = spawnSync('lsof', ['-nP', `-iTCP:${port}`, '-sTCP:LISTEN', '-t'], {
    encoding: 'utf8', timeout: 5000,
  })
  // 获取监听进程失败时停止启动，不猜测或扩大清理范围。
  if (result.error) throw new Error(`无法检测监听进程，请检查 lsof：${result.error.message}`)
  if (result.status !== 0 && (result.status !== 1 || result.stderr.trim())) {
    throw new Error(`检测端口 ${port} 失败：${result.stderr.trim() || result.status}`)
  }
  return [...new Set(result.stdout.trim().split(/\s+/).map(Number))]
    .filter((pid) => Number.isInteger(pid) && pid > 1 && pid !== process.pid)
}

function stop(pid, signal) {
  try {
    process.kill(pid, signal)
  } catch (error) {
    // 已退出的进程无需再次处理，权限不足时返回明确错误。
    if (error.code !== 'ESRCH') throw new Error(`无法终止 PID ${pid}：${error.message}`)
  }
}

async function waitFree(host, port, timeout) {
  const deadline = Date.now() + timeout
  // 检查端口实际释放后再启动，等待时间保持有界。
  do {
    if (await portFree(host, port)) return true
    await delay(100)
  } while (Date.now() < deadline)
  return false
}

export async function releasePort(host, port, label) {
  const address = `${host}:${port}`
  // 空闲端口直接使用，不影响其它端口的进程。
  if (await portFree(host, port)) {
    console.log(`[端口] ${label} ${address} 空闲`)
    return
  }
  const pids = listeners(port)
  // 另一端口的清理可能已终止共享进程，重新确认后再报错。
  if (!pids.length) {
    if (await portFree(host, port)) return
    throw new Error(`端口 ${address} 被占用，但无法获取监听 PID，请检查进程权限`)
  }
  for (const pid of pids) {
    const result = spawnSync('ps', ['-p', String(pid), '-o', 'comm='], { encoding: 'utf8', timeout: 2000 })
    const name = path.basename(result.stdout?.trim() || '未知进程')
    console.log(`[端口] ${label} ${address} 被 PID ${pid}（${name}）占用，发送 SIGTERM`)
    stop(pid, 'SIGTERM')
  }
  if (await waitFree(host, port, 3000)) {
    console.log(`[端口] ${address} 已释放`)
    return
  }
  const remaining = listeners(port).filter((pid) => pids.includes(pid))
  // 仅强制终止原先仍占用此端口的进程，避免误杀复用的 PID。
  for (const pid of remaining) {
    console.log(`[端口] PID ${pid} 正常停止超时，发送 SIGKILL`)
    stop(pid, 'SIGKILL')
  }
  if (!await waitFree(host, port, 2000)) {
    throw new Error(`端口 ${address} 未释放，当前监听 PID：${listeners(port).join(', ') || '无法读取'}`)
  }
  console.log(`[端口] ${address} 已释放`)
}
