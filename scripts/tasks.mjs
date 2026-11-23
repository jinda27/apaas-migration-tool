import { spawnSync } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { ensureDependencies } from './dependencies.mjs'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const task = process.argv[2]

function run(command, args, cwd = root) {
  const result = spawnSync(command, args, { cwd, stdio: 'inherit', env: process.env })

  // 任一步骤失败时立即终止，保留原始错误。
  if (result.error || result.status !== 0) {
    console.error(result.error?.message ?? `${command} 执行失败`)
    process.exit(result.status || 1)
  }
}

// 使用明确的任务入口，避免执行任意命令。
switch (task) {
  case 'build':
    await ensureDependencies(root, (args) => run('npm', args))
    run('npm', ['run', 'build', '--workspace', 'client'])
    mkdirSync(path.join(root, 'server/bin'), { recursive: true })
    run(process.env.GO_BIN || 'go', ['build', '-o', 'bin/api', './cmd/api'], path.join(root, 'server'))
    break
  case 'check':
    await ensureDependencies(root, (args) => run('npm', args))
    run(process.execPath, ['--test', 'scripts/browser-worker.test.mjs', 'scripts/ports.test.mjs'])
    run('npm', ['run', 'lint', '--workspace', 'client'])
    run(process.env.GO_BIN || 'go', ['vet', './...'], path.join(root, 'server'))
    run(process.env.GO_BIN || 'go', ['test', '-race', './...'], path.join(root, 'server'))
    break
  default:
    console.error('支持的任务：build、check')
    process.exit(1)
}
