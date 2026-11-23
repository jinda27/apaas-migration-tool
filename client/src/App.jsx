import { useCallback, useEffect, useRef, useState } from 'react'
import { post, request } from './api/client.js'

const kinds = [
  { id: 'pages', name: '页面', detail: '页面布局、组件与交互', format: 'JSON', accept: '.json' },
  { id: 'flows', name: '流程', detail: '触发器、条件与执行步骤', format: 'JSON', accept: '.json' },
  { id: 'functions', name: '云函数', detail: '函数源码与公共依赖', format: '源码 / ZIP', accept: '.zip,.js,.ts,.json,.yaml,.yml' },
  { id: 'components', name: '自定义组件', detail: '组件源码、样式与资源', format: '源码 / ZIP', accept: '.zip,.js,.jsx,.ts,.tsx,.css,.json' },
  { id: 'objects', name: '对象元数据', detail: '对象设置与完整字段定义', format: 'JSON', accept: '.json' },
]
const statuses = { queued: '等待执行', collecting: '收集物料', writing: '写入多维表格', completed: '已完成', partial: '部分完成', failed: '失败', cancelled: '已取消', interrupted: '待继续' }
const activeStatuses = new Set(['queued', 'collecting', 'writing'])
const progressStatuses = { waiting: '等待中', running: '处理中', completed: '采集完成', partial: '部分完成', failed: '失败', skipped: '未选择', cancelled: '已停止' }
const progressLanes = [
  { key: 'pages', name: '页面', kinds: ['pages'] },
  { key: 'flows', name: '流程', kinds: ['flows'] },
  { key: 'source', name: '源码', kinds: ['functions', 'components'] },
  { key: 'objects', name: '对象元数据', kinds: ['objects'] },
]
const initial = {
  appUrl: '', devUrl: '', appName: '', branch: 'develop', baseUrl: '',
  objectIds: '', flowIds: '', clientId: '', clientSecret: '', clientSecretConfigured: false,
}
const sources = { 'api-definition': '接口原始定义', api: '接口采集', ae: '工程源码', browser: '浏览器采集', 'browser-definition': '浏览器原始定义', imported: '导入文件' }

function toForm(c) {
  return { ...c, objectIds: (c.objectIds || []).join('\n'), flowIds: (c.flowIds || []).join('\n'), clientId: c.clientId || '', clientSecret: '' }
}

function Icon({ name, size = 18 }) {
  const paths = {
    arrow: 'M5 12h14m-6-6 6 6-6 6', check: 'm5 12 4 4L19 6', refresh: 'M20 7v5h-5M4 17v-5h5M6 7a7 7 0 0 1 12-1l2 3M4 15l2 3a7 7 0 0 0 12-1',
    folder: 'M3 7h6l2 2h10v10H3zM3 7V4h7l2 3', pages: 'M4 4h16v16H4zM4 9h16M9 9v11',
    flows: 'M9 3h6v6H9zM3 15h6v6H3zM15 15h6v6h-6zM12 9v3M6 15v-3h12v3',
    functions: 'm8 6-6 6 6 6m8-12 6 6-6 6m-3-15-2 18', components: 'm12 2 9 5v10l-9 5-9-5V7zm-9 5 9 5 9-5m-9 5v10',
    objects: 'M4 6c0-4 16-4 16 0s-16 4-16 0m0 0v12c0 4 16 4 16 0V6M4 12c0 4 16 4 16 0',
    upload: 'M12 16V3m-5 5 5-5 5 5M4 15v6h16v-6', download: 'M12 3v13m-5-5 5 5 5-5M4 16v5h16v-5',
    link: 'm10 14 4-4M8 16l-2 2a4 4 0 0 1-6-6l4-4a4 4 0 0 1 6 0m4-2 2-2a4 4 0 0 1 6 6l-4 4a4 4 0 0 1-6 0',
    plus: 'M12 5v14M5 12h14', close: 'm6 6 12 12M18 6 6 18', search: 'M16 16l5 5M18 11a7 7 0 1 1-14 0 7 7 0 0 1 14 0Z',
    edit: 'M12 20h9M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z',
    instance: 'M12 3 3 8l9 5 9-5-9-5ZM3 12l9 5 9-5M3 16l9 5 9-5',
  }
  return <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={paths[name] || paths.folder} /></svg>
}

function Field({ label, hint, children }) {
  return <label className="field"><span>{label}{hint ? <small>{hint}</small> : null}</span>{children}</label>
}

function Badge({ status }) {
  return <span className={`badge ${status}`}><i />{statuses[status] || status}</span>
}

function ProgressRow({ name, icon, current, total, status }) {
  const knownTotal = total > 0
  const value = knownTotal ? Math.min(current, total) : undefined
  const percent = knownTotal ? Math.round(value / total * 100) : 0
  return <div className={`progress-row ${status}`}>
    <div className="progress-meta">
      <span className="progress-name"><Icon name={icon} size={16} />{name}</span>
      <span className="progress-state">{progressStatuses[status] || status}</span>
      <span className="progress-count">{knownTotal ? `${value} / ${total}` : '—'}</span>
      <strong>{knownTotal ? `${percent}%` : '—'}</strong>
    </div>
    <div className={`progress-track ${knownTotal ? '' : 'unknown'}`} role="progressbar" aria-label={`${name}进度`} aria-valuemin={0} aria-valuemax={knownTotal ? total : undefined} aria-valuenow={value}>
      <span style={{ width: `${percent}%` }} />
    </div>
  </div>
}

function ExportProgress({ job, busy, onStop }) {
  const progress = job.progress?.length ? job.progress.filter((item) => item.status !== 'skipped') : progressLanes
    .filter((lane) => lane.kinds.some((kind) => job.kinds?.includes(kind)))
    .map((lane) => ({ ...lane, current: 0, total: 0, status: 'waiting' }))
  const uploaded = job.items?.filter((item) => item.status === 'uploaded').length || 0
  return <section className="export-progress" aria-labelledby="export-progress-title">
    <header className="export-progress-heading">
      <div><h2 id="export-progress-title">导出进度</h2><span role="status"><Badge status={job.status} /></span></div>
      {job.id ? <button className="button" type="button" disabled={Boolean(busy)} onClick={() => onStop(job.id)}>{busy === 'cancel' ? '正在停止…' : '停止导出'}</button> : null}
    </header>
    <div className="progress-body">
      {progress.map((item) => <ProgressRow key={item.key} name={item.name} icon={item.key === 'source' ? 'functions' : item.key} current={item.current} total={item.total} status={item.status} />)}
      {job.status === 'writing' ? <ProgressRow name="写入多维表格" icon="upload" current={uploaded} total={job.items?.length || 0} status="running" /> : null}
    </div>
  </section>
}

function time(value) {
  return new Date(value).toLocaleString('zh-CN', { hour12: false, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function size(value) {
  return value < 1024 ? `${value} B` : value < 1024 * 1024 ? `${(value / 1024).toFixed(1)} KB` : `${(value / 1024 / 1024).toFixed(1)} MB`
}

function Notice({ notice, onDismiss }) {
  if (!notice) return null
  return <div className={`notice ${notice.error ? 'error' : ''}`} role={notice.error ? 'alert' : 'status'}>
    <span>{notice.message}</span>
    <button type="button" aria-label="关闭提示" onClick={onDismiss}><Icon name="close" size={16} /></button>
  </div>
}

function Modal({ title, busy, onClose, notice, onDismiss, variant = 'modal', children }) {
  const ref = useRef(null)
  useEffect(() => {
    const dialog = ref.current
    dialog.showModal()
    return () => dialog.close()
  }, [])
  return <dialog ref={ref} className={`modal ${variant}`} aria-labelledby="modal-title" onCancel={(event) => {
    event.preventDefault()
    if (!busy) onClose()
  }}>
    <header className="modal-heading">
      <h2 id="modal-title">{title}</h2>
      <button className="icon-button" type="button" aria-label="关闭窗口" disabled={Boolean(busy)} onClick={onClose}><Icon name="close" /></button>
    </header>
    <div className="modal-body">
      <Notice notice={notice} onDismiss={onDismiss} />
      {children}
    </div>
  </dialog>
}

function ConnectionSettings({ form, edit, checks, auth, busy, running, onSave, onCheck, onAuthorize }) {
  const credentialStatus = form.clientSecret ? 'pending' : form.clientSecretConfigured ? 'configured' : 'missing'
  const credentialLabel = { pending: '待保存', configured: '已配置', missing: '未配置' }[credentialStatus]
  return <>
    <form onSubmit={(event) => { event.preventDefault(); onSave() }}>
      <section className="config-section">
        <h3>源应用</h3>
        <Field label="应用地址"><input required type="url" value={form.appUrl} onChange={(e) => edit('appUrl', e.target.value)} placeholder="https://租户.feishu.cn/ae/app_manage/…" /></Field>
        <Field label="环境"><select value={form.branch} onChange={(e) => edit('branch', e.target.value)}><option value="develop">开发中版本</option><option value="master">线上版本</option></select></Field>
      </section>
      <details className="advanced-settings">
        <summary>高级设置</summary>
        <div className="advanced-settings-body">
          <section className="config-section">
            <h3>应用信息</h3>
            <Field label="应用名称"><input value={form.appName} onChange={(e) => edit('appName', e.target.value)} placeholder="留空时使用应用标识" /></Field>
            <Field label="开发环境地址" hint="可自动发现"><input type="url" value={form.devUrl} onChange={(e) => edit('devUrl', e.target.value)} placeholder="https://租户-dev….aedev.feishuapp.cn" /></Field>
          </section>
          <section className="config-section">
            <div className="config-section-title"><h3>应用 API 凭证</h3><span className={`config-status ${credentialStatus}`}>{credentialLabel}</span></div>
            <div className="field-pair">
              <Field label="Client ID"><input autoComplete="off" value={form.clientId} onChange={(e) => edit('clientId', e.target.value)} placeholder="c_…" /></Field>
              <Field label="Client Secret" hint={form.clientSecretConfigured ? '留空保持不变' : ''}><input type="password" autoComplete="new-password" value={form.clientSecret} onChange={(e) => edit('clientSecret', e.target.value)} placeholder={form.clientSecretConfigured ? '••••••••（已保存）' : '请输入 Client Secret'} /></Field>
            </div>
            <div className="field-pair">
              <Field label="对象 API 名称" hint="每行一个"><textarea rows={3} value={form.objectIds} onChange={(e) => edit('objectIds', e.target.value)} /></Field>
              <Field label="流程 API 名称" hint="每行一个"><textarea rows={3} value={form.flowIds} onChange={(e) => edit('flowIds', e.target.value)} /></Field>
            </div>
          </section>
          <section className="config-section">
            <h3>目标多维表格</h3>
            <Field label="多维表格地址" hint="选填"><input type="url" value={form.baseUrl} onChange={(e) => edit('baseUrl', e.target.value)} placeholder="https://租户.feishu.cn/base/…" /></Field>
            <p className="hint">留空时，首次导出采集到物料后自动创建并保存地址，后续导出复用同一张表。</p>
          </section>
        </div>
      </details>
      <div className="dialog-actions">
        {running ? <span className="hint">当前实例正在导出</span> : null}
        <button className="button primary" disabled={Boolean(busy) || running}>{busy === 'save' ? '保存中…' : form.id ? '保存修改' : '添加实例'}<Icon name="check" size={16} /></button>
      </div>
    </form>
    {form.id ? <section className="authorization">
      <div className="section-heading"><h3>授权状态</h3><button className="text-button" disabled={Boolean(busy)} onClick={onCheck}><Icon name="refresh" size={15} />{busy === 'check' ? '检查中…' : '检查授权'}</button></div>
      {[['browser', '应用浏览器', '浏览器授权'], ['ae', '工程下载', 'ae 授权'], ['api', '应用 API'], ['lark', '多维表格', '飞书授权']].map(([key, title, action]) => (
        <div className="auth-row" key={key}>
          <span className={`auth-dot ${checks[key]?.ready ? 'ready' : ''}`} />
          <div><strong>{title}{checks[key]?.name ? ` · ${checks[key].name}` : ''}</strong><span>{checks[key]?.ready ? '已授权' : checks[key]?.message || '待检查'}</span></div>
          {action ? <button className="text-button" disabled={Boolean(busy)} onClick={() => onAuthorize(key)}>{action}</button> : null}
        </div>
      ))}
      {auth ? <div className="qr-panel">
        <a href={auth.url} target="_blank" rel="noreferrer">打开飞书授权页面 <Icon name="link" size={14} /></a>
        <img src={auth.qrcode} alt="飞书授权二维码" />
        <button className="button" disabled={Boolean(busy)} onClick={() => onAuthorize('lark-complete')}>完成飞书授权</button>
      </div> : null}
    </section> : null}
  </>
}

function MaterialList({ connectionId, items, loading, kind, onKind, filter, onFilter, onRefresh, busy, archiveId }) {
  const query = filter.trim().toLowerCase()
  const displayed = items.filter((item) => item.kind === kind && (!query || `${item.name} ${item.apiName}`.toLowerCase().includes(query)))
  const kindName = kinds.find((item) => item.id === kind).name
  const counts = new Map()
  for (const item of items) counts.set(item.kind, (counts.get(item.kind) || 0) + 1)

  function handleTabKey(event) {
    const current = kinds.findIndex((item) => item.id === kind)
    const destinations = { ArrowLeft: (current - 1 + kinds.length) % kinds.length, ArrowRight: (current + 1) % kinds.length, Home: 0, End: kinds.length - 1 }
    const index = destinations[event.key]
    if (index === undefined) return
    event.preventDefault()
    onKind(kinds[index].id)
    event.currentTarget.querySelectorAll('[role="tab"]')[index]?.focus()
  }

  return <section className="materials-section" aria-label="物料清单">
    <div className="material-toolbar">
      <h2>物料清单</h2>
      <div className="list-tools">
        {archiveId ? <a className="icon-button bordered" href={`/api/jobs/${archiveId}/archive`} aria-label="下载最近导出归档" title="下载最近导出归档"><Icon name="download" size={17} /></a> : null}
        <label className="search"><Icon name="search" size={15} /><input type="search" aria-label="搜索物料" placeholder="名称或 API 名称" value={filter} onChange={(e) => onFilter(e.target.value)} /></label>
        <button type="button" className="icon-button bordered" aria-label="刷新物料清单" title="刷新物料清单" disabled={!connectionId || Boolean(busy)} onClick={onRefresh}><Icon name="refresh" size={17} /></button>
      </div>
    </div>
    <div className="type-tabs" role="tablist" aria-label="物料类型" onKeyDown={handleTabKey}>
      {kinds.map((item) => <button id={`kind-${item.id}`} type="button" role="tab" aria-selected={kind === item.id} aria-controls="material-panel" tabIndex={kind === item.id ? 0 : -1} onClick={() => onKind(item.id)} key={item.id}>
        <Icon name={item.id} size={16} /><span>{item.name}</span><small>{counts.get(item.id) || 0}</small>
      </button>)}
    </div>
    <div id="material-panel" role="tabpanel" aria-labelledby={`kind-${kind}`} aria-busy={loading} tabIndex={0}>
      {displayed.length ? <div className="table-scroll">
        <table className="material-table">
          <thead><tr><th scope="col">名称<span className="column-alias"> / API 名称</span></th><th scope="col" className="source-column">来源</th><th scope="col">大小</th><th scope="col" className="time-column">收集时间</th><th scope="col" className="action-cell">操作</th></tr></thead>
          <tbody>{displayed.map((item) => <tr key={item.id}>
            <td><div className="file-name"><span className="file-icon"><Icon name={kind} size={17} /></span><div><strong className="file-title">{item.name}</strong><small className="api-name">{item.apiName}</small></div></div></td>
            <td className="source-column">{sources[item.source] || item.source || '—'}</td>
            <td className="format">{size(item.size)}</td>
            <td className="time-column format">{time(item.createdAt)}</td>
            <td className="action-cell"><a className="text-button" href={`/api/materials/${item.id}/download`} aria-label={`下载 ${item.name}`}><Icon name="download" size={15} />下载</a></td>
          </tr>)}</tbody>
        </table>
      </div> : <div className="empty">
        <span className="empty-icon"><Icon name={query ? 'search' : kind} size={24} /></span>
        <strong>{loading ? '正在加载物料…' : query ? '没有匹配的物料' : `暂无${kindName}物料`}</strong>
        {!loading ? <span>{query ? '试试其他名称或 API 名称。' : '导入文件或发起导出后，物料会显示在这里。'}</span> : null}
      </div>}
    </div>
  </section>
}

function ExportFeedback({ job, busy, onResume }) {
  if (!job || job.status === 'completed') return null
  const detail = Object.values(job.sourceErrors || {})[0] || job.items?.find((item) => item.error)?.error ||
    job.events?.findLast((event) => event.message.startsWith('多维表格写入失败'))?.message
  const titles = { failed: '导出失败', partial: '部分物料未导出', cancelled: '导出已停止', interrupted: '导出已中断' }
  return <div className={`notice ${job.status !== 'cancelled' ? 'error' : ''}`} role="status">
    <span><strong>{titles[job.status]}</strong>{detail ? ` · ${detail}` : ''}</span>
    <button className="text-button" type="button" disabled={Boolean(busy)} onClick={() => onResume(job.id)}>继续导出</button>
  </div>
}

export default function App() {
  const [workspace, setWorkspace] = useState(null)
  const [activeId, setActiveId] = useState('')
  const [materialKind, setMaterialKind] = useState('pages')
  const [materialSnapshot, setMaterialSnapshot] = useState({ connectionId: '', items: [] })
  const [materialReload, setMaterialReload] = useState(0)
  const [filter, setFilter] = useState('')
  const [dialog, setDialog] = useState('')
  const [form, setForm] = useState(initial)
  const [baseUrlDraft, setBaseUrlDraft] = useState(null)
  const [checks, setChecks] = useState({})
  const [auth, setAuth] = useState(null)
  const [selected, setSelected] = useState([])
  const [mode, setMode] = useState('online')
  const [importKind, setImportKind] = useState('pages')
  const [files, setFiles] = useState([])
  const [busy, setBusy] = useState('')
  const [notice, setNotice] = useState(null)
  const initialized = useRef(false)
  const revision = useRef(0)
  const connections = workspace?.connections || []
  const jobs = workspace?.jobs || []
  const connection = connections.find((item) => item.id === activeId)
  const settingsBaseUrl = baseUrlDraft ?? connections.find((item) => item.id === form.id)?.baseUrl ?? ''
  const connectionJobs = jobs.filter((job) => job.connectionId === activeId)
  const runningJob = connectionJobs.find((job) => activeStatuses.has(job.status))
  const recentJob = connectionJobs[0]
  const archive = connectionJobs.find((job) => job.status === 'completed' || job.status === 'partial')
  const exportProgress = runningJob || (busy === 'start' || busy === 'resume'
    ? { status: 'queued', kinds: busy === 'start' ? selected : recentJob?.kinds } : null)
  const exporting = Boolean(exportProgress)
  const hasActive = jobs.some((job) => activeStatuses.has(job.status))
  const materialRevision = [materialReload, ...connectionJobs.map((job) => `${job.id}:${job.status}:${job.updatedAt}`)].join('|')
  const materialsReady = materialSnapshot.connectionId === activeId && materialSnapshot.revision === materialRevision
  const materials = materialsReady ? materialSnapshot.items : []
  const importType = kinds.find((kind) => kind.id === importKind)

  const refresh = useCallback(async (signal) => {
    const sequence = ++revision.current
    const data = await request('/workspace', { signal })
    if (signal?.aborted || sequence !== revision.current) return data
    setWorkspace(data)
    if (!initialized.current) {
      initialized.current = true
      setActiveId(data.connections[0]?.id || '')
    }
    return data
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    refresh(controller.signal).catch((error) => {
      if (!controller.signal.aborted) setNotice({ error: true, message: error.message })
    })
    return () => controller.abort()
  }, [refresh])

  useEffect(() => {
    if (!hasActive) return
    const controller = new AbortController()
    let timer
    async function poll() {
      try {
        await refresh(controller.signal)
      } catch (error) {
        if (!controller.signal.aborted) setNotice({ error: true, message: error.message })
      }
      if (!controller.signal.aborted) timer = setTimeout(poll, 1500)
    }
    timer = setTimeout(poll, 1500)
    return () => { controller.abort(); clearTimeout(timer) }
  }, [hasActive, refresh])

  useEffect(() => {
    if (!activeId || exporting) return
    const controller = new AbortController()
    request(`/connections/${activeId}/materials`, { signal: controller.signal }).then((items) => {
      if (!controller.signal.aborted) setMaterialSnapshot({ connectionId: activeId, revision: materialRevision, items })
    }).catch((error) => {
      if (controller.signal.aborted) return
      setMaterialSnapshot((old) => ({
        connectionId: activeId, revision: materialRevision, items: old.connectionId === activeId ? old.items : [],
      }))
      setNotice({ error: true, message: error.message })
    })
    return () => controller.abort()
  }, [activeId, exporting, materialRevision])

  async function act(key, fn) {
    setBusy(key)
    setNotice(null)
    try { await fn() } catch (error) { setNotice({ error: true, message: error.message }) }
    finally { setBusy('') }
  }

  function activate(id) {
    setActiveId(id)
    setFilter('')
    setNotice(null)
  }

  function openDialog(name) {
    setNotice(null)
    setDialog(name)
  }

  function openSettings(instance) {
    setForm(instance ? toForm(instance) : initial)
    setBaseUrlDraft(null)
    setChecks({})
    setAuth(null)
    openDialog('settings')
  }

  async function save() {
    const editing = Boolean(form.id)
    const { updatedAt: _updatedAt, tenant: _tenant, namespace: _namespace, ...input } = form
    void _updatedAt; void _tenant; void _namespace
    const splitIDs = (value) => value.split('\n').map((part) => part.trim()).filter(Boolean)
    const saved = await post('/connections', { ...input, baseUrl: settingsBaseUrl, objectIds: splitIDs(form.objectIds), flowIds: splitIDs(form.flowIds) })
    revision.current++
    initialized.current = true
    setWorkspace((old) => ({ connections: [saved, ...(old?.connections || []).filter((item) => item.id !== saved.id)], jobs: old?.jobs || [] }))
    if (activeId !== saved.id) activate(saved.id)
    setForm(toForm(saved))
    setBaseUrlDraft(null)
    setChecks({})
    setAuth(null)
    setNotice({ message: editing ? '实例修改已保存。' : '实例已添加，可以检查或设置授权。' })
  }

  async function probe() {
    setChecks(await post(`/connections/${form.id}/check`, {}))
    const fresh = await refresh()
    const saved = fresh.connections.find((item) => item.id === form.id)
    if (saved) setForm((old) => ({ ...old, appName: saved.appName, devUrl: saved.devUrl }))
  }

  async function authorize(kind) {
    const result = await post(`/connections/${form.id}/authorize`, { kind })
    if (result.message) setNotice({ message: result.message })
    if (result.url) setAuth(result)
    if (kind === 'lark-complete') { setAuth(null); await probe() }
  }

  async function importFiles() {
    const data = new FormData()
    data.append('connectionId', activeId)
    data.append('kind', importKind)
    for (const file of files) data.append('files', file)
    const result = await request('/materials/import', { method: 'POST', body: data })
    if (result.materials.length) {
      setMaterialKind(importKind)
      setFilter('')
      setMaterialReload((value) => value + 1)
      setDialog('')
    }
    const summary = `已导入 ${result.materials.length} 个文件`
    setNotice({ error: result.errors.length > 0, message: [summary, ...result.errors].join('。') })
  }

  async function start() {
    setDialog('')
    const created = await post('/jobs', { connectionId: activeId, kinds: selected, mode })
    revision.current++
    setWorkspace((old) => ({ ...old, jobs: [created, ...old.jobs.filter((item) => item.id !== created.id)] }))
    setFilter('')
    if (!selected.includes(materialKind)) setMaterialKind(selected[0])
  }

  async function resume(id) {
    await post(`/jobs/${id}/retry`, {})
    revision.current++
    setWorkspace((old) => ({ ...old, jobs: old.jobs.map((job) => job.id === id ? { ...job, status: 'queued', progress: [] } : job) }))
  }

  const modalTitle = {
    settings: form.id ? '编辑 aPaaS 实例' : '添加 aPaaS 实例',
    import: '导入物料', export: '发起导出',
  }[dialog]

  return <div className="app">
    <header className="topbar">
      <a className="brand" href="/"><span className="brand-symbol"><Icon name="instance" size={21} /></span><strong>aPaaS <span>物料工作台</span></strong></a>
      <span className="local"><i />本机工作台</span>
    </header>
    <div className="layout">
      <aside className="instance-sidebar" aria-label="aPaaS 实例">
        <div className="sidebar-heading"><h2>aPaaS 实例</h2><button className="button small" type="button" disabled={Boolean(busy)} onClick={() => openSettings()}><Icon name="plus" size={15} />添加实例</button></div>
        {connections.length ? <ul className="instance-list">
          {connections.map((item) => <li key={item.id}>
            <button className="instance-select" type="button" aria-current={item.id === activeId ? 'true' : undefined} disabled={Boolean(busy)} onClick={() => activate(item.id)}>
              <span className="instance-icon"><Icon name="instance" size={19} /></span>
              <span className="instance-label"><strong>{item.appName || item.namespace}</strong><small>{item.namespace} · {item.branch}</small></span>
            </button>
            <button className="instance-edit" type="button" aria-label={`编辑实例 ${item.appName || item.namespace}`} title="编辑实例" disabled={Boolean(busy)} onClick={() => openSettings(item)}><Icon name="edit" size={15} /></button>
          </li>)}
        </ul> : <div className="sidebar-empty">
          {workspace || notice?.error ? <p>暂无实例</p> : <p role="status">正在加载实例…</p>}
          {!workspace && notice?.error ? <button className="text-button" disabled={Boolean(busy)} onClick={() => void act('refresh', () => refresh())}>重试加载</button> : null}
        </div>}
      </aside>

      <main>
        {!dialog ? <Notice notice={notice} onDismiss={() => setNotice(null)} /> : null}
        {connection ? <>
          <header className="main-heading">
            <div className="instance-heading">
              <div className="instance-meta"><span>{connection.namespace}</span><span className="branch">{connection.branch}</span></div>
              <h1>{connection.appName || connection.namespace}</h1>
              <div className="instance-target" aria-live="polite">
                <Icon name="link" size={14} />
                {connection.baseUrl ? <a href={connection.baseUrl} target="_blank" rel="noreferrer">打开目标多维表格</a> : <span>首次导出时创建目标多维表格</span>}
              </div>
            </div>
            <div className="workspace-actions">
              <button className="button" type="button" disabled={Boolean(busy)} onClick={() => {
                setImportKind(materialKind)
                setFiles([])
                openDialog('import')
              }}><Icon name="upload" size={16} />导入文件</button>
              <button className="button primary" type="button" disabled={Boolean(busy) || exporting} onClick={() => {
                setSelected([materialKind])
                setMode('online')
                openDialog('export')
              }}><Icon name="arrow" size={16} />{exporting ? '正在导出' : '发起导出'}</button>
            </div>
          </header>

          {exportProgress ? <ExportProgress job={exportProgress} busy={busy} onStop={(id) => void act('cancel', async () => {
            await post(`/jobs/${id}/cancel`, {})
            await refresh()
          })} /> : <>
            <ExportFeedback job={recentJob} busy={busy} onResume={(id) => void act('resume', () => resume(id))} />
            <MaterialList connectionId={activeId} items={materials} loading={!materialsReady} kind={materialKind} onKind={setMaterialKind} filter={filter} onFilter={setFilter} busy={busy} archiveId={archive?.id} onRefresh={() => void act('refresh', async () => {
              await refresh()
              setMaterialReload((value) => value + 1)
            })} />
          </>}
        </> : <div className="workspace-empty">
          <span className="empty-icon"><Icon name="instance" size={29} /></span>
          <h1>aPaaS 物料工作台</h1>
          {workspace || notice?.error ? <><p>添加实例后，即可导入、导出并查看物料。</p><button className="button primary" type="button" disabled={Boolean(busy)} onClick={() => openSettings()}><Icon name="plus" size={17} />添加实例</button></> : <p role="status">正在加载工作台…</p>}
        </div>}
      </main>
    </div>

    {dialog ? <Modal title={modalTitle} busy={busy} onClose={() => setDialog('')} notice={notice} onDismiss={() => setNotice(null)} variant={dialog === 'settings' ? 'drawer' : 'modal'}>
      {dialog === 'settings' ? <ConnectionSettings form={{ ...form, baseUrl: settingsBaseUrl }} edit={(key, value) => {
        if (key === 'baseUrl') setBaseUrlDraft(value)
        else setForm((old) => ({ ...old, [key]: value }))
      }} checks={checks} auth={auth} busy={busy} running={form.id === activeId && Boolean(runningJob)} onSave={() => void act('save', save)} onCheck={() => void act('check', probe)} onAuthorize={(kind) => void act(kind, () => authorize(kind))} /> : null}

      {dialog === 'import' ? <form onSubmit={(event) => { event.preventDefault(); void act('upload', importFiles) }}>
        <Field label="物料类型"><select value={importKind} onChange={(event) => { setImportKind(event.target.value); setFiles([]) }}>
          {kinds.map((kind) => <option key={kind.id} value={kind.id}>{kind.name}</option>)}
        </select></Field>
        <Field label="原始文件" hint={importType.format}><input key={importKind} type="file" accept={importType.accept} multiple required onChange={(event) => setFiles(Array.from(event.target.files))} /></Field>
        <div className="dialog-actions">
          <button className="button" type="button" disabled={Boolean(busy)} onClick={() => setDialog('')}>取消</button>
          <button className="button primary" disabled={Boolean(busy) || !files.length}>{busy === 'upload' ? '正在导入…' : '开始导入'}</button>
        </div>
      </form> : null}

      {dialog === 'export' ? <form onSubmit={(event) => { event.preventDefault(); void act('start', start) }}>
        <Field label="采集方式"><select value={mode} onChange={(event) => setMode(event.target.value)}><option value="online">在线采集</option><option value="imported">使用导入文件</option></select></Field>
        <div className="section-heading export-selection-heading"><h3>物料类型</h3><button className="text-button" type="button" disabled={Boolean(busy)} onClick={() => setSelected(selected.length === kinds.length ? [] : kinds.map((kind) => kind.id))}>{selected.length === kinds.length ? '取消全选' : '全选'}</button></div>
        <div className="export-options">{kinds.map((kind) => <label className="export-option" key={kind.id}>
          <input type="checkbox" checked={selected.includes(kind.id)} onChange={(event) => setSelected((old) => event.target.checked ? [...old, kind.id] : old.filter((id) => id !== kind.id))} />
          <Icon name={kind.id} size={17} /><span>{kind.name}</span><small>{kind.detail}</small>
        </label>)}</div>
        <div className="dialog-actions">
          <button className="button" type="button" disabled={Boolean(busy)} onClick={() => setDialog('')}>取消</button>
          <button className="button primary" disabled={Boolean(busy) || Boolean(runningJob) || !selected.length}>{busy === 'start' ? '正在创建…' : '开始导出'}</button>
        </div>
      </form> : null}

    </Modal> : null}
  </div>
}
