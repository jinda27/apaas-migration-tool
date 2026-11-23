import test from 'node:test'
import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { createBrowserWorker, isDirectoryRequest } from './browser-worker.mjs'

test('授权模板只来自当前租户和应用的 HTTPS 页面目录', () => {
  const c = { tenant: 'tenant', namespace: 'test__c' }
  const path = '/ae/api/v4/builder/namespaces/test__c/describe/pages/list'
  const origin = 'https://tenant-dev.aedev.feishuapp.cn'
  assert.equal(isDirectoryRequest(new URL(origin + path), c), true)
  for (const url of [
    'http://tenant-dev.aedev.feishuapp.cn' + path,
    'https://other-dev.aedev.feishuapp.cn' + path,
    'https://tenant-dev.example' + path,
    origin + path.replace('test__c', 'other__c'),
    origin + path + '/unexpected',
  ]) {
    assert.equal(isDirectoryRequest(new URL(url), c), false)
  }
})

async function fixture(t) {
  const profileRoot = await mkdtemp(path.join(os.tmpdir(), 'migration-browser-test-'))
  const connection = { id: '0123456789abcdef01234567', tenant: 'tenant', namespace: 'test__c',
    branch: 'develop', devUrl: 'https://tenant-dev.aedev.feishuapp.cn',
    appUrl: 'https://tenant.feishu.cn/ae/app_manage/test__c/overview' }
  const cookie = { name: 'session', value: 'saved-login', domain: 'tenant-dev.aedev.feishuapp.cn',
    path: '/', expires: -1, secure: true, httpOnly: true, sameSite: 'Lax' }
  const f = { profileRoot, connection, initialCookies: [cookie], launches: [], status: 200, error: null }
  const workers = []
  f.browserType = {
    async launchPersistentContext(directory, options) {
      const context = new EventEmitter()
      let cookies = structuredClone(f.initialCookies)
      let currentUrl = 'about:blank'
      let pending
      const page = new EventEmitter()
      page.closed = false
      page.navigations = 0
      page.frontCount = 0
      page.isClosed = () => page.closed
      page.url = () => currentUrl
      page.evaluate = async () => ({ appName: '测试应用', links: [connection.devUrl] })
      page.bringToFront = async () => { page.frontCount++ }
      page.waitForResponse = (predicate) => new Promise((resolve, reject) => { pending = { predicate, resolve, reject } })
      page.goto = async (url) => {
        page.navigations++
        // 模拟真实登录跳转及后台接口失败。
        if (f.error) throw f.error
        if (!cookies.some((entry) => entry.name === 'session')) {
          currentUrl = 'https://accounts.feishu.cn/login/'
          return
        }
        currentUrl = url
        if (!url.includes('/pc_pages?')) return
        const branch = new URL(url).searchParams.get('lane_id')
        const response = {
          url: () => connection.devUrl + '/ae/api/v4/builder/namespaces/test__c/describe/pages/list',
          status: () => f.status,
          ok: () => f.status === 200,
          json: async () => ({ status_code: 0, data: { total: 1 } }),
          request: () => ({ allHeaders: async () => ({ 'x-ae-lane': branch, 'x-kunlun-token': 'memory-only' }) }),
        }
        page.emit('response', response)
        if (pending && await pending.predicate(response)) pending.resolve(response)
      }
      context.cookies = async () => structuredClone(cookies)
      context.addCookies = async (entries) => { cookies.push(...structuredClone(entries)) }
      context.pages = () => [page]
      context.close = async () => {
        page.closed = true
        pending?.reject(new Error('浏览器上下文已关闭'))
        context.emit('close')
      }
      f.launches.push({ directory, options, context, page })
      return context
    },
  }
  f.worker = () => {
    const worker = createBrowserWorker(f.browserType)
    workers.push(worker)
    return worker
  }
  f.request = (action, overrides = {}) => ({ action, profileRoot, connection, ...overrides })
  f.cookieFile = path.join(profileRoot, connection.id, 'authorization-cookies.json')
  t.after(async () => {
    // 先关闭测试会话，再清理私有临时目录。
    for (const worker of workers) await worker.close()
    await rm(profileRoot, { recursive: true, force: true })
  })
  return f
}

test('助手重启后补回会话 Cookie，检查和授权均在后台复用', async (t) => {
  const f = await fixture(t)
  const first = f.worker()
  assert.equal((await first.handle(f.request('check'))).ready, true)
  const saved = await readFile(f.cookieFile, 'utf8')
  assert.equal((await stat(f.cookieFile)).mode & 0o777, 0o600)
  assert.equal(saved.includes('memory-only'), false)
  await first.close()
  f.initialCookies = []
  const restarted = f.worker()
  assert.equal((await restarted.handle(f.request('check'))).ready, true)
  assert.equal((await restarted.handle(f.request('authorize'))).ready, true)
  const auth = await restarted.handle(f.request('authorization'))
  assert.equal(auth.cookies[0].value, 'saved-login')
  assert.deepEqual(f.launches.map(({ options }) => options.headless), [true, true])
  assert.equal(f.launches[1].page.frontCount, 0)
})

test('过期 Cookie 不恢复，检查不弹窗，人工授权才打开浏览器', async (t) => {
  const f = await fixture(t)
  const first = f.worker()
  await first.handle(f.request('check'))
  await first.close()
  const saved = JSON.parse(await readFile(f.cookieFile, 'utf8'))
  saved.cookies[0].expires = Date.now() / 1000 - 60
  await writeFile(f.cookieFile, JSON.stringify(saved))
  f.initialCookies = []
  const restarted = f.worker()
  const result = await restarted.handle(f.request('check'))
  assert.equal(result.ready, false)
  assert.match(result.message, /失效/)
  assert.equal(f.launches.at(-1).options.headless, true)
  await restarted.handle(f.request('authorize'))
  assert.equal(f.launches.at(-1).options.headless, false)
  assert.equal(f.launches.at(-1).page.frontCount, 1)
})

test('已有 Cookie 比快照更新时保留当前登录态', async (t) => {
  const f = await fixture(t)
  const first = f.worker()
  await first.handle(f.request('check'))
  await first.close()
  f.initialCookies[0].value = 'newer-login'
  const auth = await f.worker().handle(f.request('authorization'))
  assert.equal(auth.cookies[0].value, 'newer-login')
})

test('关闭窗口后自动恢复，切换分支重新取得匹配的授权头', async (t) => {
  const f = await fixture(t)
  const worker = f.worker()
  await worker.handle(f.request('check'))
  await f.launches[0].context.close()
  f.initialCookies = []
  assert.equal((await worker.handle(f.request('check'))).ready, true)
  const auth = await worker.handle(f.request('authorization', {
    connection: { ...f.connection, branch: 'release' },
  }))
  assert.equal(auth.headers['x-ae-lane'], 'release')
  assert.equal(f.launches[1].page.navigations, 2)
  assert.deepEqual(f.launches.map(({ options }) => options.headless), [true, true])
})

test('目录返回 401 时不误报授权有效', async (t) => {
  const f = await fixture(t)
  f.status = 401
  const worker = f.worker()
  const result = await worker.handle(f.request('check'))
  assert.equal(result.ready, false)
  assert.match(result.message, /失效/)
  await assert.rejects(readFile(f.cookieFile), { code: 'ENOENT' })
  await worker.handle(f.request('authorize'))
  assert.equal(f.launches.at(-1).options.headless, false)
})

test('网络或权限错误不触发重新授权窗口', async (t) => {
  const f = await fixture(t)
  const worker = f.worker()
  f.error = new Error('网络超时')
  await assert.rejects(worker.handle(f.request('authorize')), /网络超时/)
  f.error = null
  f.status = 403
  await assert.rejects(worker.handle(f.request('authorize')), /HTTP 403/)
  assert.deepEqual(f.launches.map(({ options }) => options.headless), [true])
})
