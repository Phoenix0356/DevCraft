<!--
  ChatApp.vue —— 应用主界面（前端最核心的文件）。
  布局：左侧会话侧边栏 + 右侧聊天区（头部 Agent 选择器 / 中部消息流 / 底部输入框）。
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
  NewSession, ListSessions, DeleteSession, SetSessionAgent, GetMessages,
  SendMessage, CancelTurn, ListAgents, ApproveDeployment, RejectDeployment
} from '../../bindings/DevCraft/app.js'

import {renderMarkdown} from '../lib/markdown'     // Markdown → HTML
import SettingsModal from './SettingsModal.vue'     // 设置弹窗子组件
import SkillManager from './SkillManager.vue'       // 技能管理弹窗（侧边栏独立入口）

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
const showSkills = ref(false)         // 技能管理弹窗显隐（与 SkillManager 双向绑定）
const listRef = ref(null)             // 消息列表 DOM 引用（模板里 ref="listRef" 自动注入），用于滚动

// 当前会话绑定的 Agent id（computed：currentSession 变则自动重算）
const currentAgentId = computed(() => currentSession.value?.agentId || '')
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

// ---------------- 会话操作 ----------------

/** 新建会话并立即打开 */
async function createSession() {
  const sess = await NewSession()
  await loadSessions(false)   // 刷新侧栏（不自动选中，下一行手动选）
  await selectSession(sess)
}

/** 打开某个会话：设为当前、加载历史消息、滚到底部 */
async function selectSession(sess) {
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

/** 切换会话绑定的 Agent（头部下拉框变化时触发） */
async function onAgentChange(agentId) {
  if (!currentSession.value) return
  await SetSessionAgent(currentSession.value.id, agentId)
  currentSession.value = {...currentSession.value, agentId}
  // {...obj, agentId} = 展开运算符：复制一份对象并覆盖字段（保持响应式更新）
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
</script>

<template>
  <!-- n-layout has-sider：左右分栏布局容器 -->
  <n-layout has-sider class="app-layout">

    <!-- ============ 左侧：会话侧边栏 ============ -->
    <n-layout-sider bordered :width="250" class="sider">
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
          <span class="session-title">{{ s.title || '新会话' }}</span>
          <!-- {{ }} 双花括号 = 文本插值（≈ 模板引擎的 ${}），自动 HTML 转义 -->
          <n-button quaternary size="tiny" class="session-del" @click.stop="removeSession(s)">✕</n-button>
          <!-- .stop 修饰符 = 阻止事件冒泡（否则删会话会先触发外层的 selectSession） -->
        </div>
        <!-- v-if：条件渲染，列表为空时显示占位提示 -->
        <n-empty v-if="sessions.length === 0" description="暂无会话" size="small" class="empty"/>
      </div>

      <div class="sider-foot">
        <n-button block quaternary @click="showSettings = true">⚙ 设置</n-button>
        <!-- 技能管理独立入口：打开 SkillManager 弹窗（技能卡片列表） -->
        <n-button block quaternary @click="showSkills = true">🧩 技能</n-button>
      </div>
    </n-layout-sider>

    <!-- ============ 右侧：聊天主区 ============ -->
    <n-layout class="main">

      <!-- 头部：Agent 选择器。:options 绑定 JS 表达式：把 Agent 列表映射成下拉选项数组；
           Naive UI 组件的值更新事件统一叫 update:xxx -->
      <div class="chat-head">
        <n-select
          v-if="currentSession"
          :value="currentAgentId"
          :options="agents.map(a => ({label: a.name, value: a.id}))"
          size="small"
          style="width: 200px"
          @update:value="onAgentChange"
        />
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

      <!-- 底部：输入区。@keydown.enter.exact.prevent="send" 解读：
           .exact = 不带 Shift 等修饰键才触发；.prevent = preventDefault（阻止文本框换行）。
           v-model:value = 双向绑定：输入框内容 ↔ input 变量（≈ 自动的 getText/setText） -->
      <div class="chat-input">
        <n-input
          v-model:value="input"
          type="textarea"
          :autosize="{minRows: 1, maxRows: 5}"
          placeholder="输入消息，Enter 发送，Shift+Enter 换行；可用 @运维 强制路由"
          :disabled="sending"
          @keydown.enter.exact.prevent="send"
        />
        <!-- 停止按钮：回合流式期间或部署执行期间出现。点击后本地立即收起气泡，
             后端以"已停止"收尾（回合走 chat:done，部署走 deploy:done canceled） -->
        <n-button v-if="sending || deployRunning" type="warning" secondary @click="stopTurn">停止</n-button>
        <n-button type="primary" :loading="sending" :disabled="!input.trim()" @click="send">发送</n-button>
      </div>
    </n-layout>
  </n-layout>

  <!-- 设置弹窗。v-model:show 双向绑定显隐状态：
       弹窗内部关闭时会把 show 写回 false（父子组件通信的简洁写法） -->
  <SettingsModal v-model:show="showSettings"/>

  <!-- 技能管理弹窗（侧边栏"技能"按钮打开的独立入口） -->
  <SkillManager v-model:show="showSkills"/>
</template>

<!-- scoped：样式只作用于本组件（避免全局污染，≈ CSS Modules） -->
<style scoped>
.app-layout { height: 100vh; }
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
.session-title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.session-del { opacity: 0; }
.session-item:hover .session-del { opacity: 1; } /* 悬停时才显示删除按钮 */
.sider-foot {
  padding: 8px 12px; border-top: 1px solid rgba(255,255,255,0.08);
  display: flex; flex-direction: column; gap: 2px; /* 设置 / 技能两个入口按钮纵向堆叠 */
}
.empty { margin-top: 40px; }
.main { display: flex; flex-direction: column; }
.chat-head { padding: 10px 16px; border-bottom: 1px solid rgba(255,255,255,0.08); display: flex; align-items: center; }
.hint { color: rgba(255,255,255,0.45); font-size: 13px; }
.chat-list { flex: 1; overflow-y: auto; padding: 16px 24px; }
.msg { margin-bottom: 18px; max-width: 860px; }
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
  display: flex; gap: 10px; padding: 12px 24px; align-items: flex-end;
  border-top: 1px solid rgba(255,255,255,0.08);
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
