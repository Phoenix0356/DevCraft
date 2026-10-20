// 剪贴板写入的统一小工具（单一实现，两处共用：ChatApp 里 markdown 代码块标题栏的
// 「复制」、ApiDocManager 的「导出规范」降级路径）。
// 为什么要两条路：
//   ① navigator.clipboard.writeText —— 安全上下文（桌面 Wails WebView、浏览器 https）
//      下的标准异步 API；但服务器模式用 http 访问属于非安全上下文，浏览器可能直接
//      不提供 navigator.clipboard，或 writeText 因权限策略 reject。
//   ② 隐藏 textarea + document.execCommand('copy') —— 非安全上下文唯一还能工作的
//      老式同步手段（ApiDocManager 导出规范原先就是这么降级的，这里收拢为共用实现，
//      不再留两份）。
// 两条路都失败时返回 false（绝不"看起来成功但剪贴板是空的"），由调用方提示
// message.error 或弹窗降级——静默失败是禁止项。

/**
 * copyText：把文本写入剪贴板。
 * 本工具只写字符串，不做任何加工：调用方给的就是纯文本原文——ChatApp 给的是代码块
 * <pre> 的 textContent（= 围栏内原文，逐字节一致），ApiDocManager 给的是规范 JSON 原文。
 * @param {string} text 要复制的文本
 * @returns {Promise<boolean>} true = 已写入剪贴板；false = 两条路都不可用/被拒绝
 */
export async function copyText(text) {
  // 路①：现代异步剪贴板 API。失败（不存在 / 被权限策略拒绝）不直接 return false，
  // 而是落入路②继续尝试——http 下 navigator.clipboard 为 undefined 时这里抛
  // TypeError，同样被 catch 接住。
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch (e) {
    // fall through：降级到 execCommand
  }
  // 路②：隐藏 textarea + execCommand('copy')（已废弃，但在非安全上下文仍可用）
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    // 移出可视区域而非 display:none：部分浏览器不选中不可见元素；
    // fixed 定位不参与布局，避免 append 瞬间页面滚动/跳动
    ta.style.position = 'fixed'
    ta.style.top = '-9999px'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.focus()   // 部分 WebView 需要先聚焦，选中才会生效
    ta.select()  // 全选文本（长 token/多行都覆盖）
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok    // execCommand 返回 false 时同样由调用方兜底提示
  } catch (e) {
    return false
  }
}
