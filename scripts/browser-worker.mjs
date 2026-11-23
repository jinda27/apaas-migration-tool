import { chromium } from 'playwright-core'
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import { createInterface } from 'node:readline'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

export function isDirectoryRequest(url, c) {
  return url.protocol === 'https:' && url.hostname.startsWith(`${c.tenant}-`) &&
    url.hostname.endsWith('.aedev.feishuapp.cn') &&
    url.pathname === `/ae/api/v4/builder/namespaces/${c.namespace}/describe/pages/list`
}

function headersForBranch(headers, c) {
  return headers['x-ae-lane'] === c.branch && Boolean(headers['x-kunlun-token'])
}

class LoginRequiredError extends Error {
  constructor() {
    super('浏览器授权未建立或已失效，请点击“浏览器授权”登录目标应用')
  }
}

function assertAccess(page) {
  // 登录失效与应用无权限分别处理，避免无效的重复授权。
  if (/accounts\.feishu\.cn|\/login\/|\/authen\//.test(page.url())) {
    throw new LoginRequiredError()
  }
  if (/no_tenant_permission/.test(page.url())) {
    throw new Error('当前账号没有目标租户权限')
  }
}

async function directoryHeaders(response, c) {
  // 以真实接口响应确认授权，不以页面地址判断成功。
  if (response.status() === 401) throw new LoginRequiredError()
  if (!response.ok()) throw new Error(`开发后台授权检查失败（HTTP ${response.status()}）`)
  const data = await response.json()
  if (data.status_code != null && String(data.status_code) !== '0') {
    throw new Error('开发后台授权检查返回业务错误')
  }
  const headers = await response.request().allHeaders()
  if (!headersForBranch(headers, c)) throw new Error('未获取当前分支的开发后台授权')
  return headers
}

function cookieKey(cookie) {
  return JSON.stringify([cookie.name, cookie.domain, cookie.path, cookie.partitionKey])
}

async function restoreCookies(context, filename, c) {
  let saved
  try {
    saved = JSON.parse(await readFile(filename, 'utf8'))
  } catch (error) {
    // 缺失或损坏的快照仍可使用 Chrome 自身保存的登录态。
    if (error.code === 'ENOENT' || error instanceof SyntaxError) return
    throw error
  }
  if (saved.tenant !== c.tenant || !Array.isArray(saved.cookies)) return
  const existing = new Set((await context.cookies()).map(cookieKey))
  // 仅补回未过期的缺失 Cookie，保留 Chrome 中较新的登录态。
  const missing = saved.cookies.filter((cookie) =>
    (cookie.expires === -1 || cookie.expires > Date.now() / 1000) && !existing.has(cookieKey(cookie)))
  if (missing.length) await context.addCookies(missing)
}

function saveCookies(current) {
  // 私有快照保留会话 Cookie，授权头仍只驻留内存。
  current.saving = current.saving.catch(() => {}).then(async () => {
    const cookies = await current.context.cookies()
    const temporary = `${current.cookieFile}.tmp`
    await writeFile(temporary, JSON.stringify({ tenant: current.connection.tenant, cookies }), { mode: 0o600 })
    await rename(temporary, current.cookieFile)
  })
  return current.saving
}

async function state(page, c) {
  const data = await page.evaluate(() => ({
    appName: document.querySelector('h3')?.textContent?.trim() ?? '',
    links: [...document.querySelectorAll('a[href]')].map((a) => a.href),
  }))
  const dev = data.links.find((link) => {
    try {
      const u = new URL(link)
      return u.protocol === 'https:' && u.hostname.startsWith(`${c.tenant}-`) && u.hostname.endsWith('.aedev.feishuapp.cn')
    } catch { return false }
  })
  const current = new URL(page.url())
  const devUrl = current.hostname.startsWith(`${c.tenant}-`) && current.hostname.endsWith('.aedev.feishuapp.cn')
    ? current.origin : dev ? new URL(dev).origin : c.devUrl
  return { appName: data.appName, devUrl: devUrl ?? '' }
}

export function createBrowserWorker(browserType = chromium) {
  const sessions = new Map()

  async function session(request, background = true) {
    const c = request.connection
    const existing = sessions.get(c.id)
    // 关闭窗口或切换为人工登录时，释放旧上下文后恢复同一配置目录。
    if (existing) {
      if (!existing.page.isClosed() && (background || !existing.background)) {
        // 连接范围改变后重新取得授权，避免复用旧应用的请求头。
        if (['tenant', 'namespace', 'branch', 'devUrl'].some((key) => existing.connection[key] !== c[key])) {
          existing.headers = null
        }
        existing.connection = c
        return existing
      }
      await existing.saving.catch(() => {})
      await existing.context.close()
    }
    const directory = path.join(request.profileRoot, c.id)
    await mkdir(directory, { recursive: true, mode: 0o700 })
    // 常规检查和导出使用后台 Chrome，仅人工登录显示窗口。
    const context = await browserType.launchPersistentContext(directory, {
      channel: 'chrome', headless: background, viewport: null,
      args: ['--no-first-run', '--no-default-browser-check'],
    })
    const cookieFile = path.join(directory, 'authorization-cookies.json')
    try {
      await restoreCookies(context, cookieFile, c)
    } catch (error) {
      await context.close()
      throw error
    }
    const page = context.pages()[0] ?? await context.newPage()
    const current = { context, page, connection: c, cookieFile, saving: Promise.resolve(),
      headers: null, origin: '', background }
    sessions.set(c.id, current)
    page.on('response', (response) => {
      const url = new URL(response.url())
      const connection = current.connection
      // 仅保存当前应用与分支已验证的登录态，兼容登录后直接关闭窗口。
      if (!isDirectoryRequest(url, connection) ||
          (connection.devUrl && url.origin !== connection.devUrl)) return
      void directoryHeaders(response, connection).then(async (headers) => {
        // 忽略连接切换前尚未结束的响应。
        if (current.connection !== connection || page.isClosed()) return
        current.headers = headers
        current.origin = url.origin
        await saveCookies(current)
      }).catch(() => {})
    })
    context.on('close', () => {
      // 旧上下文延迟关闭不能删除新建立的会话。
      if (sessions.get(c.id) === current) sessions.delete(c.id)
    })
    return current
  }

  async function authorization(request) {
    const c = request.connection
    const current = await session(request)
    const { page, context } = current
    let origin = c.devUrl || (await state(page, c)).devUrl || current.origin
    // 未发现开发域名时，在后台使用已保存登录态读取应用入口。
    if (!origin) {
      await page.goto(c.appUrl, { waitUntil: 'domcontentloaded', timeout: 30000 })
      assertAccess(page)
      origin = (await state(page, c)).devUrl
    }
    if (!origin) throw new Error('无法读取开发环境地址，请填写应用开发地址后检查')
    if (request.payload?.refresh || current.origin !== origin || !current.headers ||
        !headersForBranch(current.headers, c)) {
      const ready = page.waitForResponse((response) => {
        const url = new URL(response.url())
        return url.origin === origin && isDirectoryRequest(url, c)
      }, { timeout: 20000 })
      ready.catch(() => {})
      try {
        // 从登录态重新取得当前分支的授权头，检查失败不自动弹窗。
        await page.goto(`${origin}/ae/page/ns/${c.namespace}/pc_pages?lane_id=${encodeURIComponent(c.branch)}`,
          { waitUntil: 'domcontentloaded', timeout: 30000 })
        assertAccess(page)
        current.headers = await directoryHeaders(await ready, c)
        current.origin = origin
        await saveCookies(current)
      } catch (error) {
        current.headers = null
        throw error
      }
    }
    assertAccess(page)
    const host = new URL(origin).hostname
    const cookies = (await context.cookies()).filter(({ domain }) =>
      domain === host || (domain.startsWith('.') && host.endsWith(domain)))
    const headers = { ...current.headers }
    delete headers.cookie
    return { origin, headers, cookies }
  }

  async function handle(request) {
    const c = request.connection
    // IPC 只接受服务端生成的连接标识。
    if (!/^[a-f0-9]{24}$/.test(c.id)) throw new Error('连接标识无效')
    if (request.action === 'authorize') {
      try {
        await authorization({ ...request, payload: { refresh: true } })
        return { ready: true, message: '浏览器授权有效，已复用保存的登录态' }
      } catch (error) {
        // 只有未登录或明确失效时，人工授权才打开窗口。
        if (!(error instanceof LoginRequiredError)) throw error
      }
      const { page } = await session(request, false)
      await page.goto(c.devUrl ? `${c.devUrl}/ae/page/ns/${c.namespace}/pc_pages?lane_id=${encodeURIComponent(c.branch)}` : c.appUrl,
        { waitUntil: 'domcontentloaded', timeout: 30000 })
      await page.bringToFront()
      return { message: '请在 Chrome 中登录目标应用，完成后返回检查授权状态' }
    }
    if (request.action === 'check') {
      try {
        await authorization({ ...request, payload: { refresh: true } })
        const info = await state(sessions.get(c.id).page, c)
        return { ready: true, message: '浏览器授权有效，已复用保存的登录态', ...info }
      } catch (error) {
        return { ready: false, message: error.message }
      }
    }
    if (request.action === 'authorization') return authorization(request)
    throw new Error('不支持的浏览器操作')
  }

  async function close() {
    // 先保存已验证的会话，再正常关闭专用 Chrome。
    await Promise.all([...sessions.values()].map(async (current) => {
      await current.saving.catch(() => {})
      await current.context.close().catch(() => {})
    }))
  }

  return { handle, close }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const worker = createBrowserWorker()
  const lines = createInterface({ input: process.stdin })
  let queue = Promise.resolve()
  lines.on('line', (line) => {
    // 每个授权接口任务串行执行，响应仅写入 JSON 协议。
    queue = queue.then(async () => {
      let request
      try {
        request = JSON.parse(line)
        const result = await worker.handle(request)
        process.stdout.write(`${JSON.stringify({ id: request.id, result })}\n`)
      } catch (error) {
        process.stdout.write(`${JSON.stringify({ id: request?.id, error: error.message })}\n`)
      }
    })
  })
  const close = async () => { await worker.close(); process.exit(0) }
  lines.on('close', () => void close())
  process.on('SIGINT', () => void close())
  process.on('SIGTERM', () => void close())
}
