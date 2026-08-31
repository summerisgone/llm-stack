import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Public path under which the dashboard is mounted. The Go server exposes
// the Vite output below <URL_PREFIX>/assets/, and the Gateway's URLRewrite
// strips <URL_PREFIX> before forwarding. Updating this must be done in
// lock-step with the URL_PREFIX env on the pat-service Deployment.
const base = process.env.PAT_PUBLIC_BASE || '/platform'

export default defineConfig({
  plugins: [react()],
  base: `${base}/assets/`,
  build: {
    outDir: '../cmd/pat-service/assets',
    emptyOutDir: true,
  },
})
