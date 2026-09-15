import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The build writes straight into the Go embed directory, which is committed so
// `go install` works without npm. Rebuild with `make ui` after changing src/.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
  },
  server: {
    // `make ui-dev` gives hot reload against a locally running envoy-view.
    proxy: {
      '/api': 'http://127.0.0.1:8080',
    },
  },
})
