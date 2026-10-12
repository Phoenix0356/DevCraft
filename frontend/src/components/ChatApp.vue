<!--
  ChatApp.vue —— 应用主界面（前端最核心的文件）。
  布局：左侧会话侧边栏 + 右侧主区（三态视图，由 view 状态变量控制）：
    chat   —— 聊天区（头部 Agent 选择器 / 中部消息流 / 底部输入框）
    skills —— 技能管理视图（SkillManager：卡片网格占满视图，详情为弹窗）
    agents —— 智能体管理视图（AgentManager：卡片网格占满视图，详情为弹窗）
  技能/智能体为整个主区切换的视图（视图内详情用弹窗）；设置仍走 SettingsModal 弹窗。
  切到技能/智能体视图时聊天区 DOM 以 v-show 保留：流式帧照常接收处理，
  切回聊天页消息不丢、气泡与滚动状态原样。
  与 Go 后端的两条通信链路（Wails v3，桌面/浏览器两种环境通用）：
    ① 绑定桩函数调用（请求-响应，返回 Promise ≈ JS 的 Future）
    ② 聊天流 JSONStream('chat')（Go 主动推送流式增量：chat:delta / chat:tool / chat:done 帧）
-->
<script setup>
// ---------------- 导入 ----------------
import {ref, computed, nextTick, onMounted, onBeforeUnmount, watch} from 'vue'
// Vue 响应式 API：
//   ref(x)     = 创建一个"响应式盒子"，.value 变化 → 界面自动重渲染（≈ setState）
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
  RenameSession
} from '../../bindings/DevCraft/app.js'

import {renderMarkdown} from '../lib/markdown'     // Markdown → HTML
import SettingsModal from './SettingsModal.vue'     // 设置弹窗子组件
import SkillManager from './SkillManager.vue'       // 技能管理视图（主区三态之一，卡片网格 + 详情弹窗）
import AgentManager from './AgentManager.vue'       // 智能体管理视图（主区三态之一，卡片网格 + 详情弹窗）

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
// 右侧主区当前视图：'chat' | 'skills' | 'agents'（技能/智能体为整块视图，不再是弹窗）
const view = ref('chat')
const listRef = ref(null)             // 消息列表 DOM 引用（模板里 ref="listRef" 自动注入），用于滚动

// 当前会话绑定的 Agent id（computed：currentSession 变则自动重算）
const currentAgentId = computed(() => currentSession.value?.agentId || '')
// 当前会话 Agent 的显示名：头部下拉框移除后改为只读文字展示，切换靠消息里的 @ 路由
const currentAgentName = computed(() =>
  agents.value.find(a => a.id === currentAgentId.value)?.name || '')
// ?. 是可选链：currentSession 为 null 时不报错直接得 undefined（≈ Java 的 Optional.map）

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

/** 发送：乐观上屏 → 调 Go → 流式增量经事件回来 → done 事件收尾 */
async function send() {
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
// 帧格式与 Go 端 emitChat 对应：{event: 'chat:delta'|'chat:tool'|'chat:done', payload: {...}}
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
  }
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
      </div>
    </n-layout-sider>

    <!-- ============ 右侧：主区（三态视图：聊天 / 技能 / 智能体） ============ -->
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

        <!-- 历史消息：role 决定样式（用户/助手） -->
        <div v-for="(m, i) in messages" :key="i" class="msg" :class="m.role">
          <div class="msg-role">{{ m.role === 'user' ? '我' : 'DevCraft' }}</div>
          <!-- v-html：把渲染后的 HTML 直接插入（用于 Markdown）。
               注意：只用于可信内容（本应用的 LLM 回复），不可信输入会引发 XSS -->
          <div class="msg-body" v-html="renderMarkdown(m.content)"></div>
        </div>

        <!-- 流式气泡：sending 期间显示，内容随 chat:delta 增长 -->
        <div v-if="streaming" class="msg assistant">
          <div class="msg-role">DevCraft</div>
          <!-- 技能执行状态标签（n-tag 颜色随状态变化） -->
          <div class="msg-tools" v-if="streaming.tools.length">
            <n-tag
              v-for="(t, i) in streaming.tools" :key="i" size="small"
              :type="t.status === 'failed' ? 'error' : t.status === 'done' ? 'success' : 'info'"
            >
              {{ t.skill }} {{ t.status === 'running' ? '执行中…' : t.status === 'failed' ? '失败' : '完成' }}
            </n-tag>
          </div>
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
        <n-input
          ref="inputRef"
          v-model:value="input"
          type="textarea"
          :autosize="{minRows: 1, maxRows: 5}"
          placeholder="输入消息，Enter 发送，Shift+Enter 换行；/ 触发技能，@运维 强制路由"
          :disabled="sending"
          @keydown="onSuggKeydown"
          @keydown.enter.exact="onEnterOrSend"
        />
        <!-- 停止按钮：回合流式期间或部署执行期间出现。点击后本地立即收起气泡，
             后端以"已停止"收尾（回合走 chat:done，部署走 deploy:done canceled） -->
        <n-button v-if="sending || deployRunning" type="warning" secondary @click="stopTurn">停止</n-button>
        <n-button type="primary" :loading="sending" :disabled="!input.trim()" @click="send">发送</n-button>
      </div>

      </div><!-- /.chat-view -->

      <!-- ---- skills 视图：技能管理（卡片网格 + 详情弹窗）。v-if 渲染——进入视图挂载并加载数据，离开即卸载 ---- -->
      <SkillManager v-if="view === 'skills'"/>

      <!-- ---- agents 视图：智能体管理（卡片网格 + 详情弹窗）。同上 ---- -->
      <AgentManager v-if="view === 'agents'"/>
    </n-layout>
  </n-layout>

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
  display: flex; flex-direction: column; gap: 2px; /* 设置 / 技能 / 智能体三个入口按钮纵向堆叠 */
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
.chat-list { flex: 1; overflow-y: auto; padding: 16px 24px; }
/* 气泡宽度随视图拉伸：16:9 宽屏下占满主区（1600px 上限防超宽屏行长失控） */
.msg { margin-bottom: 18px; max-width: min(1600px, 100%); }
.msg-role { font-size: 12px; color: rgba(255,255,255,0.45); margin-bottom: 4px; }
.msg.user .msg-body {
  background: rgba(99,226,183,0.12); border-radius: 8px; padding: 8px 12px; display: inline-block;
}
.msg-body { line-height: 1.6; font-size: 14px; word-break: break-word; }
/* :deep() 穿透 scoped 限制，给 v-html 渲染出的 Markdown 元素加样式
   （scoped 默认不会作用于动态插入的子节点） */
.msg-body :deep(pre) {
  background: rgba(0,0,0,0.35); padding: 10px; border-radius: 6px; overflow-x: auto;
}
.msg-body :deep(code) { font-family: Consolas, monospace; font-size: 13px; }
.msg-body :deep(p) { margin: 6px 0; }
.msg-tools { display: flex; gap: 6px; margin-bottom: 6px; flex-wrap: wrap; }
.chat-input {
  position: relative; /* 建议浮层的定位上下文（浮层钉在输入区上方） */
  display: flex; gap: 10px; padding: 12px 24px; align-items: flex-end;
  border-top: 1px solid rgba(255,255,255,0.08);
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
</style>
