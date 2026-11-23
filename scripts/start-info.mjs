import { existsSync } from 'node:fs'
import path from 'node:path'

function accessURL() {
  const root = process.env.PROJECT_ROOT
  const envFile = path.join(root, '.env')

  // 环境变量优先，缺失项再从项目 .env 加载。
  if (existsSync(envFile)) process.loadEnvFile(envFile)

  const host = process.env.FRONTEND_HOST || '127.0.0.1'
  const frontendPort = process.env.FRONTEND_PORT || '10020'
  const backendPort = process.env.BACKEND_PORT || '8081'

  // 启动前校验链接与服务使用同一套端口配置。
  for (const [name, value] of [['FRONTEND_PORT', frontendPort], ['BACKEND_PORT', backendPort]]) {
    if (!/^\d+$/.test(value) || Number(value) < 1 || Number(value) > 65535) {
      throw new Error(`${name} 必须是 1～65535 的整数`)
    }
  }
  if (frontendPort === backendPort) throw new Error('前后端端口不能相同')
  return `http://${host}:${frontendPort}`
}

try {
  process.stdout.write(accessURL())
} catch (error) {
  console.error(error.message)
  process.exit(1)
}
