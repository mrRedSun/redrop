import path from 'node:path'
import { rmSync } from 'node:fs'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The Go server embeds the build from internal/webui/dist.
// `npm run dev` proxies API calls to a redrop server (REDROP_DEV_API).
export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    {
      name: 'clean-generated-assets',
      apply: 'build',
      buildStart() {
        // Vite must not delete the tracked dist/.gitkeep. Remove only the
        // disposable asset directory so old fingerprinted chunks do not accumulate.
        rmSync(path.resolve(import.meta.dirname, '../internal/webui/dist/assets'), { recursive: true, force: true })
      },
    },
  ],
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, './src') },
  },
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: false,
  },
  server: {
    host: true,
    proxy: {
      '/api': { target: process.env.REDROP_DEV_API ?? 'http://127.0.0.1:8080' },
    },
  },
})
