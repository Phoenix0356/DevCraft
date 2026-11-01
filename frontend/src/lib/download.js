// Blob 文本下载的统一小工具（单一实现，两处共用：ChatApp 的「下载修正稿」、
// ApiDocManager 的「导出文档」）。
// 为什么用 Blob + <a download> 而不是原生保存对话框：本应用有两种构建形态——桌面
// WebView2 与服务器模式（浏览器 http 访问）。@wailsio/runtime 只提供 dialogs.SaveFile
// （拿一个路径字符串），真正写盘得再开一个「按任意路径写文件」的后端绑定，服务器模式下
// 那就是对外暴露的任意文件写入口，收益（省一次「另存为」）抵不上风险。
// Blob 下载在两种形态里行为一致，浏览器会自己落到下载目录。
// 只做下载，不做降级：失败返回 false，弹窗给全文还是提示失败由调用方决定。

/**
 * downloadText：把一段文本作为文件触发浏览器下载。
 * @param {string} fileName 落盘文件名（调用方负责拼好，含扩展名）
 * @param {string} text     文件内容（原文，不做任何加工）
 * @param {string} [mime]   Blob 的 MIME，默认 Markdown
 * @returns {boolean} true = 已触发下载；false = 当前环境不允许（调用方需兜底）
 */
export function downloadText(fileName, text, mime = 'text/markdown;charset=utf-8') {
  try {
    const blob = new Blob([text], {type: mime})
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = fileName
    a.rel = 'noopener'
    document.body.appendChild(a)
    a.click()
    a.remove()
    // 立刻 revoke 会让部分浏览器/WebView 取消还没起步的下载，延后一拍再回收
    setTimeout(() => URL.revokeObjectURL(url), 2000)
    return true
  } catch (e) {
    return false
  }
}
