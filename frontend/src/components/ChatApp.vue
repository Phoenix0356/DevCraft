<!--
  ChatApp.vue —— 应用主界面（前端最核心的文件）。
  布局：左侧会话侧边栏 + 右侧主区（四态视图，由 view 状态变量控制）：
    chat   —— 聊天区（头部 Agent 选择器 / 中部消息流 / 底部输入框）
    skills —— 技能管理视图（SkillManager：卡片网格占满视图，详情为弹窗）
    agents —— 智能体管理视图（AgentManager：卡片网格占满视图，详情为弹窗）
    apidocs—— 接口文档视图（ApiDocManager：「接口」三栏浏览 + 「变更事件」列表与详情，含导入向导）
  技能/智能体为整个主区切换的视图（视图内详情用弹窗）；设置仍走 SettingsModal 弹窗。
  切到技能/智能体视图时聊天区 DOM 以 v-show 保留：流式帧照常接收处理，
  切回聊天页消息不丢、气泡与滚动状态原样。
  与 Go 后端的两条通信链路（Wails v3，桌面/浏览器两种环境通用）：
    ① 绑定桩函数调用（请求-响应，返回 Promise ≈ JS 的 Future）
    ② 聊天流 JSONStream('chat')（Go 主动推送流式增量：chat:delta / chat:tool / chat:done 帧）
-->
<script setup>
// ---------------- 导入 ----------------
import {ref, reactive, computed, nextTick, onMounted, onBeforeUnmount, watch} from 'vue'
// Vue 响应式 API：
//   ref(x)     = 创建一个"响应式盒子"，.value 变化 → 界面自动重渲染（≈ setState）
//   reactive   = 把对象本身变成代理（附件结果卡要用：异步回调里逐字段写回卡片状态，
//                若写成 raw 对象再塞 ref，改的是 raw 目标、绕开代理，界面不会重渲染）
//   computed   = 派生属性（≈ 只读 getter，依赖变化自动重算）
//   nextTick   = 等 DOM 更新完成后再执行回调
//   onMounted / onBeforeUnmount = 生命周期钩子（≈ @PostConstruct / @PreDestroy）
//   watch      = 监听某个响应式值的变化（≈ 观察者）

import {useMessage} from 'naive-ui' // 取消息提示器（依赖 App.vue 根部的 n-message-provider）

// Wails v3 运行时：JSONStream 建立命名双向通道。
// 桌面模式下由框架在进程内模拟；服务器模式下是真 WebSocket（每标签页独立连接）。
import {JSONStream} from '@wailsio/runtime'
// 自动生成的 Go 方法桩（≈ OpenAPI 客户端）：每个函数对应 app.go 里的一个导出方法。
// 由 `wails3 generate bindings` 生成，勿手改。
import {
  NewSession, ListSessions, DeleteSession, GetMessages,
  SendMessage, CancelTurn, ListAgents, ListAgentsDetail, ApproveDeployment, RejectDeployment,
  ApproveApiDocImport, RejectApiDocImport,
  PreviewApiDocImport, CommitApiDocImport,
  RenameSession
} from '../../bindings/DevCraft/app.js'

import {renderMarkdown} from '../lib/markdown'      // Markdown → HTML
import {decorateCodeBlocks} from '../lib/md-blocks' // HTML → 给每个代码块套标题栏外壳（见 lib/md-blocks.js）
import {copyText} from '../lib/clipboard'           // 剪贴板写入（含 http 非安全上下文降级，见 lib/clipboard.js）
import SettingsModal from './SettingsModal.vue'     // 设置弹窗子组件
import SkillManager from './SkillManager.vue'       // 技能管理视图（主区三态之一，卡片网格 + 详情弹窗）
import AgentManager from './AgentManager.vue'       // 智能体管理视图（主区三态之一，卡片网格 + 详情弹窗）
import ApiDocManager from './ApiDocManager.vue'     // 接口文档视图（主区第四态：两个 tab + 三栏浏览 + 导入向导）
import ApiIssueList from './ApiIssueList.vue'       // E-*/W-* 结构化校验清单（导入向导在用，附件结果卡直接复用，不重写）

// ---------------- 响应式状态（≈ 组件的成员变量）----------------
const message = useMessage()          // 顶部轻提示器

const sessions = ref([])              // 会话列表
const agents = ref([])                // Agent 列表
const currentSession = ref(null)      // 当前选中的会话对象
const messages = ref([])              // 当前会话的消息列表 [{role, content}]
const input = ref('')                 // 输入框双向绑定的文本
const sending = ref(false)            // 是否正在等待 Agent 回合结束
const streaming = ref(null)           // 流式气泡状态 {content, tools:[]}；null = 不在流式中
const showSettings = ref(false)       // 设置弹窗显隐（与 SettingsModal 双向绑定）
// 右侧主区当前视图：'chat' | 'skills' | 'agents' | 'apidocs'（技能/智能体/接口文档为整块视图，不再是弹窗）
const view = ref('chat')
const listRef = ref(null)             // 消息列表 DOM 引用（模板里 ref="listRef" 自动注入），用于滚动

// 当前会话绑定的 Agent id（computed：currentSession 变则自动重算）
const currentAgentId = computed(() => currentSession.value?.agentId || '')
// 当前会话 Agent 的显示名：头部下拉框移除后改为只读文字展示，切换靠消息里的 @ 路由
const currentAgentName = computed(() =>
  agents.value.find(a => a.id === currentAgentId.value)?.name || '')
// ?. 是可选链：currentSession 为 null 时不报错直接得 undefined（≈ Java 的 Optional.map）

// ---------------- 代码块级复制 + 折叠（D2/D7/D8/D9）----------------
// 交互链路三件套（谁读谁、谁写谁）：
//   ① expandedBlocks（视图态 Set）→ ② renderedMessages（computed：状态烘进 HTML 的
//      class）→ ③ onBlockClick（.msg-body 上的事件委托）写 Set → Vue 重算 ② →
//      v-html 整段替换 → 折叠态更新。全程不摸 DOM 改样式。
/** 已手动展开的长块 key 集合（key = 消息序号:块序号，如 'm3:1'）。
 *  纯视图态：不入库、不发事件、不改消息内容（R3b），切会话清空。
 *  用 ref(new Set()) 而非 reactive：Set 的 add/delete/has 由 Vue 的集合代理
 *  自动做依赖收集，computed 里的 has() 读、handler 里的 add/delete 写都能触发重算。*/
const expandedBlocks = ref(new Set())

/** 正式 assistant 消息的渲染结果（HTML 字符串数组，下标与 messages 对齐）。
 *  为什么是单个 computed 而不是模板里直接调函数：模板每次组件重渲染（包括输入框
 *  每一次按键、流式每一个 delta）都会重跑函数，那样每条消息每帧都要走一遍
 *  DOMParser；computed 只依赖 messages + expandedBlocks，两者不变即命中缓存。
 *  用户气泡刻意不装饰：D2 的复制粒度是「AI 回复里的文档块」，用户消息的复制按钮
 *  在 Out of Scope；且折叠遮罩的终点色是按 assistant 卡片底算出来的（见样式注释），
 *  套到 #1f2b27 气泡上会露出一块色斑。*/
const renderedMessages = computed(() =>
  messages.value.map((m, i) => m.role === 'assistant'
    ? decorateCodeBlocks(renderMarkdown(m.content), {keyPrefix: `m${i}`, expanded: expandedBlocks.value})
    : renderMarkdown(m.content))
)

/** 块级复制：text 来自该块 <pre> 的 textContent（= 围栏内原文，零转换）
 *  失败必须提示（AC4）：clipboard 两条降级路都不可用时 message.error，绝不静默 */
async function copyBlock(text) {
  if (await copyText(text)) {
    message.success('已复制该块原文')
  } else {
    message.error('复制失败：当前环境剪贴板不可用')
  }
}

/** 折叠开关：只翻 Set，真正的 class 由 renderedMessages 重算烘出来 */
function toggleBlock(key) {
  const set = expandedBlocks.value
  if (set.has(key)) set.delete(key)
  else set.add(key)
}

/** 块标题栏点击（D9.3 事件委托）：v-html 里的按钮 Vue 绑不了事件，所以在
 *  .msg-body 容器上监听一次，closest('[data-act]') 找出被点的按钮再分流。
 *  为什么不用 data-* 存正文（D9.4）：HTML 属性值里的换行会被折叠成空格、
 *  & < > " 还得来回转义，复制到剪贴板就不再是围栏内原文（AC3 直接破）；
 *  而 live DOM 里那个 <pre> 的 textContent 天然逐字节等于围栏内原文，白拿。*/
function onBlockClick(e) {
  const btn = e.target?.closest?.('[data-act]')
  if (!btn) return                       // 点的是正文/表格等非按钮区域
  const key = btn.getAttribute('data-key')
  if (!key) return
  if (btn.getAttribute('data-act') === 'copy') {
    // 从被点按钮所属的那块往里取 pre：多个块各读各的，绝不串台（AC3）
    copyBlock(btn.closest('.md-block')?.querySelector('pre')?.textContent ?? '')
  } else if (btn.getAttribute('data-act') === 'toggle') {
    toggleBlock(key)
  }
}

// ---------------- 卡死防御：停止按钮 + 前端看门狗 ----------------
// 后端已有双层超时（90 秒流空闲心跳 / 3 分钟回合总上限），看门狗是最后防线：
// 帧完全到不了（后端异常/网络断流）时，前端必须自愈解锁，绝不永久锁死输入框。
// 阈值须大于后端空闲心跳（90 秒），正常情况永远是后端先触发、看门狗备而不用。
const WATCHDOG_MS = 95_000
const stopped = ref(false)    // 本回合是否被用户主动点"停止"（用于幂等收尾，不重复报错）
const errShown = ref(false)   // 本回合是否已弹过错误提示（done 帧 / RPC 返回 / 看门狗三选一）
let watchdogTimer = null      // 看门狗计时器句柄（模块级：生命周期钩子间共享）

function clearWatchdog() {
  if (watchdogTimer) {
    clearTimeout(watchdogTimer)
    watchdogTimer = null
  }
}

/** 启动/重置看门狗：WATCHDOG_MS 内没有收到任何帧就自动报错解锁。
 *  仅回合进行中（sending）才允许挂表：服务器模式多标签页扇出下，
 *  旁观/空闲标签页收到的帧若也挂表，会在 95 秒后弹出无端的"超时"提示。 */
function kickWatchdog() {
  if (!sending.value) return
  clearWatchdog()
  watchdogTimer = setTimeout(onWatchdogTimeout, WATCHDOG_MS)
}

/** 看门狗到期：报错 → 清空气泡 → 解锁输入（后端卡死时的自愈路径） */
function onWatchdogTimeout() {
  watchdogTimer = null
  reportTurnError('响应超时，已为你解锁输入，可重试')
  streaming.value = null
  sending.value = false
}

/** 统一错误提示入口：一个回合最多弹一次（防止 done 帧与 RPC 返回重复报错） */
function reportTurnError(text) {
  if (errShown.value || !text) return
  errShown.value = true
  message.error(text)
}

/** 停止：通知后端取消当前回合（或部署执行），同时本地立即收起气泡（不等后端错误帧）。
 *  后端取消后照常发 chat:done（error=已停止）或 deploy:done（canceled），
 *  对应处理器里凭 stopped 幂等收尾。 */
async function stopTurn() {
  if ((!sending.value && !deployRunning.value) || !currentSession.value) return
  stopped.value = true
  // 注意：这里不清看门狗——后端完全失联时 CancelTurn 本身也会挂起，
  // 看门狗保留着，是"点停止后仍能自愈解锁"的最后手段
  streaming.value = null
  deployRunning.value = false
  try {
    await CancelTurn(currentSession.value.id)
  } catch {
    // 取消调用失败不影响前端已完成的本地解锁；后端回合总上限仍是最终兜底
  }
}

// ---------------- 一键部署：审批卡片 + 进度 ----------------
// 部署链路与回合链路并行：技能生成审批单（deploy:approval 帧）→ 用户点批准 →
// 后端异步执行（deploy:progress 帧逐步推进）→ deploy:done 帧带总结收尾。
// 审批单有效期 10 分钟；未批准/拒绝/过期后端绝不执行。
const approval = ref(null)       // 当前待处理审批单 {...载荷, state}；null = 无
const deployRunning = ref(false) // 是否有部署正在执行（驱动停止按钮与进度气泡）

/** deploy:approval：收到审批卡片（按 sessionId 过滤后到达此处）。 */
function onDeployApproval(payload) {
  if (!currentSession.value || payload.sessionId !== currentSession.value.id) return
  approval.value = {
    approvalId: payload.approvalId,
    flowName: payload.flowName,
    description: payload.description || '',
    target: payload.target,
    params: payload.params || {},
    commands: payload.commands || [],
    // 有效期提示用静态文案即可（不做倒计时，批准时后端会再做过期校验）
    expiresInMin: Math.max(1, Math.round(((payload.expiresAt || 0) - Date.now()) / 60000)),
    state: 'pending'
  }
  nextTick(scrollToBottom)
}

/** deploy:progress：步骤进度，渲染成流式气泡里的步骤标签（复用 streaming.tools 样式）。
 *  与 delta 同理——进度帧到达即证明链路/执行活着，看门狗已在分发前统一重置。 */
function onDeployProgress(payload) {
  if (!currentSession.value || payload.sessionId !== currentSession.value.id) return
  if (stopped.value) return // 本地点过停止：忽略后端残余帧
  deployRunning.value = true
  if (sending.value) return // 回合进行中：进度不打断回合气泡（回合有自己的收尾）
  if (!streaming.value) streaming.value = {content: '', tools: []}
  const label = `部署步骤 ${payload.step}/${payload.total}`
  if (payload.status === 'start') {
    streaming.value.tools.push({skill: label, status: 'running'})
  } else {
    const t = streaming.value.tools.findLast(t => t.skill === label)
    if (t) t.status = payload.status === 'failed' ? 'failed' : 'done'
  }
  nextTick(scrollToBottom)
}

/** deploy:done：部署执行终态。总结文本作为助手消息入列表；
 *  rejected 只更新卡片状态（点击者之外的旁观标签页同步用）。 */
function onDeployDone(payload) {
  if (!currentSession.value || payload.sessionId !== currentSession.value.id) return
  if (approval.value && approval.value.approvalId === payload.approvalId) {
    if (payload.status === 'rejected' || payload.status === 'expired') {
      approval.value = {...approval.value, state: payload.status}
      deployRunning.value = false
      return
    }
    approval.value = null // 已执行的审批卡片使命结束
  }
  deployRunning.value = false
  if (payload.summary) {
    messages.value.push({role: 'assistant', content: payload.summary})
  }
  if (!sending.value) streaming.value = null // 回合进行中的气泡归回合收尾管，不动
  nextTick(scrollToBottom)
}

/** 批准部署：调用后卡片置为已批准态，执行进度随后经 deploy:progress 到达。 */
async function approveDeploy() {
  if (!approval.value) return
  const id = approval.value.approvalId
  approval.value = {...approval.value, state: 'approving'}
  try {
    await ApproveDeployment(id)
    if (approval.value?.approvalId === id) {
      approval.value = {...approval.value, state: 'approved'}
    }
    deployRunning.value = true
  } catch (err) {
    message.error(String(err)) // 常见原因：审批单已过期 / 已被处理
    if (approval.value?.approvalId === id) approval.value = null
  }
}

/** 拒绝部署：后端销毁审批单（绝不执行），卡片置为已拒绝态。 */
async function rejectDeploy() {
  if (!approval.value) return
  const id = approval.value.approvalId
  try {
    await RejectDeployment(id)
  } catch (err) {
    message.error(String(err))
  }
  if (approval.value?.approvalId === id) {
    approval.value = {...approval.value, state: 'rejected'}
  }
}

// ---------------- 接口文档导入：审批卡片 + 终态 ----------------
// 第二条审批链路，与上面的部署链路**并行且状态隔离**：技能 apidoc_import 遇到
// 「删除接口」或「破坏性变更」时不起手写入，而是起一张待审批单并推 api:approval 帧
// → 用户点批准 → 后端 takeApiDocApproval 单次消费 + 异步 CommitApiDocImport
// → api:done 帧回报终态。未批准/已拒绝/已过期，后端一行都不写（PRD D16）。
// 两个 ref 各自独立、互不覆盖：两种审批同时出现的概率极低，但状态必须能共存。
// 有效期只作静态提示，真正的过期判定在后端批准时兜底（与 deploy 同款）。
const apiApproval = ref(null)  // 当前待处理导入审批单 {...载荷, state}；null = 无

/** api:approval：收到接口文档导入审批卡片（按 sessionId 过滤后到达此处）。 */
function onApiDocApproval(payload) {
  if (!currentSession.value || payload.sessionId !== currentSession.value.id) return
  apiApproval.value = {
    approvalId: payload.approvalId,
    docRef: payload.docRef || '',
    project: payload.project || '',
    mode: payload.mode || '',
    breaking: payload.breaking || 0,   // 三态：0 无破坏 / 1 破坏性 / 2 需人工确认
    nextSeq: payload.nextSeq || 0,     // 将生成的事件版本号（0 = 本次不产生事件）
    summary: payload.summary || [],    // 逐接口变更摘要（后端已掩码，不含文档正文）
    stats: payload.stats || {},
    expiresInMin: Math.max(1, Math.round(((payload.expiresAt || 0) - Date.now()) / 60000)),
    state: 'pending'
  }
  nextTick(scrollToBottom)
}

/** api:done：导入执行终态。rejected/expired 只更新卡片状态；
 *  success/failed 收起卡片并把总结作助手消息入列表（后端同时已落库，
 *  与 deploy:done 同款处理）。本链路不产生流式气泡，故不动 streaming。 */
function onApiDocDone(payload) {
  if (!currentSession.value || payload.sessionId !== currentSession.value.id) return
  if (apiApproval.value && apiApproval.value.approvalId === payload.approvalId) {
    if (payload.status === 'rejected' || payload.status === 'expired') {
      apiApproval.value = {...apiApproval.value, state: payload.status}
      return
    }
    apiApproval.value = null // 已落终态的审批卡片使命结束
  }
  if (payload.summary) {
    messages.value.push({role: 'assistant', content: payload.summary})
  }
  nextTick(scrollToBottom)
}

/** 批准导入：卡片置已批准态，写入在后端异步进行，终态随后经 api:done 到达。
 *  批准即人工二次确认成立——破坏性内容唯一的放行理由是用户刚点过批准，
 *  而不是模型说了"确认"（PRD D5.1/D16）。 */
async function approveApiDoc() {
  if (!apiApproval.value) return
  const id = apiApproval.value.approvalId
  apiApproval.value = {...apiApproval.value, state: 'approving'}
  try {
    await ApproveApiDocImport(id)
    if (apiApproval.value?.approvalId === id) {
      apiApproval.value = {...apiApproval.value, state: 'approved'}
    }
  } catch (err) {
    message.error(String(err)) // 常见原因：审批单已过期 / 已被处理
    if (apiApproval.value?.approvalId === id) apiApproval.value = null
  }
}

/** 拒绝导入：后端销毁审批单（绝不写入），卡片置已拒绝态。 */
async function rejectApiDoc() {
  if (!apiApproval.value) return
  const id = apiApproval.value.approvalId
  try {
    await RejectApiDocImport(id)
  } catch (err) {
    message.error(String(err))
  }
  if (apiApproval.value?.approvalId === id) {
    apiApproval.value = {...apiApproval.value, state: 'rejected'}
  }
}

// ---------------- 附件导入（任务 10-20：DeepSeek 式入口，附件 = 操作而不是上下文）----------------
// 四条红线（PRD D1/D8/D9 + R8）：
//   ① 文件正文只作为**参数**传给 Go 方法（PreviewApiDocImport / CommitApiDocImport），
//      不进 prompt、不走 LLM 回合 ⇒ 10-18 的 D5.1「保真」红线一条不破；
//   ② 只吃 .md / .markdown、只吃 ≤2 MB、只吃文本；不合格本地就中文拒绝，不猜、不发请求；
//   ③ 文件名只做文本插值（{{ }} 自动 HTML 转义），不进 v-html、不拼路径、不落盘；
//   ④ 修正稿下载走 Blob（D7：绝不写回用户磁盘上的原文件），不通就降级为弹窗 + 复制。
// 后端四道闸门一条都不绕（D5）：autoFix=true = 确定性修复 → 重新完整校验 → 仍有 E-* 照样拒收；
// 附件条「基线导入」勾选位 = req.initMode = §13.26 的解析档位权威位（不勾而文档自声明
// scope: init 被拒；勾了而文档声明 incremental/full 同样被拒）；allowInitOnNonEmpty 恒 false，
// §13.12「init 只允许空项目」的闸门语义一字未动（D6 只放宽「不再在入口硬拒聊天附件」）。

const MAX_ATTACH_BYTES = 2 * 1024 * 1024   // 2 MB 上限（D9）：超了不读、不发请求
const MD_EXT_RE = /\.(md|markdown)$/i      // 只认扩展名，不看 MIME（拖拽进来的 file.type 常是空串）

// 修复类型 → 中文（词表封闭，与 internal/apidoc/repair.go 的四个 Fix* 常量一一对应；
// 白名单外的 kind 出现说明后端被人加了新修复类别，兜底原样显示而不是丢行）
const FIX_KIND_TEXT = {
  'bullet-form': '标点形态归一',
  'fence-close': '围栏补闭合',
  'pos-column': '补「位置」列（推断）',
  'cross-ref-expand': '交叉引用展开（原样搬运）'
}

const attach = ref(null)           // 待发送附件 {name, size, text, initMode}；null = 无。纯组件视图态，不落库
const attachReading = ref(false)   // FileReader 读取中（独立 loading，与回合 sending 各自一根）
const attachInput = ref(null)      // 隐藏 input[type=file]
const dragOver = ref(false)        // 拖拽悬停输入区（描边提示）
const apiImport = ref(null)        // 附件导入结果卡（视图态；卡片对象本身是 reactive，见 sendWithAttachment）
const fixedModalShow = ref(false)  // 修正稿降级弹窗（下载与剪贴板都不通时）
const fixedModalText = ref('')

function formatSize(bytes) {
  const n = Number(bytes) || 0
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

/** 附件通道的导入请求：与界面向导同一个 DTO（同一结构 = 同一条通道，D5）。
 *  projectName 留空 = 跟随文档 meta.project：拿文件名猜项目名会造出错误的桶，
 *  真没 meta 的散文文档由后端报「无法确定项目名」并指路（不猜，AC4）。
 *  docRef 只作溯源文本（与向导同款：未手填来源时用文件名兜底），不参与任何路径拼接。 */
function attachRequestOf(src, confirmed) {
  return {
    docText: src?.text || '',
    docRef: src?.name || '',
    projectName: '',
    initMode: !!src?.initMode,     // 附件条「基线导入」勾选 = 解析档位权威位（§13.26）
    allowInitOnNonEmpty: false,    // §13.12 闸门恒不放宽（聊天附件通道没有这个放行键）
    autoFix: true,                 // 确定性修复 + 重新完整校验（D2/D4）
    confirmed: !!confirmed,
    title: '', reason: '', label: '', kind: '',
    domainOverrides: []
  }
}

function pickAttach() {
  if (sending.value || attachReading.value) return // 回合进行中 / 正在读文件：不叠操作
  attachInput.value?.click()
}

function onAttachFilePicked(e) {
  const file = e.target.files?.[0]
  e.target.value = '' // 必须清空：否则重选同一个文件不再触发 change（DeployFlowConfig 同款坑）
  if (file) addAttachment(file)
}

/** 校验全在本地做完才谈读取：非 md / 空 / 超限一律中文拒绝，不发任何请求（D9/AC4）。
 *  新附件直接替换旧附件（一次只挂一个，批量多文件是 Out of Scope）。 */
function addAttachment(file) {
  const name = file.name || '未命名文件'
  if (!MD_EXT_RE.test(name)) {
    message.error(`只接受接口文档 Markdown（.md / .markdown）：「${name}」已拒绝`)
    return
  }
  if (!file.size) {
    message.error(`「${name}」是空文件，没有可导入的内容`)
    return
  }
  if (file.size > MAX_ATTACH_BYTES) {
    message.error(`「${name}」${formatSize(file.size)}，超过 ${formatSize(MAX_ATTACH_BYTES)} 上限，已拒绝`)
    return
  }
  attachReading.value = true
  const reader = new FileReader()
  reader.onload = () => {
    attachReading.value = false
    const text = String(reader.result ?? '')
    // 非文本判定：NUL 是文本文件不会出现的字节（不猜内容，只拒绝）
    if (text.includes('\u0000')) {
      message.error(`「${name}」不是文本文件，附件只接受 Markdown 文本`)
      return
    }
    if (!text.trim()) {
      message.error(`「${name}」读出来只有空白字符，没有可导入的内容`)
      return
    }
    attach.value = {name, size: file.size, text, initMode: false}
    message.success(`已附加「${name}」：点发送即「体检 → 机械修复 → 导入」，正文不会发给模型`)
  }
  reader.onerror = () => {
    attachReading.value = false
    message.error('读取文件失败：当前环境不允许访问该文件')
  }
  reader.readAsText(file, 'utf-8')
}

function removeAttachment() {
  attach.value = null
}

/** 收起结果卡（纯视图态清理：不改后端任何东西，导入已落库的结果与卡片无关）。
 *  调用方按钮在 previewing/committing 期间被禁用，所以这里不需要再防"半途丢卡"。 */
function dismissAttachCard() {
  apiImport.value = null
}

// ---- 拖拽入口（与回形针等效，AC8）----
function onZoneDragOver(e) {
  e.preventDefault()
  if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy'
  dragOver.value = true
}
function onZoneDragLeave(e) {
  // 区内元素之间移动也会派发 dragleave：relatedTarget 还在区内就不熄灭高亮
  const zone = e.currentTarget
  if (zone && e.relatedTarget && zone.contains(e.relatedTarget)) return
  dragOver.value = false
}
function onZoneDrop(e) {
  e.preventDefault()
  dragOver.value = false
  const files = e.dataTransfer?.files
  if (!files || files.length === 0) return // 拖的是选中文字/链接而不是文件：不动
  if (files.length > 1) {
    message.error(`一次只能附加一个接口文档（拖入了 ${files.length} 个），已整体拒绝`)
    return
  }
  addAttachment(files[0])
}

/** D8：带附件的「发送」= 触发体检+修复+导入，不注入 prompt、不走 LLM 回合。
 *  输入框里若还有文字，保留不清空（附件优先，绝不静默吞掉用户刚打的字）。
 *  卡片用 reactive()：异步回调逐字段写回状态必须走代理，raw 对象塞 ref 改了不重渲染。 */
async function sendWithAttachment() {
  const file = attach.value
  if (!file || sending.value || attachReading.value) return
  if (!currentSession.value) await createSession()
  attach.value = null // 附件即操作：发出即消费（AC8：移除或发送后回到普通聊天）
  const card = reactive({
    fileName: file.name,
    fileSize: file.size,
    initMode: !!file.initMode,
    src: file,           // FileReader 读到的那份字节原样留着，提交时再传一次同一份（不重新加工）
    preview: null,
    result: null,
    error: '',
    fixedText: '',       // = 后端 fixedText（修复稿全文；未修出东西时 = 原文）
    confirmed: false,    // 破坏性/删除的卡片内二次确认勾选（等价向导那一条，不是新审批机制）
    previewing: true,
    committing: false,
    phase: 'previewing'  // previewing → ready → done / failed
  })
  apiImport.value = card
  await nextTick()
  scrollToBottom()
  try {
    const pv = await PreviewApiDocImport(attachRequestOf(file, false))
    card.preview = pv || null
    card.fixedText = pv?.fixedText || ''
    card.phase = pv ? 'ready' : 'failed'
    if (!pv) card.error = '后端没有返回预览结果'
  } catch (err) {
    card.error = String(err) // 后端中文短句原样展示（档位冲突 / 空文本等都在这一路）
    card.phase = 'failed'
  }
  card.previewing = false
  await nextTick()
  scrollToBottom()
}

/** 确认导入：零 E-* 才可能走到这里；needConfirm 必须先勾卡片内的确认勾选（D16 同款）。
 *  提交入参与预览同源（同一份 docText + 同一组勾选位），后端再跑一次同一个 Repair，
 *  确定性保证「预览看到的 = 库里落下的」（AC6/AC7）。 */
async function commitAttachmentImport() {
  const card = apiImport.value
  if (!card || card.previewing || card.committing || card.result) return
  if (!canCommitAttach.value) {
    if (attachBlockReason.value) message.warning(attachBlockReason.value)
    return
  }
  card.committing = true
  try {
    const res = await CommitApiDocImport(attachRequestOf(card.src, true))
    card.result = res || null
    if (res?.fixedText) card.fixedText = res.fixedText
    card.phase = res ? 'done' : 'failed'
    if (!res) card.error = '后端没有返回导入结果'
    else message.success(res.message || '导入完成')
  } catch (err) {
    // 服务层闸门拒收（剩余 E-* / 非空项目 init / 未确认 / 档位冲突）：一行都没写
    card.error = String(err)
    card.phase = 'failed'
    message.error(String(err))
  } finally {
    card.committing = false
    await nextTick()
    scrollToBottom()
  }
}

// ---- 结果卡的派生状态（只读当前卡片）----
const attachCard = computed(() => apiImport.value)
const attachPreview = computed(() => apiImport.value?.preview || null)
const attachIssues = computed(() => attachPreview.value?.issues || [])
const attachErrors = computed(() => attachIssues.value.filter(i => (i.level || '') === 'E'))
const attachWarnings = computed(() => attachIssues.value.filter(i => (i.level || '') === 'W'))
const attachFixes = computed(() => attachPreview.value?.fixes || [])
const attachReviewCount = computed(() => attachFixes.value.filter(f => f.needsReview).length)
const attachNotes = computed(() => attachPreview.value?.notes || [])
const attachResultNotes = computed(() => apiImport.value?.result?.notes || [])
const attachHasFix = computed(() => attachFixes.value.length > 0 && !!apiImport.value?.fixedText)

/** 提交闸（前端只是把后端已有的闸显式化，后端同样会拒——不是"前端不显示按钮"那种假闸） */
const canCommitAttach = computed(() => {
  const c = apiImport.value
  const p = c?.preview
  if (!c || c.previewing || c.committing || c.result) return false
  if (!p || !p.ok) return false
  if (p.needConfirm && !c.confirmed) return false
  return true
})

/** 按钮灰掉时必须说清为什么灰（绝不只变灰不解释） */
const attachBlockReason = computed(() => {
  const c = apiImport.value
  const p = c?.preview
  if (!c) return ''
  if (c.error) return '后端已拒收，原因见上面的中文提示；一行都没写入。'
  if (!p) return '还没拿到预览结果。'
  if (attachErrors.value.length) return `仍有 ${attachErrors.value.length} 个 E-* 阻断问题：未导入，请修正文档后重新发送。`
  if (p.initBlocked) return '基线导入被后端闸门拦住：init 只允许空项目。请先清项目，或去「接口文档」视图的导入向导执行。'
  if (!p.ok) return attachNotes.value.length ? '已被后端闸门拦下，原因见上方中文提示。' : '当前预览结果不允许提交。'
  if (p.needConfirm && !c.confirmed) return '本次导入含接口删除或破坏性变更，勾选确认后才能写入。'
  return ''
})

const attachCommitLabel = computed(() =>
  attachPreview.value?.needConfirm ? '我已核对，确认写入（删除/破坏性）' : '确认导入')

/** 修正稿另存为 *.fixed.md：绝不与原名相同（D7 不写回原文件，静默改用户磁盘不可回退）。
 *  文件名来自用户选的本地文件，这里只做字符串拼接给 Blob 下载属性用，不落任何真实路径。 */
function fixedNameOf(fileName) {
  const base = String(fileName || 'api-doc').replace(/\.(md|markdown)$/i, '')
  return `${base || 'api-doc'}.fixed.md`
}

/** Blob 下载（桌面 WebView2 与浏览器都支持）：失败降级为弹窗 + 复制（D7/AC6） */
function downloadFixedText() {
  const card = apiImport.value
  const text = card?.fixedText || ''
  if (!text) return
  try {
    const blob = new Blob([text], {type: 'text/markdown;charset=utf-8'})
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = fixedNameOf(card.fileName)
    a.rel = 'noopener'
    document.body.appendChild(a)
    a.click()
    a.remove()
    // 立刻 revoke 会让部分浏览器/WebView 取消还没起步的下载，延后一拍再回收
    setTimeout(() => URL.revokeObjectURL(url), 2000)
    message.success(`修正稿已导出为 ${fixedNameOf(card.fileName)}（原文件未被改写）`)
  } catch {
    fixedModalText.value = text
    fixedModalShow.value = true
    message.warning('当前环境不允许下载，已弹出修正稿全文供复制')
  }
}

/** 复制修正稿：走与代码块「复制」同一个 copyText（navigator.clipboard → execCommand 双路） */
async function copyFixedText() {
  const card = apiImport.value
  const text = card?.fixedText || ''
  if (!text) return
  if (await copyText(text)) {
    message.success(`修正稿已复制到剪贴板（${text.length} 字符）`)
  } else {
    fixedModalText.value = text
    fixedModalShow.value = true
    message.warning('剪贴板不可用，已弹出修正稿全文供手动复制')
  }
}

// ---------------- 数据加载 ----------------

/** 拉取会话列表；selectFirst=true 且当前无选中时自动打开第一个会话 */
async function loadSessions(selectFirst = true) {
  // await = 等 Promise 完成（≈ future.get()，但不会阻塞线程）
  sessions.value = (await ListSessions()) || []
  if (!currentSession.value && selectFirst && sessions.value.length > 0) {
    await selectSession(sessions.value[0])
  }
}

/** 拉取 Agent 列表（头部下拉框数据源） */
async function loadAgents() {
  agents.value = (await ListAgents()) || []
}

// ---------------- 输入建议浮层（/ 触发技能软路由、@ 触发智能体强制路由）----------------
// 触发规则与后端解析对齐：仅当输入首字符是 / 或 @、且触发段尚未出现空白时弹出
// （后端 parseSkillPrefix / parseAgentPrefix 都要求"行首触发 + 空白分隔正文"）。
// / 数据源 = 当前会话 Agent 已装配的技能（ListAgentsDetail 明细的 skills，含描述）；
// @ 数据源 = 全部智能体（agents，头部下拉框同源）。选中只插入前缀文本，
// 路由本身仍由后端发送时解析——浮层是补全器，不是新链路。

const agentDetails = ref([])         // Agent 明细列表（含已装配技能 name/description）
const inputRef = ref(null)           // n-input 组件引用（选中后恢复焦点/光标用）
const suggIndex = ref(0)             // 浮层高亮项下标（↑↓ 循环导航）
const suggDismissed = ref(false)     // Esc 临时关闭标记（触发段失效后自动复位）

/** 拉取 Agent 明细（/ 浮层数据源）。装配只能在智能体视图里改，
 *  故挂载时 + 离开智能体视图时刷新即可；失败不阻塞输入（浮层退化为空态）。 */
async function loadAgentDetails() {
  try {
    agentDetails.value = (await ListAgentsDetail()) || []
  } catch {
    agentDetails.value = []
  }
}

// 当前会话 Agent 已装配的技能（computed：会话切换 / Agent 变更后自动跟随）
const currentAgentSkills = computed(() => {
  const d = agentDetails.value.find(a => a.id === currentAgentId.value)
  return d?.skills || []
})

// 触发解析：行首触发符 + 触发段（\S* 保证段内无空白；输入正文后浮层自动关闭）
const suggTrigger = computed(() => {
  const m = /^([/@])(\S*)$/.exec(input.value)
  if (!m) return null
  return {kind: m[1] === '/' ? 'skill' : 'agent', query: m[2].toLowerCase()}
})

// 过滤后的建议条目：名字/描述模糊匹配（不区分大小写；@ 智能体附带匹配 id）
const suggItems = computed(() => {
  const trig = suggTrigger.value
  if (!trig) return []
  const hit = (s) => !trig.query || (s || '').toLowerCase().includes(trig.query)
  if (trig.kind === 'skill') {
    return currentAgentSkills.value.filter(s => hit(s.name) || hit(s.description))
  }
  return agents.value.filter(a => hit(a.name) || hit(a.id))
})

// 浮层可见 = 触发段有效且未被 Esc 关闭
const suggVisible = computed(() => !!suggTrigger.value && !suggDismissed.value)

// 空列表提示文案（不阻塞输入：此时 Enter 仍是发送）
const suggEmptyText = computed(() => {
  const trig = suggTrigger.value
  if (!trig) return ''
  if (trig.kind === 'skill') return currentAgentSkills.value.length ? '无匹配技能' : '当前智能体未装配技能'
  return '无匹配智能体'
})

// 输入变化：高亮回到第一项；触发段失效（如已选中插入空格）时复位 Esc 关闭标记
watch(input, () => {
  suggIndex.value = 0
  if (!suggTrigger.value) suggDismissed.value = false
})

// 列表异步变化（Agent 明细刷新等）导致条目数收缩时，高亮下标可能越界——
// 越界后 items[suggIndex] 为 undefined，Enter/Tab 会静默失灵，故在此复位
watch(suggItems, (items) => {
  if (suggIndex.value >= items.length) suggIndex.value = 0
})

// 高亮变化时把条目滚进可视区（浮层内部滚动，键盘导航不丢焦点项）
watch(suggIndex, () => {
  if (!suggVisible.value) return
  nextTick(() => {
    document.querySelector('.sugg-item.active')?.scrollIntoView({block: 'nearest'})
  })
})

/** 选中建议：把行首触发段（如 "/pg"、"@运"）整体替换为 "/技能名 " 或 "@名字 "，
 *  光标置尾等待输入正文。浮层只在"整段无空白"时打开，故直接整体替换即可。 */
function selectSuggestion(item) {
  const trig = suggTrigger.value
  if (!trig || !item) return
  input.value = (trig.kind === 'skill' ? '/' : '@') + item.name + ' '
  nextTick(() => {
    const el = inputRef.value?.textareaElRef // n-input(type=textarea) 暴露的内部元素
    if (el) {
      el.focus()
      el.setSelectionRange(input.value.length, input.value.length)
    }
  })
}

/** 浮层键盘导航（挂在 n-input 的 keydown 上；Enter 由 onEnterOrSend 单独接管） */
function onSuggKeydown(e) {
  if (!suggVisible.value) return
  const items = suggItems.value
  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault()
      if (items.length) suggIndex.value = (suggIndex.value + 1) % items.length // 循环
      break
    case 'ArrowUp':
      e.preventDefault()
      if (items.length) suggIndex.value = (suggIndex.value - 1 + items.length) % items.length
      break
    case 'Tab':
      if (items.length) {
        e.preventDefault() // 浮层打开时 Tab = 选中，不切换焦点
        selectSuggestion(items[suggIndex.value])
      }
      break
    case 'Escape':
      e.preventDefault()
      suggDismissed.value = true
      break
  }
}

/** Enter（无修饰键）：浮层打开且有可选项时 = 选中建议，绝不触发发送；否则照常发送。
 *  输入法组合态防御：中文候选词按回车"上屏"时，部分浏览器（Firefox/Safari）会派发
 *  key=Enter 且 isComposing=true 的 keydown——此时直接放行（不 preventDefault、
 *  不选中不发送），把回车还给输入法完成上屏；正常路径手动 preventDefault 阻止换行
 *  （等价于原 .prevent 修饰符——修饰符版会在处理函数之前无条件 prevent，
 *  组合态就守不住了，故改在函数内按 isComposing 分流）。
 *  Chromium/WebView2 组合态派发的是 key=Process，本就进不了 .enter 分支，双保险 */
function onEnterOrSend(e) {
  if (e?.isComposing || e?.keyCode === 229) return
  e?.preventDefault() // 阻止 textarea 默认换行（= 原 @keydown.enter.exact.prevent 的语义）
  if (suggVisible.value && suggItems.value.length) {
    selectSuggestion(suggItems.value[suggIndex.value])
    return
  }
  send()
}

// ---------------- 会话操作 ----------------

/** 新建会话并立即打开 */
async function createSession() {
  const sess = await NewSession()
  await loadSessions(false)   // 刷新侧栏（不自动选中，下一行手动选）
  await selectSession(sess)
}

/** 打开某个会话：切回聊天视图、设为当前、加载历史消息、滚到底部。
 *  所有进入会话的路径（会话项点击/新建会话/初始自动选中）都经过这里，
 *  选会话即语义明确地要看聊天，故在此统一重置三态视图状态 */
async function selectSession(sess) {
  view.value = 'chat'
  currentSession.value = sess
  streaming.value = null       // 切会话时丢弃旧的流式状态（若旧回合仍在跑，
  // 看门狗保持守卫：任何会话的帧都会重置它，回合 RPC 结束时 finally 清除）
  approval.value = null        // 审批卡片属于会话：切换后不再显示（帧按 sessionId 过滤）
  apiApproval.value = null     // 接口文档导入审批卡片同理（两条链路各自独立清空）
  // 代码块折叠态同理是会话内视图态（R3b）：整个 Set 换新对象（Vue 对 Set 的
  // clear/add/delete 都能触发依赖，但换对象最直白，也顺带丢弃上一个会话的死 key）
  expandedBlocks.value = new Set()
  // 附件与它的结果卡也是组件内视图态（R7/D9）：切会话即丢弃，不落库、不跨会话串台
  attach.value = null
  attachReading.value = false
  apiImport.value = null
  dragOver.value = false
  fixedModalShow.value = false
  fixedModalText.value = ''
  deployRunning.value = false
  messages.value = ((await GetMessages(sess.id)) || []).map(normalize)
  await nextTick()             // 等消息渲染进 DOM
  scrollToBottom()
}

/** 把 Go 返回的消息对象裁剪成视图需要的形状 */
function normalize(m) {
  return {role: m.role, content: m.content}
}

/** 删除会话；删的是当前会话则清空主区 */
async function removeSession(sess) {
  await DeleteSession(sess.id)
  if (currentSession.value?.id === sess.id) {
    currentSession.value = null
    messages.value = []
  }
  await loadSessions(true)
}

// ---------------- 会话标题：行内改名 ----------------
// 标题由后端在首条消息时截断生成；手动改名后标题不再是"新会话"，
// 占位逻辑天然不会覆盖。前端只负责发起改名并就地刷新。

const editingId = ref(null)    // 正在行内改名的会话 id；null = 无
const editingText = ref('')    // 改名输入框的临时文本

// 自定义指令 v-auto-focus：元素挂载后聚焦内部 input 并全选文本。
// n-input 用 v-if 动态出现，原生 autofocus 对"动态插入"的元素不总生效，故手动聚焦。
// （<script setup> 里以 v 开头的变量会自动注册为同名指令：vAutoFocus → v-auto-focus）
const vAutoFocus = {
  mounted: (el) => {
    const field = el.querySelector('input, textarea') || el
    field.focus()
    if (field.select) field.select()
  }
}

/** 就地把某会话的标题更新到列表与 currentSession。
 *  createSession 后 currentSession 可能与列表项不是同一对象引用（NewSession 返回体
 *  vs 回拉列表项），故按 id 分别更新，两条数据流都覆盖到。 */
function applyTitle(id, title) {
  const item = sessions.value.find(x => x.id === id)
  if (item) item.title = title
  if (currentSession.value?.id === id) currentSession.value.title = title
}

/** ✏️：进入该会话的行内改名态，预填当前标题 */
function startRename(s) {
  editingId.value = s.id
  editingText.value = s.title || ''
}

/** 取消改名（Esc）：直接退出编辑态，不发请求 */
function cancelRename() {
  editingId.value = null
  editingText.value = ''
}

/** Enter 保存（输入法组合态防御，与 onEnterOrSend 同款思路）：
 *  中文候选词按回车"上屏"时部分浏览器派发 key=Enter 且 isComposing=true，
 *  此时放行不保存，把回车还给输入法；正常回车才提交改名。 */
function onRenameEnter(s, e) {
  if (e?.isComposing || e?.keyCode === 229) return
  commitRename(s)
}

/** 保存改名（Enter / 失焦）：非空且有变化才调 RenameSession，成功就地更新。
 *  幂等守卫：Enter 保存后输入框卸载会再触发 blur，用 editingId 命中判断防重复提交。
 *  失败恢复：保存失败（后端校验/落库错误）时把输入态还原——重挂输入框并回填
 *  用户刚敲的文本，让其能直接修正重试，而不是丢失已输入内容从头再来。 */
async function commitRename(s) {
  if (editingId.value !== s.id) return   // 已退出编辑态（Enter/Esc/切到别的项）：不重复提交
  const title = editingText.value.trim()
  editingId.value = null                 // 先退出编辑态（也阻断随后的 blur 再次进入）
  if (!title || title === s.title) return // 空标题或未变化：不发请求（后端也会拒绝空标题）
  try {
    await RenameSession(s.id, title)
    applyTitle(s.id, title)
  } catch (err) {
    message.error(String(err))           // 后端中文校验错误（如"标题不能为空"）直接展示
    // 还原输入态：先回填文本再重挂输入框（editingId 命中即渲染 n-input，
    // v-auto-focus 会自动聚焦全选），用户无需重新点开 ✏️ 就能改。
    editingText.value = title
    editingId.value = s.id
  }
}


/** @ 前缀路由在后端切掉了会话绑定的 Agent——回合结束后重拉会话列表，
 *  把本地 currentSession 换成新对象，头部显示与 / 技能建议（都依赖 agentId）才不会停留在旧 Agent */
async function syncSessionAgent() {
  if (!currentSession.value) return
  sessions.value = (await ListSessions()) || []
  const fresh = sessions.value.find(s => s.id === currentSession.value.id)
  if (fresh) currentSession.value = fresh
}

/** 消息列表滚到底部 */
function scrollToBottom() {
  if (listRef.value) {
    listRef.value.scrollTo({top: listRef.value.scrollHeight, behavior: 'auto'})
  }
}

// ---------------- 发送消息 ----------------

// 回合代号：每次 send 递增。看门狗解锁后用户可能立刻重发新回合，
// 此时旧回合 RPC 才姗姗返回——它的 finally 绝不能清掉新回合的看门狗、
// 也不能覆盖新回合的 sending 锁，所以清理前先核对代号。
let turnSeq = 0

/** 发送：乐观上屏 → 调 Go → 流式增量经事件回来 → done 事件收尾。
 *  带附件时走另一条路（D8）：不拼 prompt、不调 SendMessage、不起 LLM 回合，
 *  直接触发「体检 + 确定性修复 + 导入」并渲染结果卡。 */
async function send() {
  if (attach.value) {
    await sendWithAttachment()
    return
  }
  const text = input.value.trim()
  if (!text || sending.value) return   // 空消息或上一回合未结束：直接忽略
  if (!currentSession.value) {
    await createSession()              // 没有会话就自动建一个
  }
  input.value = ''
  messages.value.push({role: 'user', content: text}) // 用户消息立即上屏
  sending.value = true
  const mySeq = ++turnSeq              // 本回合代号（收尾时用它识别"还是不是本回合"）
  stopped.value = false                // 新回合：复位停止/报错去重标记
  errShown.value = false
  streaming.value = {content: '', tools: []}         // 出现流式气泡
  kickWatchdog()                       // 启动看门狗：此后每收到一帧会重置它
  await nextTick()
  scrollToBottom()
  try {
    // 阻塞直到整个 Agent 回合结束；期间 chat:delta 事件不断填充气泡
    await SendMessage(currentSession.value.id, text)
  } catch (err) {
    // Go 返回的 error 会变成 Promise reject；若 done 帧已报过错（或用户主动停止）
    // 则不再重复提示——一个回合最多弹一次错。
    // 代号核对：旧回合的迟到失败不得干扰已切换会话/已重发的新回合
    if (mySeq === turnSeq) {
      if (!stopped.value) reportTurnError(String(err))
      streaming.value = null
    }
  } finally {
    // 回合结束（正常或异常）都清除看门狗并解锁（≈ try-finally）
    if (mySeq === turnSeq) {
      clearWatchdog()
      sending.value = false
      // @ 前缀路由在后端切了会话 Agent：回拉同步本地 agentId（先解锁再同步，不阻塞 UI）
      if (text.startsWith('@')) await syncSessionAgent()
    }
  }
}

// ---------------- 聊天流回调（Go → 前端推送）----------------
// 三个回调都先校验 sessionId：只处理当前会话的事件，避免串台。

/** chat:delta：模型吐出一小段文字，追加进流式气泡 */
function onDelta(payload) {
  if (!currentSession.value || payload.sessionId !== currentSession.value.id) return
  if (stopped.value) return  // 本地点过停止：忽略后端残余帧，防止气泡复活
  if (!streaming.value) streaming.value = {content: '', tools: []}
  streaming.value.content += payload.content
  nextTick(scrollToBottom)  // 内容变长后跟随滚动
}

/** chat:tool：技能开始/结束，维护气泡上的状态标签（执行中…/完成/失败） */
function onTool(payload) {
  if (!currentSession.value || payload.sessionId !== currentSession.value.id) return
  if (stopped.value) return  // 本地点过停止：忽略后端残余帧
  if (!streaming.value) streaming.value = {content: '', tools: []}
  if (payload.status === 'start') {
    streaming.value.tools.push({skill: payload.skill, status: 'running'})
  } else {
    // findLast：找最后一个同名技能条目（同一技能可能被调多次）
    const t = streaming.value.tools.findLast(t => t.skill === payload.skill)
    if (t) t.status = payload.failed ? 'failed' : 'done'
  }
}

/** chat:done：回合结束。成功→最终回答入消息列表；失败→红色提示 */
function onDone(payload) {
  if (!currentSession.value || payload.sessionId !== currentSession.value.id) return
  clearWatchdog()           // 回合结束，看门狗使命完成
  if (payload.error) {
    // 用户主动点过"停止"：错误帧属预期收尾，静默处理不重复报错；
    // 其余错误经 reportTurnError 去重（可能与 RPC reject 二选一）
    if (!stopped.value) reportTurnError(payload.error)
  } else if (payload.content) {
    messages.value.push({role: 'assistant', content: payload.content})
  }
  streaming.value = null   // 流式气泡消失（已被停止按钮清空时此赋值幂等无害）
  nextTick(scrollToBottom)
}

// 聊天流连接（模块级变量：生命周期钩子间共享）。
// 帧格式与 Go 端 emitChat 对应：{event: 'chat:delta'|'chat:tool'|'chat:done'|'deploy:*'|'api:*'|'session:title', payload: {...}}
let chatStream = null

/** 流帧分发：按事件名路由到对应回调。
 *  看门狗在分发前统一重置，且不分 sessionId——看门狗守护的是"传输链路还活着"：
 *  别的会话的帧同样证明链路畅通（本会话回合自身由后端双层超时兜底）。
 *  这样"回合进行中切走会话"不会导致看门狗误报。
 *  未知事件名不做任何处理（无副作用）；部署帧与聊天帧同样重置看门狗。 */
function onStreamFrame(ev) {
  const frame = ev.data // JSONStream 已把帧解析成对象
  if (!frame || !frame.event) return
  kickWatchdog()        // 收到任一帧 = 连接活着（内部仅 sending 时才真正挂表）
  switch (frame.event) {
    case 'chat:delta': onDelta(frame.payload); break
    case 'chat:tool': onTool(frame.payload); break
    case 'chat:done': onDone(frame.payload); break
    case 'deploy:approval': onDeployApproval(frame.payload); break
    case 'deploy:progress': onDeployProgress(frame.payload); break
    case 'deploy:done': onDeployDone(frame.payload); break
    case 'api:approval': onApiDocApproval(frame.payload); break
    case 'api:done': onApiDocDone(frame.payload); break
    case 'session:title': onSessionTitle(frame.payload); break
  }
}

/** session:title：AI 总结出的新标题（回合结束后异步生成，几秒后到达）。
 *  只按 sessionId 更新侧栏对应项的标题（不整表重拉 ListSessions、不动消息区、
 *  不动 streaming——标题帧与回合内容完全无关，重拉列表还会覆盖本地编辑态）。
 *  别的会话的帧同样处理：侧栏是全会话共用的列表。 */
function onSessionTitle(payload) {
  if (!payload || !payload.sessionId || !payload.title) return
  applyTitle(payload.sessionId, payload.title)
}

// ---------------- 生命周期 ----------------

// 组件挂载后：打开聊天流 + 初始化数据（≈ @PostConstruct）
onMounted(async () => {
  chatStream = JSONStream('chat')
  chatStream.onmessage = onStreamFrame
  await loadAgents()
  await loadAgentDetails()
  await loadSessions(true)
})

// 组件销毁前：关闭聊天流 + 清除看门狗，防止连接/计时器泄漏（≈ @PreDestroy）
onBeforeUnmount(() => {
  clearWatchdog()
  if (chatStream) {
    chatStream.close()
    chatStream = null
  }
})

// 设置弹窗关闭后刷新 Agent 列表（用户可能在设置里改了配置）
watch(showSettings, async (v) => {
  if (!v) await loadAgents()
})

// 视图切换收尾：回到聊天页滚到底部（离开期间到达的帧未能驱动跟随滚动）；
// 离开智能体视图后刷新头部下拉的 Agent 列表（名称可能已被编辑）与 Agent 明细
// （技能装配可能已被修改——/ 建议浮层的数据源）；
// 会话绑定无需处理——会话只存 agentId，Runner 每回合实时读装配
watch(view, (v, oldV) => {
  if (v === 'chat') nextTick(scrollToBottom)
  if (oldV === 'agents' && v !== 'agents') {
    loadAgents()
    loadAgentDetails()
  }
})
</script>

<template>
  <!-- n-layout has-sider：左右分栏布局容器 -->
  <n-layout has-sider class="app-layout">

    <!-- ============ 左侧：会话侧边栏 ============ -->
    <!-- 280px：16:9 宽屏下稍宽，容纳更长的会话标题；主区 flex 自适应剩余宽度 -->
    <n-layout-sider bordered :width="280" class="sider">
      <div class="sider-head">
        <div class="brand">DevCraft</div>
        <n-button size="small" type="primary" ghost block @click="createSession">+ 新会话</n-button>
        <!-- @click 是 v-on:click 的缩写：绑定点击事件 -->
      </div>

      <!-- 会话列表：v-for 循环渲染（≈ for-each），:key 帮 Vue 识别每项身份。
           :class 绑定对象语法：active 类在条件为真时生效 -->
      <div class="session-list">
        <div
          v-for="s in sessions" :key="s.id"
          class="session-item"
          :class="{active: currentSession?.id === s.id}"
          @click="selectSession(s)"
        >
          <!-- 行内改名：editingId 命中时标题变输入框（Enter/失焦保存、Esc 取消）。
               v-auto-focus 挂载后自动聚焦全选；@click.stop 防止冒泡触发 selectSession -->
          <n-input
            v-if="editingId === s.id"
            v-model:value="editingText"
            v-auto-focus
            size="tiny"
            class="session-edit"
            @click.stop
            @keydown.enter="onRenameEnter(s, $event)"
            @keyup.esc="cancelRename"
            @blur="commitRename(s)"
          />
          <span v-else class="session-title">{{ s.title || '新会话' }}</span>
          <!-- {{ }} 双花括号 = 文本插值（≈ 模板引擎的 ${}），自动 HTML 转义 -->

          <!-- 悬停操作组（不挤占标题布局）：✏️ 改名 / ✕ 删除。
               .stop 阻止冒泡到 selectSession -->
          <span v-if="editingId !== s.id" class="session-actions">
            <n-button quaternary size="tiny" class="session-act" title="重命名" @click.stop="startRename(s)">✏️</n-button>
            <n-button quaternary size="tiny" class="session-del" title="删除会话" @click.stop="removeSession(s)">✕</n-button>
          </span>
        </div>
        <!-- v-if：条件渲染，列表为空时显示占位提示 -->
        <n-empty v-if="sessions.length === 0" description="暂无会话" size="small" class="empty"/>
      </div>

      <div class="sider-foot">
        <!-- 三个入口按钮统一结构：固定宽度图标列 + 左对齐文字列（flex 布局），
             图标纵向对齐成一列、文字也对齐成一列；保留 block quaternary 观感 -->
        <n-button block quaternary class="sider-btn" @click="showSettings = true">
          <span class="sider-btn-icon">⚙</span>
          <span class="sider-btn-label">设置</span>
        </n-button>
        <!-- 技能：右侧主区切换为技能管理视图（卡片网格占满视图；详情为弹窗） -->
        <n-button
          block quaternary class="sider-btn"
          :class="{active: view === 'skills'}"
          @click="view = 'skills'"
        >
          <span class="sider-btn-icon">🧩</span>
          <span class="sider-btn-label">技能</span>
        </n-button>
        <!-- 智能体：右侧主区切换为智能体管理视图（卡片网格占满视图；详情为弹窗） -->
        <n-button
          block quaternary class="sider-btn"
          :class="{active: view === 'agents'}"
          @click="view = 'agents'"
        >
          <span class="sider-btn-icon">🤖</span>
          <span class="sider-btn-label">智能体</span>
        </n-button>
        <!-- 接口文档：右侧主区切换为接口文档视图（「接口」三栏浏览 + 「变更事件」列表与详情 + 导入向导；
             详情全部内联在栏内，不用弹窗） -->
        <n-button
          block quaternary class="sider-btn"
          :class="{active: view === 'apidocs'}"
          @click="view = 'apidocs'"
        >
          <span class="sider-btn-icon">📄</span>
          <span class="sider-btn-label">接口文档</span>
        </n-button>
      </div>
    </n-layout-sider>

    <!-- ============ 右侧：主区（四态视图：聊天 / 技能 / 智能体 / 接口文档） ============ -->
    <n-layout class="main">

      <!-- ---- chat 视图：v-show 保持 DOM 常驻（而非 v-if 卸载）。
           切去技能/智能体视图期间，流式帧照常写入消息与气泡状态；
           切回时消息不丢、气泡状态正确、滚动位置保留 ---- -->
      <div v-show="view === 'chat'" class="chat-view">

      <!-- 头部：当前会话绑定的 Agent（只读展示）。切换不再用下拉框——
           在消息里输入 "@名字 正文" 强制路由（输入 @ 有补全弹窗），
           回合结束后 syncSessionAgent 回拉会话，此处显示与 / 技能列表随之同步 -->
      <div class="chat-head">
        <span v-if="currentSession" class="hint">
          当前智能体：{{ currentAgentName || '未绑定（输入 @名字 可切换）' }}
        </span>
        <span v-else class="hint">创建或选择一个会话开始</span>
      </div>

      <!-- 中部：消息流 -->
      <div ref="listRef" class="chat-list">
        <!-- ref="listRef" 把这个 DOM 元素注入到脚本里的 listRef 变量 -->

        <!-- 历史消息：user = 右对齐暗色气泡；assistant = 靠左文档卡片（D1/D4）。
             卡片头只有角色名（D4 第二轮修订：整条消息的复制按钮已移除，
             复制粒度下沉到每个代码块自己的标题栏上，见 lib/md-blocks.js） -->
        <div v-for="(m, i) in messages" :key="i" class="msg" :class="m.role">
          <div v-if="m.role === 'assistant'" class="msg-head">
            <span class="msg-role">DevCraft</span>
          </div>
          <div v-else class="msg-role">{{ m.role === 'user' ? '我' : 'DevCraft' }}</div>
          <!-- v-html：把渲染后的 HTML 直接插入（用于 Markdown）。
               注意：只用于可信内容（本应用的 LLM 回复），不可信输入会引发 XSS。
               renderedMessages[i] = renderMarkdown + 代码块外壳装饰（assistant 才装饰）。
               @click 挂在容器上做事件委托：v-html 里的块标题栏按钮（复制/展开收起）
               是浏览器解析出来的原生节点，Vue 绑不上，只能由父容器接管（D9.3）。
               用户气泡走同一处理也没副作用——那里面没有 [data-act] 节点，直接 return。-->
          <div class="msg-body" @click="onBlockClick" v-html="renderedMessages[i]"></div>
        </div>

        <!-- 流式气泡：sending 期间显示，内容随 chat:delta 增长。
             与正式 assistant 消息同款文档卡片，但按 D6 流式期间**不做块装饰**
             （不注入标题栏、不折叠）：每个 delta 都会重跑渲染，若此时套外壳，
             DOM 整段重排、折叠态随行数增长反复跳动、按钮一闪一闪。
             done 帧把内容转成正式消息后，上面的 v-for 才走装饰路径。-->
        <div v-if="streaming" class="msg assistant">
          <div class="msg-head">
            <span class="msg-role">DevCraft</span>
          </div>
          <!-- 技能执行状态标签（n-tag 颜色随状态变化）。
               位置决策：保留在卡片内、角色名与正文之间（与原竖排顺序完全一致），
               不合进 msg-head 行——标签可能多条换行（flex-wrap），塞头部会把单行
               角色名撑歪；挪到卡片外上方则与所属正文割裂。类名/结构/行为未动（R4/AC7） -->
          <div class="msg-tools" v-if="streaming.tools.length">
            <n-tag
              v-for="(t, i) in streaming.tools" :key="i" size="small"
              :type="t.status === 'failed' ? 'error' : t.status === 'done' ? 'success' : 'info'"
            >
              {{ t.skill }} {{ t.status === 'running' ? '执行中…' : t.status === 'failed' ? '失败' : '完成' }}
            </n-tag>
          </div>
          <!-- 这里仍是裸 renderMarkdown：不装饰、不绑 @click（D6），
               装饰与事件委托只在上面正式消息那条路径上 -->
          <div class="msg-body" v-html="renderMarkdown(streaming.content)"></div>
        </div>

        <n-empty v-if="messages.length === 0 && !streaming" description="试试：查看所有容器" class="empty"/>
      </div>

      <!-- 部署审批卡片：deploy:approval 帧到达时出现。
           展示替换参数后的完整命令清单（等宽样式），用户人工批准才执行。 -->
      <div v-if="approval" class="deploy-card">
        <div class="deploy-head">
          <n-tag type="warning" size="small">部署审批</n-tag>
          <strong class="deploy-title">流程「{{ approval.flowName }}」</strong>
          <span class="deploy-meta">目标: {{ approval.target === 'ssh' ? 'SSH 远程主机' : '本机' }}</span>
          <n-tag
            v-if="approval.state !== 'pending'"
            :type="approval.state === 'approved' ? 'success' : approval.state === 'rejected' ? 'error' : 'default'"
            size="small"
          >
            {{ {approved: '已批准', rejected: '已拒绝', expired: '已过期', approving: '批准中…'}[approval.state] || approval.state }}
          </n-tag>
        </div>
        <div v-if="approval.description" class="deploy-desc">{{ approval.description }}</div>
        <div v-if="Object.keys(approval.params).length" class="deploy-params">
          <span v-for="(v, k) in approval.params" :key="k" class="deploy-param">{{ k }} = {{ v }}</span>
        </div>
        <div class="deploy-cmds">
          <div v-for="(c, i) in approval.commands" :key="i" class="deploy-cmd">{{ i + 1 }}. {{ c }}</div>
        </div>
        <div class="deploy-actions">
          <span class="deploy-hint">请核对命令清单；批准后立即执行，{{ approval.expiresInMin }} 分钟内未确认自动过期</span>
          <n-button
            size="small" type="error" secondary
            :disabled="approval.state !== 'pending'"
            @click="rejectDeploy"
          >拒绝</n-button>
          <n-button
            size="small" type="primary"
            :disabled="approval.state !== 'pending'"
            :loading="approval.state === 'approving'"
            @click="approveDeploy"
          >批准执行</n-button>
        </div>
      </div>

      <!-- 接口文档导入审批卡片：api:approval 帧到达时出现（与上面的部署卡片同属
           .deploy-* 样式族，复用类名 → 视觉一致且零新增 CSS）。
           摘要行是后端已掩码的「逐接口变更」文本，一律当纯文本渲染（零 v-html、
           不走 marked）——这里展示的是外部导入文档派生内容，不能进 HTML 管道。 -->
      <div v-if="apiApproval" class="deploy-card">
        <div class="deploy-head">
          <n-tag type="warning" size="small">接口文档导入审批</n-tag>
          <strong class="deploy-title">项目「{{ apiApproval.project }}」</strong>
          <span class="deploy-meta">
            {{ apiApproval.mode === 'init' ? '初始化模式（不产生变更事件）' : '增量导入' }}<template v-if="apiApproval.nextSeq"> · 将生成 v{{ apiApproval.nextSeq }}</template>
          </span>
          <n-tag
            :type="apiApproval.breaking === 1 ? 'error' : apiApproval.breaking === 2 ? 'warning' : 'success'"
            size="small"
          >
            {{ apiApproval.breaking === 1 ? '含破坏性变更' : apiApproval.breaking === 2 ? '需人工确认' : '无破坏性' }}
          </n-tag>
          <n-tag
            v-if="apiApproval.state !== 'pending'"
            :type="apiApproval.state === 'approved' ? 'success' : apiApproval.state === 'rejected' ? 'error' : 'default'"
            size="small"
          >
            {{ {approved: '已批准', rejected: '已拒绝', expired: '已过期', approving: '批准中…'}[apiApproval.state] || apiApproval.state }}
          </n-tag>
        </div>
        <div v-if="apiApproval.docRef" class="deploy-desc">来源文档: {{ apiApproval.docRef }}</div>
        <div class="deploy-params">
          <span class="deploy-param">新增 {{ apiApproval.stats.added || 0 }}</span>
          <span class="deploy-param">修改 {{ apiApproval.stats.updated || 0 }}</span>
          <span class="deploy-param">删除 {{ apiApproval.stats.removed || 0 }}</span>
          <span class="deploy-param">恢复 {{ apiApproval.stats.restored || 0 }}</span>
        </div>
        <div class="deploy-cmds">
          <!-- 逐接口变更摘要：批准前必须让人看见"要删哪个/改哪个字段"，
               这是点不点批准的唯一依据（PRD D9/D16） -->
          <div v-for="(line, i) in apiApproval.summary" :key="i" class="deploy-cmd">{{ i + 1 }}. {{ line }}</div>
        </div>
        <div class="deploy-actions">
          <span class="deploy-hint">请核对将写入与将删除的接口；批准后立即导入，{{ apiApproval.expiresInMin }} 分钟内未确认自动过期</span>
          <n-button
            size="small" type="error" secondary
            :disabled="apiApproval.state !== 'pending'"
            @click="rejectApiDoc"
          >拒绝</n-button>
          <n-button
            size="small" type="primary"
            :disabled="apiApproval.state !== 'pending'"
            :loading="apiApproval.state === 'approving'"
            @click="approveApiDoc"
          >批准导入</n-button>
        </div>
      </div>

      <!-- 附件导入结果卡（任务 10-20，R4/AC1/AC2/AC6）：
           体检汇总 → 已自动修复清单（行号 + 改了什么 + 待核对）→ 剩余 E-*/W-*（复用
           ApiIssueList）→ 确认导入（needConfirm 需勾二次确认）→ 导入结果 → 下载修正稿。
           沿用上面两张审批卡片的 .deploy-* 样式族（同一视觉语言，零新增 CSS 变量）；
           文件名 / before / after / note 一律文本插值，**零新增 v-html**。
           后端闸门不被前端代替：按钮只把「已存在的闸」显示出来，硬拦在服务层。 -->
      <div v-if="attachCard" class="deploy-card">
        <div class="deploy-head">
          <n-tag type="info" size="small">接口文档附件导入</n-tag>
          <strong class="deploy-title">{{ attachCard.fileName }}</strong>
          <span class="deploy-meta">
            {{ formatSize(attachCard.fileSize) }} ·
            {{ attachCard.initMode ? '基线导入（已勾选）' : '增量导入' }} ·
            解析档位 {{ attachPreview?.mode || '—' }}
          </span>
          <n-tag v-if="attachCard.previewing" size="small" type="info">体检中…</n-tag>
          <n-tag v-else-if="attachCard.result" size="small" type="success">已导入</n-tag>
          <n-tag v-else-if="attachCard.error" size="small" type="error">未导入</n-tag>
          <n-tag v-else-if="attachPreview && !attachPreview.ok" size="small" type="error">未导入</n-tag>
          <n-tag v-else-if="attachPreview?.needConfirm" size="small" type="warning">需二次确认</n-tag>
          <!-- 收卡：只在没有异步动作在飞时可点（提交中关掉卡片会让"写了什么"失去着落） -->
          <n-button
            size="tiny" quaternary class="attach-close" title="关闭这张卡片"
            :disabled="attachCard.previewing || attachCard.committing"
            @click="dismissAttachCard"
          >✕</n-button>
        </div>

        <!-- 后端中文错误原样展示（档位冲突 / 空文本 / 闸门拒收都走这一路，不加工不猜测） -->
        <div v-if="attachCard.error" class="attach-error">后端拒绝：{{ attachCard.error }}</div>

        <div v-if="attachPreview" class="deploy-desc">
          {{ attachPreview.message }}
          <template v-if="attachFixes.length"> · 已自动修复 {{ attachFixes.length }} 处</template>
          <template v-if="attachErrors.length"> · 剩余 {{ attachErrors.length }} 个 E-* 需人工处理</template>
          <template v-else-if="attachWarnings.length"> · 剩余 {{ attachWarnings.length }} 个 W-* 建议核对</template>
        </div>

        <!-- 修复清单（D4：改了哪几处、每处行号与理由必须可见，不能静默改完就导） -->
        <div v-if="attachFixes.length" class="fix-zone">
          <div class="zone-title">
            已自动修复 {{ attachFixes.length }} 处<template v-if="attachReviewCount">（其中 {{ attachReviewCount }} 处为推断，待人工核对）</template>
          </div>
          <div class="deploy-cmds">
            <div v-for="(f, i) in attachFixes" :key="i" class="fix-row">
              <div class="fix-head">
                <span class="fix-kind">{{ FIX_KIND_TEXT[f.kind] || f.kind }}</span>
                <span class="fix-line">第 {{ f.line }} 行</span>
                <n-tag v-if="f.needsReview" size="tiny" type="warning">待人工核对</n-tag>
              </div>
              <div class="fix-diff">
                <div class="fix-before">− {{ f.before }}</div>
                <div class="fix-after">+ {{ f.after }}</div>
              </div>
              <div v-if="f.note" class="fix-note">{{ f.note }}</div>
            </div>
          </div>
        </div>

        <!-- 剩余问题：E-* 红（阻断）/ W-* 黄（不阻断但要核对），带行号 —— 现成组件，不重写 -->
        <ApiIssueList v-if="attachErrors.length" :issues="attachIssues" level="E"/>
        <ApiIssueList v-if="attachWarnings.length" :issues="attachIssues" level="W"/>

        <div v-if="attachNotes.length" class="note-zone">
          <div v-for="(n, i) in attachNotes" :key="i">· {{ n }}</div>
        </div>

        <!-- 导入结果（R4：事件 seq / stats / 是否生成事件 / 修复处数） -->
        <div v-if="attachCard.result" class="result-zone">
          <div class="zone-title">导入结果</div>
          <div class="deploy-params">
            <span class="deploy-param">
              事件 {{ attachCard.result.eventId ? 'v' + attachCard.result.seq : '未生成（基线导入或无变更）' }}
            </span>
            <span class="deploy-param">新增 {{ attachCard.result.stats?.added || 0 }}</span>
            <span class="deploy-param">修改 {{ attachCard.result.stats?.updated || 0 }}</span>
            <span class="deploy-param">删除 {{ attachCard.result.stats?.removed || 0 }}</span>
            <span class="deploy-param">恢复 {{ attachCard.result.stats?.restored || 0 }}</span>
            <span class="deploy-param">未变更 {{ attachCard.result.stats?.unchanged || 0 }}</span>
            <span class="deploy-param">已修复 {{ attachCard.result.fixCount || 0 }} 处</span>
          </div>
          <div v-if="attachCard.result.createdProject || attachCard.result.createdDomains?.length" class="deploy-desc">
            <template v-if="attachCard.result.createdProject">本次新建项目「{{ attachPreview?.projectName }}」；</template>
            <template v-if="attachCard.result.createdDomains?.length">新建领域：{{ attachCard.result.createdDomains.join('、') }}</template>
          </div>
          <div v-if="attachResultNotes.length" class="note-zone">
            <div v-for="(n, i) in attachResultNotes" :key="i">· {{ n }}</div>
          </div>
          <div class="deploy-desc">{{ attachCard.result.message }}</div>
        </div>

        <div class="deploy-actions">
          <span class="deploy-hint">{{ attachBlockReason || (attachCard.result ? '本次导入已完成，可下载修正稿存档' : '确认后写入并生成变更事件') }}</span>
          <n-checkbox
            v-if="attachPreview?.needConfirm && !attachCard.result"
            v-model:checked="attachCard.confirmed"
            :disabled="attachCard.committing || attachCard.previewing"
          >我已核对删除/破坏性变更</n-checkbox>
          <!-- 下载/复制的是后端算好的修复稿全文（fixedText），不是前端再修一遍（D7/AC6） -->
          <n-button v-if="attachHasFix" size="small" @click="downloadFixedText">下载修正后的 md</n-button>
          <n-button v-if="attachHasFix" size="small" quaternary @click="copyFixedText">复制修正稿</n-button>
          <n-button
            v-if="!attachCard.result"
            size="small" type="primary"
            :disabled="!canCommitAttach"
            :loading="attachCard.committing || attachCard.previewing"
            @click="commitAttachmentImport"
          >{{ attachCommitLabel }}</n-button>
        </div>
      </div>

      <!-- 输入区外包一层 .input-zone：附件条钉在输入框上方 + 整块区域是拖拽落点（R1）。
           分隔线从 .chat-input 挪到本层，否则附件条会跑到分隔线之上、与输入框割裂。 -->
      <div
        class="input-zone"
        :class="{'drag-over': dragOver}"
        @dragenter.prevent="onZoneDragOver"
        @dragover.prevent="onZoneDragOver"
        @dragleave="onZoneDragLeave"
        @drop.prevent="onZoneDrop"
      >
        <!-- 附件条（R1）：文件名 / 大小 / 「基线导入」勾选 / 移除。
             名字只用 {{ }} 文本插值（Vue 自动 HTML 转义）：不进 v-html、不拼路径、不落盘（D9）。
             勾选位 = req.initMode = §13.26 的解析档位权威位：不勾而文档自声明 scope: init
             被后端拒收；勾了而文档声明 incremental/full 同样被拒（对称面）。 -->
        <div v-if="attach" class="attach-bar">
          <span class="attach-file">📄 {{ attach.name }}</span>
          <span class="attach-size">{{ formatSize(attach.size) }}</span>
          <n-checkbox v-model:checked="attach.initMode" size="small">基线导入</n-checkbox>
          <span class="attach-tip">
            {{ attach.initMode
              ? '按 scope: init 灌入存量基线（仅空项目允许，不生成变更事件）'
              : '发送即「体检 → 机械修复 → 导入」，正文不发给模型、不占上下文' }}
          </span>
          <n-button size="tiny" quaternary :disabled="attachReading" @click="removeAttachment">移除</n-button>
        </div>

        <!-- 底部：输入区（position:relative，作为建议浮层的定位上下文）。
             @keydown.enter.exact="onEnterOrSend" 解读：
             .exact = 不带 Shift 等修饰键才触发（Shift+Enter 照常换行）；
             preventDefault 在 onEnterOrSend 内手动调用——输入法组合态（isComposing）
             时直接放行不阻止默认行为，候选回车才能正常上屏（详见函数注释）。
             浮层打开时 Enter 是"选中建议"而非发送（在 onEnterOrSend 内分流）。
             v-model:value = 双向绑定：输入框内容 ↔ input 变量（≈ 自动的 getText/setText） -->
        <div class="chat-input">
          <!-- 建议浮层：输入首字符为 / 或 @ 时弹出在输入框上方（不遮挡发送按钮）。
               mousedown.prevent = 点击选中不让文本框失焦；↑↓ 导航、Enter/Tab 选中、Esc 关闭 -->
          <div v-if="suggVisible" class="sugg-pop">
            <div
              v-for="(it, i) in suggItems" :key="it.id || it.name"
              class="sugg-item" :class="{active: i === suggIndex}"
              @mouseenter="suggIndex = i"
              @mousedown.prevent="selectSuggestion(it)"
            >
              <span class="sugg-name">{{ (suggTrigger.kind === 'skill' ? '/' : '@') + it.name }}</span>
              <span v-if="it.description" class="sugg-desc">{{ it.description }}</span>
            </div>
            <div v-if="suggItems.length === 0" class="sugg-empty">{{ suggEmptyText }}</div>
            <div class="sugg-hint">↑↓ 选择 · Enter/Tab 确认 · Esc 关闭</div>
          </div>
          <!-- 附件入口①：回形针（入口②是整块输入区的拖拽，两条等效，AC8）。
               accept 限定扩展名与 MIME；隐藏 input 由按钮 programmatic click 触发 -->
          <n-button
            size="small" quaternary class="attach-btn"
            title="附加接口文档 Markdown（.md / .markdown，≤2 MB）"
            :loading="attachReading"
            :disabled="sending"
            @click="pickAttach"
          >📎</n-button>
          <input
            ref="attachInput"
            type="file"
            accept=".md,.markdown,text/markdown"
            class="hidden-file"
            @change="onAttachFilePicked"
          />
          <n-input
            ref="inputRef"
            v-model:value="input"
            type="textarea"
            :autosize="{minRows: 1, maxRows: 5}"
            placeholder="输入消息，Enter 发送，Shift+Enter 换行；/ 触发技能，@运维 强制路由；📎 或拖入 .md = 直接导入接口文档"
            :disabled="sending"
            @keydown="onSuggKeydown"
            @keydown.enter.exact="onEnterOrSend"
          />
          <!-- 停止按钮：回合流式期间或部署执行期间出现。点击后本地立即收起气泡，
               后端以"已停止"收尾（回合走 chat:done，部署走 deploy:done canceled） -->
          <n-button v-if="sending || deployRunning" type="warning" secondary @click="stopTurn">停止</n-button>
          <!-- 带附件时输入文字不是必要条件（D8：附件本身就是要执行的操作）；
               读文件期间禁发，防"附件还没读完就点发送" -->
          <n-button
            type="primary"
            :loading="sending"
            :disabled="(!input.trim() && !attach) || attachReading"
            @click="send"
          >发送</n-button>
        </div>
      </div>

      </div><!-- /.chat-view -->

      <!-- ---- skills 视图：技能管理（卡片网格 + 详情弹窗）。v-if 渲染——进入视图挂载并加载数据，离开即卸载 ---- -->
      <SkillManager v-if="view === 'skills'"/>

      <!-- ---- agents 视图：智能体管理（卡片网格 + 详情弹窗）。同上 ---- -->
      <AgentManager v-if="view === 'agents'"/>

      <!-- ---- apidocs 视图：接口文档（「接口」/「变更事件」两个 tab + 导入向导）。
             同上：v-if 渲染，进视图挂载即自拉数据 ---- -->
      <ApiDocManager v-if="view === 'apidocs'"/>
    </n-layout>
  </n-layout>

  <!-- 修正稿降级弹窗（D7/AC6）：Blob 下载被浏览器策略拦掉、或剪贴板两条路都不通时，
       把全文摆出来供手动复制。n-code 的 code 是纯字符串属性，不经 markdown/v-html 管道
       ⇒ 外部文档内容零注入面（与 ApiDocManager「导出规范」降级弹窗同一手法）。 -->
  <n-modal
    v-if="fixedModalShow"
    :show="true"
    preset="card"
    title="修正后的接口文档（可全选复制）"
    style="width: 760px"
    :segmented="{content: true}"
    @update:show="fixedModalShow = false"
  >
    <div class="fixed-modal-body">
      <div class="fixed-modal-tip">
        当前环境不允许直接下载。原文如下（已由后端确定性修复，原文件未被改写），请全选复制后存为 .md。
      </div>
      <n-code :code="fixedModalText" language="markdown" word-wrap/>
      <div class="fixed-modal-acts">
        <n-button size="small" @click="copyFixedText">再次尝试复制</n-button>
        <n-button size="small" type="primary" ghost @click="fixedModalShow = false">关闭</n-button>
      </div>
    </div>
  </n-modal>

  <!-- 设置弹窗。v-model:show 双向绑定显隐状态：
       弹窗内部关闭时会把 show 写回 false（父子组件通信的简洁写法） -->
  <SettingsModal v-model:show="showSettings"/>
</template>

<!-- scoped：样式只作用于本组件（避免全局污染，≈ CSS Modules） -->
<style scoped>
/* 全屏链条：html/body/#app 高度在 style.css；此处保证布局本身显式占满视口。
   .app-layout 显式 flex + width/height：不依赖 n-layout 默认块级行为，
   服务器模式（浏览器标签页）下也稳定铺满。
   .main 显式 height + flex:1：右侧视图吃掉侧边栏之外的全部剩余宽/高 */
.app-layout { display: flex; width: 100%; height: 100vh; }
.main { flex: 1; min-width: 0; height: 100%; display: flex; flex-direction: column; }
.sider { display: flex; flex-direction: column; }
.sider-head { padding: 12px; display: flex; flex-direction: column; gap: 8px; }
.brand { font-weight: 700; font-size: 16px; letter-spacing: 1px; }
.session-list { flex: 1; overflow-y: auto; padding: 4px 8px; }
.session-item {
  display: flex; align-items: center; justify-content: space-between;
  padding: 8px 10px; border-radius: 6px; cursor: pointer; font-size: 13px;
}
.session-item:hover { background: rgba(255,255,255,0.06); }
.session-item.active { background: rgba(99,226,183,0.15); }
/* 标题占据剩余宽度并可截断（min-width:0 是 flex 子项省略号生效的关键） */
.session-title { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
/* 行内改名输入框：撑满标题原本的宽度 */
.session-edit { flex: 1; min-width: 0; }
/* 悬停操作组（✏️/✕）：默认隐藏，hover 才显现，不挤占标题布局 */
.session-actions { display: flex; align-items: center; gap: 2px; margin-left: 6px; opacity: 0; }
.session-item:hover .session-actions { opacity: 1; }
.session-act, .session-del { padding: 0 2px; } /* tiny 图标按钮收紧内边距，并排不溢出 */
.sider-foot {
  padding: 8px 12px; border-top: 1px solid rgba(255,255,255,0.08);
  display: flex; flex-direction: column; gap: 2px; /* 设置 / 技能 / 智能体 / 接口文档四个入口按钮纵向堆叠 */
}
/* 入口按钮对齐：n-button 默认内容居中——撑满其内部 __content 容器后，
   用"固定宽度图标列 + 左对齐文字列"的 flex 行实现三按钮图标/文字各自纵向成列。
   （保留 block quaternary 观感，仅改内容排布） */
.sider-btn :deep(.n-button__content) { width: 100%; }
.sider-btn-icon {
  width: 24px; flex: 0 0 24px; font-size: 16px; line-height: 1; text-align: left;
}
.sider-btn-label { flex: 1; text-align: left; }
/* 当前激活视图对应的按钮：轻度高亮（沿用会话列表 .active 的绿色系）。
   quaternary 按钮的背景/文字色由 --n-color* 变量驱动（组件内联样式注入），
   故以 !important 的变量覆盖实现选中态 */
.sider-btn.active {
  --n-color: rgba(99,226,183,0.15) !important;
  --n-color-hover: rgba(99,226,183,0.22) !important;
  --n-color-pressed: rgba(99,226,183,0.28) !important;
  --n-color-focus: rgba(99,226,183,0.22) !important;
}
.empty { margin-top: 40px; }
/* 聊天视图包裹层：v-show 常驻（三态切换不卸载聊天 DOM）。
   撑满主区并以 flex 列排布子区，消息流 flex:1 内部滚动、输入区钉底 */
.chat-view { display: flex; flex-direction: column; height: 100%; }
.chat-head { padding: 10px 16px; border-bottom: 1px solid rgba(255,255,255,0.08); display: flex; align-items: center; }
.hint { color: rgba(255,255,255,0.45); font-size: 13px; }
/* overflow-x 必须显式 hidden：CSS 规定一个轴非 visible 时另一轴自动按 auto 处理，
   所以只要消息里有超宽内容（宽表格、长 inline code），就会长出横向滚动条，
   把纵向滚动条推到窗口最右、中间露一条空面板。横向滚动一律禁掉，超宽内容由
   元素自己滚（见下面 table/pre 的 overflow-x: auto）。 */
.chat-list { flex: 1; min-width: 0; overflow-y: auto; overflow-x: hidden; padding: 16px 24px; }
/* 气泡宽度随视图拉伸：16:9 宽屏下占满主区（1600px 上限防超宽屏行长失控） */
.msg { margin-bottom: 18px; max-width: min(1600px, 100%); }
.msg-role { font-size: 12px; color: rgba(255,255,255,0.45); margin-bottom: 4px; }
/* ---- user 消息：右对齐暗色气泡（D1/D3）----
   整列 align-items:flex-end → 角色名「我」与气泡一起贴右缘；
   取色：底色 #1f2b27 是主色 #63e2b7 的暗调派生（与页面底色 #18181c 拉开一档，
   替换原先太淡的 rgba(99,226,183,0.12)——用户点名要暗色实心）；
   1px 边框 rgba(99,226,183,0.18) 用低透明度主色描边，可辨而不抢；
   圆角 12px、右下角收 4px = 右侧消息的"尾巴"指向发送者（DeepSeek 风格）。
   备选（若气泡色不满意只改这一处）：中性灰 #26262c + 边框 rgba(255,255,255,0.08) */
.msg.user { display: flex; flex-direction: column; align-items: flex-end; }
.msg.user .msg-body {
  background: #1f2b27;
  border: 1px solid rgba(99,226,183,0.18);
  border-radius: 12px 12px 4px 12px;
  padding: 8px 12px;
  max-width: 72%;   /* 内容列的 72%：长消息留宽度余量，且明显窄于 assistant 卡片（AC2） */
  text-align: left; /* 右对齐只定位气泡，正文在泡内仍左对齐（R1） */
}
/* ---- assistant 消息：文档卡片，刻意不是聊天气泡（D4）----
   长 markdown（表格/代码块/标题层级）需要"文档"感与可读宽度，气泡形态会压缩行宽
   且与用户泡同质化，故：无主色底、无尾巴，占满内容列宽度（仅受 1600px 上限约束）。
   取色：底 rgba(255,255,255,0.02) + 1px 边框 rgba(255,255,255,0.06)——在白字深色底上
   只做出"比页面底色 #18181c 略亮一层"的分隔，弱于审批卡描边（琥珀 0.45），不抢正文 */
.msg.assistant {
  background: rgba(255,255,255,0.02);
  border: 1px solid rgba(255,255,255,0.06);
  border-radius: 10px;
  padding: 10px 14px;
}
/* 卡片头：第二轮修订后**只放角色名**——整条消息的「复制原文」按钮已移除
   （D2/D4：复制粒度下沉到每个代码块自己的标题栏）。仍留 flex 行，与卡片内
   msg-head / msg-tools 的基线一致，将来加徽标不用重排 */
.msg-head { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; }
.msg-head .msg-role { margin-bottom: 0; }
/* word-break/overflow-wrap 是上一轮溢出修复的核心（R5）：长 token 在泡内/卡片内
   任意位置断行，配合 .chat-list 的 overflow-x:hidden 保证不出横向滚动条 */
.msg-body { line-height: 1.6; font-size: 14px; word-break: break-word; overflow-wrap: anywhere; }
/* :deep() 穿透 scoped 限制，给 v-html 渲染出的 Markdown 元素加样式
   （scoped 默认不会作用于动态插入的子节点） */
/* pre 显式写 line-height:1.5 + font-size:13px 是折叠限高的数学前提（见下面
   .md-block-collapsed）：行盒由 pre 的 strut（line-height × 自身字号）与 code 的
   行高取大者，两边必须同源才能用 calc(8 * 1.5em) 精确切在 8 行处。
   13px 与 :deep(code) 的 13px 同值 → 视觉零变化，只是不再依赖继承来的 14px。 */
.msg-body :deep(pre) {
  background: rgba(0,0,0,0.35); padding: 10px; border-radius: 6px; overflow-x: auto;
  font-size: 13px; line-height: 1.5;
}
/* inline code 里的长 token（E-SPEC-BLOCK-MISSING、/api/v1.0/paper 这类）是
   默认不换行的，撑宽父容器 → 允许任意位置断行 */
.msg-body :deep(code) { font-family: Consolas, monospace; font-size: 13px; overflow-wrap: anywhere; }
.msg-body :deep(p) { margin: 6px 0; }
/* Markdown 表格（marked 开了 gfm:true，LLM 常回参数表/对照表）：
   原先只有 pre/code/p 三条 :deep()，裸 <table> 无边框无对齐、行高失控，与深色 UI 明显不搭。
   display:block + overflow-x:auto 是关键：table 的固有最小宽度不受 max-width 约束，
   列多时会把整个消息列撑宽（→ 横向滚动条 + 右侧空面板），改成块级后表格**自己**横向滚。 */
.msg-body :deep(table) {
  display: block; border-collapse: collapse; margin: 8px 0; font-size: 13px;
  max-width: 100%; overflow-x: auto;
}
.msg-body :deep(th), .msg-body :deep(td) {
  border: 1px solid rgba(255,255,255,0.09); padding: 6px 10px; text-align: left; vertical-align: top;
}
.msg-body :deep(th) { color: rgba(255,255,255,0.55); font-weight: 600; background: rgba(255,255,255,0.03); }
.msg-body :deep(td) { color: rgba(255,255,255,0.75); }
/* ================= 代码块「文档块」：标题栏 + 折叠态（D7/D8/D9）=================
   这些节点由 lib/md-blocks.js 用 DOMParser 烘进 HTML 字符串（不是 Vue 模板节点），
   类名 .md-block / .md-block-collapsed / .md-block-head / .md-btn 两边要对齐，
   改名得同时改装饰器。*/
/* font-size:13px = 与 pre 行盒同源，好让下面遮罩的 calc(3 * 1.5em) 是整行倍数
   （标题栏与按钮各自显式写 12px，不受这条影响） */
.msg-body :deep(.md-block) { position: relative; margin: 8px 0; font-size: 13px; }
/* 语言标签 + 行数靠左、按钮组靠右；flex-wrap 是窄窗口的保险：
   「展开剩余 12 行」这类长文案挤不下时整组换行，而不是溢出卡片撑出横向滚动 */
.msg-body :deep(.md-block-head) {
  display: flex; align-items: center; flex-wrap: wrap; gap: 6px;
  padding: 0 2px 5px;
}
/* 语言标签用主色但压到 0.7 透明度：只当"这是什么格式"的记号，不抢正文 */
.msg-body :deep(.md-block-lang) {
  font-family: Consolas, monospace; font-size: 12px; color: rgba(99,226,183,0.7);
}
.msg-body :deep(.md-block-meta) { font-size: 12px; color: rgba(255,255,255,0.4); }
.msg-body :deep(.md-block-acts) { margin-left: auto; display: flex; gap: 4px; }
/* 标题栏按钮是原生 <button>（v-html 里渲染不出 n-button，见 D9），观感照抄侧栏
   tiny quaternary：常态近乎透明（白 0.04 底 + 白 0.10 描边），hover 才现主色，
   可见但不抢正文 */
.msg-body :deep(.md-btn) {
  font-family: inherit; font-size: 12px; line-height: 1.5;
  color: rgba(255,255,255,0.7);
  background: rgba(255,255,255,0.04);
  border: 1px solid rgba(255,255,255,0.10); border-radius: 4px;
  padding: 1px 8px; cursor: pointer;
}
.msg-body :deep(.md-btn:hover) {
  color: #63e2b7; background: rgba(99,226,183,0.12); border-color: rgba(99,226,183,0.45);
}
/* 折叠态限高 = 前 8 行：8 * 1.5em（em 取 pre 自己的 13px → 156px = 8 × 19.5px）。
   不用魔法像素值：写死 156px 的话换个字号/行高就切在半行上。
   ⚠️ 行数与 lib/md-blocks.js 的 COLLAPSE_PREVIEW_LINES 是同一个数（CSS 读不到
   JS 常量、仓库不抽 token）——改一个必须改另一个，文案「展开剩余 N 行」才算得对。
   overflow-y 显式 hidden：CSS 规定一个轴非 visible 时另一轴按 auto 处理，pre 已经
   有 overflow-x:auto，这里留 auto 就会在折叠块里长出纵向滚动条（难看且跟遮罩打架）；
   overflow-x 保持 auto，长行仍由 pre 自己横向滚，不撑宽卡片（R5 不得回归）。 */
.msg-body :deep(.md-block-collapsed) pre {
  /* content-box 显式写出来：仓库目前没有任何全局 box-sizing 重置，但一旦将来有人加
     了 * { box-sizing: border-box }，max-height 会连 10px 下内边距一起算，切在半行上 */
  box-sizing: content-box;
  max-height: calc(8 * 1.5em);
  overflow-x: auto; overflow-y: hidden;
}
/* 底部渐隐遮罩：纯装饰，pointer-events:none 保证不挡 pre 里的文字选中与滚动。
   挂在**外壳 .md-block** 上而不是 pre 上——pre 自身是可横向滚动的滚动容器，
   给滚动容器加 absolute 伪元素会跟着内容一起滚，盖不住可视区下沿。
   终点色怎么算出来的（这条只能人工看眼，CSS 没有叠色计算）：
     页面底 #18181c = rgb(24,24,28)
   → assistant 卡片底 rgba(255,255,255,0.02) 合成：24*0.98 + 255*0.02 ≈ rgb(28.6, 28.6, 32.5)
   → pre 再叠自己的 rgba(0,0,0,0.35)：0.65 × 上一行 ≈ rgb(19, 19, 21)
   遮罩盖的是 **pre 内部**，所以终点取 rgb(19,19,21)；若图省事取卡片色 rgb(29,29,33)
   或页面色 rgb(24,24,28)，折叠块底部就会显出一块更亮/更暗的色斑。
   78% 提前压满：到 100% 才不透明的话最后一行字仍透出来，看着像没盖住。
   高度取 3 行；pre 还有 10px 下内边距吃掉一截，实际压住约最后两行半——
   觉得太厚/太薄就改这一个数。 */
.msg-body :deep(.md-block-collapsed)::after {
  content: ''; position: absolute; left: 0; right: 0; bottom: 0;
  height: calc(3 * 1.5em); pointer-events: none;
  background: linear-gradient(to bottom, rgba(19,19,21,0), rgb(19,19,21) 78%);
  border-radius: 0 0 6px 6px; /* 跟着 pre 的 6px 圆角收口，遮罩方角不戳出卡片 */
}
/* 滚动条收窄（仓库此前无任何滚动条样式）：Windows 经典滚动条宽 17px，会实打实吃掉
   内容宽度，配合上面的横向溢出就表现为"页面被往左拽、右侧露出一条空面板"。 */
.chat-list::-webkit-scrollbar,
.msg-body :deep(pre)::-webkit-scrollbar,
.msg-body :deep(table)::-webkit-scrollbar { width: 8px; height: 8px; }
.chat-list::-webkit-scrollbar-thumb,
.msg-body :deep(pre)::-webkit-scrollbar-thumb,
.msg-body :deep(table)::-webkit-scrollbar-thumb {
  background: rgba(255,255,255,0.18); border-radius: 4px;
}
.chat-list::-webkit-scrollbar-track,
.msg-body :deep(pre)::-webkit-scrollbar-track,
.msg-body :deep(table)::-webkit-scrollbar-track { background: transparent; }
.msg-tools { display: flex; gap: 6px; margin-bottom: 6px; flex-wrap: wrap; }
/* 输入区外包裹层：附件条 + 输入行的共同容器，也是拖拽落点。
   分隔线从 .chat-input 挪到这里 —— 附件条必须落在分隔线**下方**（与输入框同区），
   否则视觉上会变成"附件属于消息流"。 */
.input-zone { border-top: 1px solid rgba(255,255,255,0.08); }
/* 拖拽悬停：主色虚线内描边（outline-offset 负值 → 不撑高布局、不产生横向溢出，R5） */
.input-zone.drag-over {
  outline: 1px dashed rgba(99,226,183,0.65);
  outline-offset: -3px;
  background: rgba(99,226,183,0.05);
}
/* 附件条（R1）：文件名 + 大小 + 基线导入勾选 + 移除，钉在输入框上方 */
.attach-bar {
  display: flex; align-items: center; flex-wrap: wrap; gap: 8px;
  padding: 8px 24px 0; font-size: 12px;
}
/* 文件名可长（Downloads/api-doc-2026-…md）：max-width + 省略号，min-width:0 是
   flex 子项截断生效的关键（与会话标题同一手法），绝不撑出横向滚动条 */
.attach-file {
  max-width: 320px; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  font-family: Consolas, monospace; color: rgba(99,226,183,0.85);
}
.attach-size { color: rgba(255,255,255,0.45); font-family: Consolas, monospace; }
.attach-tip { color: rgba(255,255,255,0.4); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.attach-btn { flex: 0 0 auto; }
/* 收卡按钮钉在卡片头行右缘（margin-left:auto 吃掉剩余宽度，长文件名换行也不影响它） */
.attach-close { margin-left: auto; }
/* 隐藏的原生 file input：不用 display:none 在 n-button 之外占据可见空间 */
.hidden-file { display: none; }
.chat-input {
  position: relative; /* 建议浮层的定位上下文（浮层钉在输入区上方） */
  display: flex; gap: 10px; padding: 12px 24px; align-items: flex-end;
}
/* 建议浮层：深色面板贴合主题（背景取全局 #18181c 略提亮），高亮沿用
   会话列表的绿色选中色系；描述单行省略（部署技能的动态描述可能很长） */
.sugg-pop {
  position: absolute; bottom: calc(100% + 4px); left: 24px;
  width: min(560px, calc(100% - 48px));
  max-height: 260px; overflow-y: auto; z-index: 30;
  background: #232329; border: 1px solid rgba(255,255,255,0.12);
  border-radius: 8px; box-shadow: 0 6px 24px rgba(0,0,0,0.45);
  padding: 4px;
}
.sugg-item {
  display: flex; align-items: baseline; gap: 10px;
  padding: 6px 10px; border-radius: 6px; cursor: pointer; font-size: 13px;
}
.sugg-item:hover, .sugg-item.active { background: rgba(99,226,183,0.15); }
.sugg-name { font-family: Consolas, monospace; white-space: nowrap; }
.sugg-desc {
  color: rgba(255,255,255,0.55); font-size: 12px;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.sugg-empty { padding: 8px 10px; font-size: 13px; color: rgba(255,255,255,0.45); }
.sugg-hint {
  margin-top: 2px; padding: 4px 10px; font-size: 11px;
  color: rgba(255,255,255,0.35); border-top: 1px solid rgba(255,255,255,0.08);
}
/* 部署审批卡片：醒目的边框 + 等宽命令清单，写操作必须人工核对 */
.deploy-card {
  margin: 0 24px 8px; padding: 12px 14px;
  border: 1px solid rgba(240,168,48,0.45); border-radius: 8px;
  background: rgba(240,168,48,0.06);
}
.deploy-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.deploy-title { font-size: 14px; }
.deploy-meta { color: rgba(255,255,255,0.55); font-size: 12px; }
.deploy-desc { margin-top: 6px; font-size: 13px; color: rgba(255,255,255,0.75); }
.deploy-params { margin-top: 6px; display: flex; gap: 8px; flex-wrap: wrap; }
.deploy-param {
  font-size: 12px; padding: 2px 8px; border-radius: 4px;
  background: rgba(255,255,255,0.08); font-family: Consolas, monospace;
}
.deploy-cmds {
  margin-top: 8px; padding: 8px 10px; border-radius: 6px;
  background: rgba(0,0,0,0.35); max-height: 220px; overflow-y: auto;
}
.deploy-cmd {
  font-family: Consolas, monospace; font-size: 12.5px; line-height: 1.7;
  word-break: break-all; white-space: pre-wrap;
}
.deploy-actions { margin-top: 10px; display: flex; align-items: center; gap: 10px; justify-content: flex-end; }
.deploy-hint { margin-right: auto; font-size: 12px; color: rgba(255,255,255,0.45); }
/* ================= 附件导入结果卡（任务 10-20）=================
   容器复用上面的 .deploy-card 族（三张卡片同一视觉语言，不另起样式族、不抽 token）。
   配色沿用仓库既有三档：红 rgba(224,108,117) 阻断 / 琥珀 rgba(240,168,48) 待核对 /
   主色 rgba(99,226,183) 信息，与 ApiIssueList.vue 里错误区、警告区两栏完全一致。 */
.attach-error {
  margin-top: 8px; padding: 8px 10px; border-radius: 6px; font-size: 12.5px; line-height: 1.7;
  background: rgba(224,108,117,0.08); border: 1px solid rgba(224,108,117,0.35);
  color: rgba(255,255,255,0.82); word-break: break-word; overflow-wrap: anywhere;
}
.fix-zone, .result-zone, .note-zone { margin-top: 8px; }
.note-zone { font-size: 12px; color: rgba(255,255,255,0.6); line-height: 1.7; }
.fix-zone > .zone-title, .result-zone > .zone-title {
  font-size: 12px; font-weight: 600; color: rgba(99,226,183,0.85); margin-bottom: 4px;
}
/* 修复清单每条：类型 + 行号 + 待核对徽标一行，before/after 等宽两行，note 次行。
   .deploy-cmds 已经给了 max-height:220 + overflow-y:auto（复用，不重定义滚动区） */
.fix-row {
  padding: 6px 0; border-top: 1px dashed rgba(255,255,255,0.12); line-height: 1.7; font-size: 12px;
}
.fix-row:first-of-type { border-top: none; }
.fix-head { display: flex; align-items: center; gap: 10px; }
.fix-kind { color: rgba(255,255,255,0.9); font-weight: 600; }
.fix-line { color: rgba(255,255,255,0.5); font-family: Consolas, monospace; }
.fix-diff {
  margin-top: 2px; font-family: Consolas, Menlo, monospace; color: rgba(255,255,255,0.7);
  /* 修复前后原文可能很长（搬运进来的整张表概述）：允许任意断行，绝不撑出横向滚动 */
  word-break: break-word; overflow-wrap: anywhere; white-space: pre-wrap;
}
.fix-before { color: rgba(224,108,117,0.85); }
.fix-after { color: rgba(99,226,183,0.85); }
.fix-note { margin-top: 2px; color: rgba(255,255,255,0.6); }
/* 修正稿降级弹窗 */
.fixed-modal-body { display: flex; flex-direction: column; gap: 8px; max-height: 60vh; overflow-y: auto; }
.fixed-modal-tip { font-size: 12px; color: rgba(255,255,255,0.55); }
.fixed-modal-acts { display: flex; gap: 8px; justify-content: flex-end; }
</style>
