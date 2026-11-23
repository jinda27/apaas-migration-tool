import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import net from 'node:net'
import test from 'node:test'
import { releasePort } from './ports.mjs'

async function listener(t, mode = 'normal') {
  const source = `
    import net from 'node:net'
    const server = net.createServer()
    // 模拟正常停止和拒绝正常停止的真实监听进程。
    process.on('SIGTERM', () => {
      if (process.argv[1] !== 'ignore') server.close(() => process.exit(0))
    })
    server.listen(0, '127.0.0.1', () => process.send(server.address().port))
  `
  const child = spawn(process.execPath, ['--input-type=module', '-e', source, mode], {
    stdio: ['ignore', 'ignore', 'ignore', 'ipc'],
  })
  const exited = new Promise((resolve) => child.once('exit', (code, signal) => resolve({ code, signal })))
  t.after(async () => {
    // 无论测试是否成功都回收专用监听进程。
    if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL')
    await exited
  })
  const [port] = await once(child, 'message')
  return { child, port, exited }
}

async function freePort(port = 0) {
  const server = net.createServer()
  server.listen(port, '127.0.0.1')
  await once(server, 'listening')
  const address = server.address()
  await new Promise((resolve) => server.close(resolve))
  return address.port
}

test('空闲端口和无效端口不终止其它监听进程', { timeout: 10000 }, async (t) => {
  const other = await listener(t)
  const port = await freePort()
  await releasePort('127.0.0.1', port, '测试')
  await assert.rejects(releasePort('127.0.0.1', -1, '测试'))
  assert.equal(other.child.exitCode, null)
  assert.equal(other.child.signalCode, null)
})

test('占用端口的进程正常停止后端口可重新绑定', { timeout: 10000 }, async (t) => {
  const current = await listener(t)
  const other = await listener(t)
  await releasePort('127.0.0.1', current.port, '测试')
  assert.deepEqual(await current.exited, { code: 0, signal: null })
  assert.equal(await freePort(current.port), current.port)
  assert.equal(other.child.exitCode, null)
  assert.equal(other.child.signalCode, null)
})

test('拒绝 SIGTERM 的占用进程超时后强制结束', { timeout: 10000 }, async (t) => {
  const current = await listener(t, 'ignore')
  await releasePort('127.0.0.1', current.port, '测试')
  assert.equal((await current.exited).signal, 'SIGKILL')
  assert.equal(await freePort(current.port), current.port)
})
