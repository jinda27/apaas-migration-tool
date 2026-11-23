export async function request(path, { signal, timeout = 60000, ...options } = {}) {
  const response = await fetch(`/api${path}`, {
    ...options,
    signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(timeout)]) : AbortSignal.timeout(timeout),
    headers: { Accept: 'application/json', 'X-Requested-With': 'apaas-migration-tool', ...options.headers },
  })

  const result = await response.json().catch(() => ({}))
  // 直接呈现后端业务错误，便于处理授权和物料失败。
  if (!response.ok) throw new Error(result.error || `请求失败（HTTP ${response.status}）`)
  return result
}

export function post(path, body) {
  return request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
}
