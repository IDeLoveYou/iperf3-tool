import { defineConfig } from 'vite'

export default defineConfig({
  define: {
    'process.env.NODE_ENV': JSON.stringify('production')
  },
  build: {
    outDir: '../internal/web/assets',
    emptyOutDir: true,
    lib: {
      entry: 'src/monitor-ui.js',
      name: 'IperfMonitorUI',
      formats: ['iife'],
      fileName: () => 'monitor-ui.js'
    },
    cssCodeSplit: false,
    rollupOptions: {
      output: {
        assetFileNames: 'monitor-ui.css'
      }
    }
  }
})
