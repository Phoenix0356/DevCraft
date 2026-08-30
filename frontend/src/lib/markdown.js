// 把 LLM 回复（Markdown 文本）渲染成 HTML 的小工具。
// marked 是一个 Markdown → HTML 转换器（≈ Java 的 commonmark 库）。
import {marked} from 'marked'

// 全局配置：breaks=true 让单个换行也变成 <br>（聊天场景更符合直觉）；
// gfm=true 启用 GitHub 风格 Markdown（表格、任务列表、代码块语言标识等）。
marked.setOptions({breaks: true, gfm: true})

/**
 * renderMarkdown：把 Markdown 文本转成 HTML 字符串。
 * 调用方用 v-html 指令插入 DOM（见 ChatApp.vue）。
 * @param {string} text 原始 Markdown
 * @returns {string} HTML
 */
export function renderMarkdown(text) {
  if (!text) return ''  // 空值防御：流式渲染初期 content 可能是空串
  return marked.parse(text)
}
