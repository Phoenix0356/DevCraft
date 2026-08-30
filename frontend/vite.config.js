import {defineConfig} from 'vite'
import vue from '@vitejs/plugin-vue'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [vue()],
  server: {
    // 固定 IPv4 回环：Wails 桌面开发模式的资源代理按 127.0.0.1 拨号，
    // 若只监听 ::1（localhost 默认）会报 proxy connection refused
    host: '127.0.0.1'
  }
})
