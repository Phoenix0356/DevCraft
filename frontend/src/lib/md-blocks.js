// Markdown「文档块」装饰器：把 marked 产出的 HTML 里每个围栏代码块（<pre>）
// 套一层带标题栏的外壳 <div class="md-block">（语言标签 + 行数 + 复制 + 展开/收起）。
//
// 为什么要在「字符串 → 字符串」这一步动手（D9）：AI 正文走 v-html 进 DOM，
// Vue 编译期看不到这些节点，既绑不了 @click，也用不了 v-if 决定折叠态。
// 所以结构补齐只能在此完成，交互交给父容器事件委托（见 ChatApp.vue onBlockClick）。
//
// 为什么用 DOMParser 而不是正则改写 HTML（硬约束，禁止正则）：
//   marked 的输出形状随版本与内容变化（属性顺序、有无 <code> 包裹、正文里出现
//   形似 <pre> 的文本、多行 pre 跨行），正则既会漏也会误伤正文。DOMParser 是
//   浏览器原生解析器（零新增依赖），只操作真 <pre> 节点，非节点级文本一概不动。
//   本模块只做**结构包裹**：原 <pre> 节点整体搬进外壳，内部文本节点一个字符都
//   不改——否则复制到的就不是围栏内原文了（R3）。
//
// 折叠态从哪来：调用方传 reactive Set（已手动展开的长块 key），本模块**只读**它
//   来决定 class，状态 → HTML 单向流，靠 v-html 重算反映展开/收起，
//   绝不 nextTick 后摸 DOM 改样式（D9.2）。
//
// 顺带的安全事实（不是本模块的职责，但值得记录）：DOMParser 生成的文档没有活动
//   浏览上下文，不会执行脚本、不会加载图片；外壳节点用 createElement + textContent
//   构造，标签名与行数都经 textContent 落地，不拼 innerHTML，注入不进来。

// ---------------- 折叠阈值：看着不对就改这三个数 ----------------
// 行数按围栏内的换行数计（与 CSS 里 max-height: calc(8 * 1.5em) 同源——
// 仓库刻意不抽 CSS token，CSS 读不到 JS 常量，改 COLLAPSE_PREVIEW_LINES
// 必须同时改 ChatApp.vue 里那条 max-height，两处各有注释互指）。
export const COLLAPSE_LINES = 12           // 超过 12 行才默认折叠（D7）
export const COLLAPSE_CHARS = 1500         // 或超过 1500 字符才折叠（单行压缩 JSON 也算长块）
export const COLLAPSE_PREVIEW_LINES = 8    // 折叠时露出前 8 行（D8）

/**
 * 围栏内行数。marked 保留闭合 ``` 前那个换行（text 形如 "a\nb\n"），
 * split 后末尾多一个空串，不计行；连续空行是正文行，照算。
 */
function countLines(text) {
  const parts = text.split('\n')
  if (parts.length > 1 && parts[parts.length - 1] === '') parts.pop()
  return parts.length
}

/** 建元素的小工具：一律 textContent 落地文本（不拼 innerHTML，杜绝注入） */
function el(doc, tag, cls, text) {
  const node = doc.createElement(tag)
  if (cls) node.className = cls
  if (text != null) node.textContent = text
  return node
}

/**
 * decorateCodeBlocks：给 HTML 里每个 <pre> 加文档块外壳，返回序列化后的 HTML。
 * @param {string} html renderMarkdown 的产物
 * @param {object} opts
 * @param {string} opts.keyPrefix 折叠 key 前缀（消息序号，如 'm3' → key 'm3:0'）
 * @param {Set}   [opts.expanded] 视图态 Set：存**用户已手动展开**的长块 key。
 *   为什么存「已展开」而不是「已折叠」：默认态是长块折叠，只有长块才可能翻转，
 *   存反面状态集合才不会无界增长（短块永不进集合，新消息再多也不占内存）。
 * @returns {string} 装饰后的 HTML；无 <pre> 时等价于原样返回
 */
export function decorateCodeBlocks(html, {keyPrefix = 'b', expanded = null} = {}) {
  if (!html) return ''
  // 用 DOMParser 的 body 当片段根节点，而不是自己套一层 <div data-md-root>：
  // marked 不消毒（自 v5 起无 sanitize 选项），AI 回复里的裸 HTML 会直通输出，
  // 万一正文带个未配对的 </div>，自套外壳会被它提前闭合、后半段正文直接丢失；
  // 而 body 是最外层不可能被内层结束标签闭合，多余的 </div> 只会被解析器忽略。
  const doc = new DOMParser().parseFromString(html, 'text/html')
  const root = doc.body
  if (!root) return html          // 理论不可达；解析异常时原样返回，绝不吞掉正文
  const pres = Array.from(root.querySelectorAll('pre'))
  if (pres.length === 0) return html   // 没有代码块就不做无谓的序列化往返

  pres.forEach((pre, i) => {
    const key = `${keyPrefix}:${i}`
    // marked 的围栏语言固定输出 <code class="language-xxx">；裸 ``` 无类名 → text
    const lang = (pre.querySelector('code')?.getAttribute('class') || '')
      .match(/(?:^|\s)language-([\w+#._-]+)/)?.[1] || 'text'
    const text = pre.textContent || ''
    // text 就是围栏内原文（marked 保留闭合 ``` 前那个换行，所以末尾多一个 \n——
    // 与 GFM 的"内容行 + 行终止符"定义一致，不算多复制）。这里只拿它数行数/
    // 判长度，**不参与复制**：复制走 live DOM 的 textContent（见 ChatApp 委托）。
    const lines = countLines(text)
    // 折叠判定（D7）：行数**或**字符数超阈值；未超的短块连折叠按钮都不渲染（AC5b）
    const isLong = lines > COLLAPSE_LINES || text.length > COLLAPSE_CHARS
    // 当前是否折叠 = 长块 且 未被手动展开
    const collapsed = isLong && !(expanded && expanded.has(key))
    // 「展开剩余 N 行」的 N：按行折叠是准的；若块是靠字符数判定为长块、行数反而
    // 不足 8（整段压缩成一行的 JSON），N 会是 0 甚至负数 → 退化成光秃秃的「展开」。
    const rest = Math.max(lines - COLLAPSE_PREVIEW_LINES, 0)

    const block = el(doc, 'div', collapsed ? 'md-block md-block-collapsed' : 'md-block')
    block.setAttribute('data-key', key)

    const head = el(doc, 'div', 'md-block-head')
    head.appendChild(el(doc, 'span', 'md-block-lang', lang))
    head.appendChild(el(doc, 'span', 'md-block-meta', `${lines} 行`))
    const acts = el(doc, 'span', 'md-block-acts')
    // 按钮只带 data-act/data-key，**不带正文**：复制内容从 live DOM 的
    // pre.textContent 现取（D9.4），塞 data-* 反而会被属性值折叠换行/转义坑掉。
    const copyBtn = el(doc, 'button', 'md-btn', '复制')
    copyBtn.setAttribute('type', 'button')
    copyBtn.setAttribute('data-act', 'copy')
    copyBtn.setAttribute('data-key', key)
    acts.appendChild(copyBtn)
    if (isLong) {
      const toggleBtn = el(doc, 'button', 'md-btn', collapsed ? (rest > 0 ? `展开剩余 ${rest} 行` : '展开') : '收起')
      toggleBtn.setAttribute('type', 'button')
      toggleBtn.setAttribute('data-act', 'toggle')
      toggleBtn.setAttribute('data-key', key)
      acts.appendChild(toggleBtn)
    }
    head.appendChild(acts)

    // 结构包裹：先把外壳插到 pre 前面，再把 pre 原节点移进去（节点移动不改内容）
    pre.parentNode.insertBefore(block, pre)
    block.appendChild(head)
    block.appendChild(pre)
  })

  return root.innerHTML
}
