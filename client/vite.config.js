import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig(({ mode }) => {
  const env = { ...loadEnv(mode, '..', ''), ...process.env }
  const backend = `http://${env.BACKEND_HOST || '127.0.0.1'}:${env.BACKEND_PORT || '8081'}`

  return {
    plugins: [react()],
    envDir: '..',
    server: {
      host: env.FRONTEND_HOST || '127.0.0.1',
      port: Number(env.FRONTEND_PORT || 10020),
      strictPort: true,
      open: false,
      proxy: {
        '/api': {
          target: backend,
          changeOrigin: true,
        },
      },
    },
  }
})
