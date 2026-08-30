// 前端入口文件（≈ Java 的 main 方法 / Spring Boot 的 Application.run）。
// 执行顺序：创建 Vue 应用实例 → 安装组件库 → 挂载到页面。

// import ... from 是 ES Module 语法（≈ Java 的 import），
// Vite 构建器负责解析依赖并打包。
import {createApp} from 'vue'      // Vue 3 框架的应用工厂函数
import naive from 'naive-ui'       // Naive UI 组件库（整体导入，注册后模板里可直接用 n-xxx 组件）
import App from './App.vue'        // 根组件（.vue 单文件组件，见 App.vue 顶部说明）
import './style.css';               // 全局样式（无导出的纯副作用导入）

// 创建应用实例并注册 Naive UI：
// app.use(naive) ≈ 全局注册所有 n-button / n-layout 等组件，
// 之后任何组件模板里都能直接使用，无需逐个 import。
const app = createApp(App)
app.use(naive)

// 把整个应用渲染到 index.html 里 id="app" 的 div 中（DOM 挂载点）。
app.mount('#app')
