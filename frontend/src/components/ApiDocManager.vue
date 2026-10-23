<!--
  ApiDocManager.vue —— 接口文档模块主入口（右侧主区第四态视图：聊天/技能/智能体/接口文档）。
  ChatApp 以 v-if 渲染本组件：进入「接口文档」视图即挂载、离开即卸载（与技能/智能体一致，
  子视图自己 onMounted 重拉数据，父侧不加 watch(view) 副作用）。

  布局结构（全部普通 flex div —— 仓库对三栏没有任何现成件可抄，且 ChatApp.vue:860-864
  已用 display:flex 覆盖 n-layout 默认行为；n-layout 多 sider 行为未经验证，不赌）：
    顶层两个 tab（接口 / 变更事件）+ 右上角全局按钮（导入文档 / 导出规范 / 删除项目）。
    tab 用两个按钮 + v-if 切换实现（不是 n-tabs）：本模块三栏需要自己声明 height 与
    分区滚动，仓库无「全高 n-tabs」先例，硬穿 :deep() 不如沿用 ChatApp 的 view 字符串习惯。
    「接口」tab = 三栏：
      左栏（固定 240px）：项目列表（ListApiProjects）→ 选中项目后拉领域（ListApiDomains）
                          + 领域手工维护（改名 SaveApiDomain / 删除空领域 DeleteApiDomain）
                          + 「未归领域」筛选（domainId==='' 的残余条目，spec-draft §13.23）
                          + 搜索框（keyword 交后端模糊匹配名称/路径）
      中栏（固定 320px）：接口列表（ListApiEntries），每行 method 徽标 + pathRaw + 名称 +
                          parseState 标记；「显示内部端点」「显示已删除」开关（默认都关）
      右栏（flex:1）：ApiEntryDetail（详情 + 手工编辑 + 软删/恢复）
    「变更事件」tab = ApiChangeEvents（列表 seq DESC + 按 label 聚合 + 事件详情 + 原文 <pre>）。
    跳转链路（双向都走通）：
      接口详情历史事件 / 导入结果卡「查看该事件」 → 本组件 openEvent(eventId)
        → 切到「变更事件」tab 并把 eventId 交给 ApiChangeEvents 直接打开详情；
      事件明细「查看接口」 → openEntry(entryId) → 切回「接口」tab 并选中该接口
        （被 internal/removed 默认过滤挡掉时自动开两个开关再找一次）。
    导入视图：screen='import' 时整块换成 ApiDocImport（返回列表由 @cancel 触发）。

  数据来源：ListApiProjects / ListApiDomains / ListApiEntries / ExportApiDocSpec /
  DeleteApiProject / SaveApiDomain / DeleteApiDomain（bindings 桩）；
  事件侧数据在 ApiChangeEvents / ApiEventDetail 内部自取；
  详情与编辑在 ApiEntryDetail 内部自取（GetApiEntry/SaveApiEntry）。

  props / emit 契约：无 props、无 emit —— 本组件是顶层视图，与 ChatApp 只存在挂载关系。
  跨组件刷新沿用 DeployFlowConfig 的 emit('changed') + 父侧重拉 + 把 detail 指向 fresh 对象。

  D19 的三级不对称边界（本栏的领域操作只有两个，刻意没有「+新建领域」按钮）：
    项目 = 导入自动建 / 可删（级联）/ 不改名；领域 = 改名 / 合并（在导入预览里）/ 只删空领域；
    接口条目 = 手工编辑 / 软删 / 恢复。理由见 PRD D19：数据源头是文档，手工造领域只会制造
    下次导入时的身份冲突。
-->
<script setup>
import {computed, onMounted, ref} from 'vue'
import {useMessage} from 'naive-ui'
import {
  ListApiProjects, ListApiDomains, ListApiEntries,
  ExportApiDocSpec, DeleteApiProject, SaveApiDomain, DeleteApiDomain
} from '../../bindings/DevCraft/app.js'
import ApiEntryDetail from './ApiEntryDetail.vue'
import ApiDocImport from './ApiDocImport.vue'
import ApiChangeEvents from './ApiChangeEvents.vue'
import {copyText} from '../lib/clipboard' // 剪贴板写入共用 helper（含 http 非安全上下文降级，见 lib/clipboard.js）

const message = useMessage()

// ---------------- 顶层状态 ----------------
const tab = ref('entries')      // 'entries' | 'events'
const screen = ref('browse')    // 'browse' | 'import'
const exporting = ref(false)    // 导出规范 loading（防重复点）
const deletingProject = ref(false)
const showSpecModal = ref(false)
const specText = ref('')
// 事件详情跳转意图（0 = 无）：ApiChangeEvents 挂载时消费一次；手动切 tab 会清零，
// 否则用户点「变更事件」看列表时会被上一次的跳转意图拽进详情
const pendingEventId = ref(0)

// ---------------- 左栏：项目 + 领域 + 搜索 ----------------
const projects = ref([])
const currentProjectId = ref('')
const domains = ref([])
const currentDomainId = ref('') // '' = 全项目；UNGROUPED = 只看没归领域的残余条目
const keyword = ref('')

const currentProject = computed(() => projects.value.find(p => p.id === currentProjectId.value) || null)

/** 领域 id → 领域名（中栏与详情都做面包屑，必须容忍查不到 = 未归领域，不许崩） */
const domainNameById = computed(() => {
  const m = {}
  for (const d of domains.value) m[d.id] = d.name
  return m
})

// ---------------- 中栏：接口列表 ----------------
const entries = ref([])
const currentEntryId = ref('')
const includeInternal = ref(false) // 默认关（R11：internal 入库但列表默认过滤）
const includeRemoved = ref(false)  // 默认关（R7：软删行默认过滤）
const listLoading = ref(false)

const currentEntry = computed(() => entries.value.find(e => e.id === currentEntryId.value) || null)

// ---------------- 领域手工维护（D19：只有改名与删空，不提供新建） ----------------
const UNGROUPED = '__ungrouped__' // currentDomainId 的哨兵值（不可能与真实 UUID 撞车）
const renamingId = ref('')
const renameValue = ref('')
const domainSaving = ref(false)   // 改名提交中（同一时刻只允许一个）
const domainDeleting = ref('')    // 正在删除的领域 id（行级 loading）

/** 未归领域条目的可信计数：只有「全部领域」且无关键字时当前列表才等于全项目列表 */
const ungroupedCount = computed(() => {
  if (currentDomainId.value !== '' || keyword.value.trim()) return null
  return entries.value.filter(e => !e.domainId).length
})

// ---------------- 加载 ----------------
/** 项目列表：进入视图即拉；无项目时三栏整体走空态（导入向导可新建项目） */
async function loadProjects() {
  try {
    projects.value = (await ListApiProjects()) || []
  } catch (err) {
    projects.value = []
    message.error(String(err))
  }
}

/**
 * 领域列表：跟随选中项目；失败不阻塞接口列表（领域只是筛选维度）
 * keepSelection=true 给改名/删除后用：选中领域仍然存在时保持选中，别把用户踢回「全部领域」。
 * 改名/删除后 currentDomainId 可能指向已消失的领域，所以这里必须在重拉后裁决一次。
 */
async function loadDomains(keepSelection = false) {
  const keep = currentDomainId.value
  currentDomainId.value = ''
  if (!currentProjectId.value) {
    domains.value = []
    return
  }
  try {
    domains.value = (await ListApiDomains(currentProjectId.value)) || []
  } catch (err) {
    domains.value = []
    message.error(String(err))
  }
  if (keepSelection && keep && keep !== UNGROUPED && domains.value.some(d => d.id === keep)) {
    currentDomainId.value = keep
  }
}

/** 接口列表：查询条件全部交后端（keyword/domainId/includeInternal/includeRemoved） */
async function loadEntries() {
  if (!currentProjectId.value) {
    entries.value = []
    return
  }
  listLoading.value = true
  const ungrouped = currentDomainId.value === UNGROUPED
  try {
    const list = (await ListApiEntries({
      projectId: currentProjectId.value,
      // 后端没有「按空领域查」的入参（domainId='' 就是不过滤），所以未归领域只能取全项目再前端筛
      domainId: ungrouped ? '' : currentDomainId.value,
      keyword: keyword.value.trim(),
      includeInternal: includeInternal.value,
      includeRemoved: includeRemoved.value
    })) || []
    entries.value = ungrouped ? list.filter(e => !e.domainId) : list
  } catch (err) {
    entries.value = []
    message.error(String(err))
  } finally {
    listLoading.value = false
  }
}

/** 切换项目：领域与列表都要重来，详情指向清空 */
async function selectProject(p) {
  if (currentProjectId.value === p.id) return
  currentProjectId.value = p.id
  currentEntryId.value = ''
  await loadDomains()
  await loadEntries()
}

/** 事件页的项目下拉：只给 id（那里没有中栏/详情，不必清详情选中） */
async function selectProjectById(id) {
  const p = projects.value.find(x => x.id === id)
  if (p) await selectProject(p)
}

function selectDomain(id) {
  if (currentDomainId.value === id) return
  currentDomainId.value = id
  loadEntries()
}

function selectEntry(e) {
  currentEntryId.value = e.id
}

/** 选中项目后首次进视图的初始化：项目 → 领域 → 列表三段串起来 */
async function bootstrap() {
  await loadProjects()
  if (!currentProjectId.value && projects.value.length) {
    currentProjectId.value = projects.value[0].id
  }
  if (!currentProjectId.value) return
  await loadDomains()
  await loadEntries()
}

onMounted(() => {
  bootstrap().catch(err => message.error(String(err)))
})

/** 详情/列表刷新：接口条目发生手工编辑或软删/恢复后重拉，并把选中指向 fresh 对象
 *  （照 SkillManager.vue:60-66 的竞态论证：右栏一直可见，重拉期间用户可能已切换条目，
 *   所以只在 id 不变时同步详情，切走了就不动） */
async function onEntriesChanged() {
  const keep = currentEntryId.value
  await Promise.all([loadDomains(), loadEntries()])
  const fresh = entries.value.find(e => e.id === keep)
  if (fresh) currentEntryId.value = fresh.id
}

/** 导入完成后的全量刷新：项目可能新建、领域可能改名，所以三段都重来 */
async function onImported() {
  screen.value = 'browse'
  tab.value = 'entries'
  await loadProjects()
  await Promise.all([loadDomains(), loadEntries()])
}

/** 提交成功后只后台重拉数据，不切视图：导入向导的结果卡要留在屏上供核对
 *  （否则用户点完「导入」立刻被拽回列表，看不到事件 id/seq/stats） */
async function onImportRefresh() {
  await loadProjects()
  await Promise.all([loadDomains(), loadEntries()])
}

// ---------------- 领域改名 / 删除 ----------------
function startRename(d) {
  renamingId.value = d.id
  renameValue.value = d.name
}

function cancelRename() {
  renamingId.value = ''
  renameValue.value = ''
}

/**
 * 领域改名：appsvc.SaveApiDomain 是服务端读-改-写白名单，只覆盖 name/sort
 * （spec-draft §13.21/§13.22；internal/notesRaw 由文档章节决定，手工编辑一律沿用库里值）。
 * 三条刻意在前端拦掉的假动作（后端不会报错，但会静默什么都不做）：
 *   · 空名 = 后端语义「不改名」→ 前端直接拒绝，免得提示「已保存」而名字没变
 *   · 同名 = 无操作 → 直接收起输入框
 *   · 空 id = 后端中文拒绝「不支持手工新建领域」→ 这里根本没有新建入口（D19）
 * 撞 UNIQUE(project_id, name)（与别的领域重名）由 store 给中文错误，原样展示。
 */
async function renameDomain(d) {
  const next = renameValue.value.trim()
  if (!next) {
    message.warning('领域名不能为空：服务端把空值当作「不改名」，这里不发请求')
    return
  }
  if (next === d.name) {
    cancelRename()
    return
  }
  if (domainSaving.value) return
  domainSaving.value = true
  try {
    await SaveApiDomain({id: d.id, projectId: d.projectId, name: next, sort: d.sort || 0})
    message.success(`领域已改名为「${next}」`)
    cancelRename()
    await loadDomains(true)
    await loadEntries()
  } catch (err) {
    message.error(String(err)) // 后端中文错误原样展示（重名/不支持移动等）
  } finally {
    domainSaving.value = false
  }
}

/**
 * 删除领域：后端只允许删空领域（DeleteApiDomain 内部 CountApiEntriesInDomain active>0 即拒）。
 * 前端不预判拦截 —— 非空时后端会回「该领域下还有 N 个接口，无法删除…」，
 * 那句中文比我们自己数出来的更有信息量（spec-draft §13.23）。
 * 删空领域后，该领域下的软删条目 domainId 会被置 ''（下次导入自愈重挂），
 * 所以左栏始终给「未归领域」入口，这些条目不许消失也不许崩。
 */
async function removeDomain(d) {
  if (domainDeleting.value) return
  domainDeleting.value = d.id
  try {
    await DeleteApiDomain(d.id)
    message.success(`已删除空领域「${d.name}」`)
    if (renamingId.value === d.id) cancelRename()
    await loadDomains(true)
    await loadEntries()
  } catch (err) {
    message.error(String(err)) // 「该领域下还有 N 个接口，无法删除…」原样展示
  } finally {
    domainDeleting.value = ''
  }
}

// ---------------- 顶部按钮 ----------------
/** 导出规范：调 ExportApiDocSpec 拿全文（与 skill 内置文本字节一致，AC9），
 *  写剪贴板走共用 helper copyText（双路降级实现与说明见 lib/clipboard.js，
 *  与 ChatApp 代码块标题栏的「复制」同一实现，不留两份）；两条路都失败才降级为弹窗展示全文 */
async function exportSpec() {
  exporting.value = true
  try {
    const text = (await ExportApiDocSpec()) || ''
    if (!text) {
      message.error('导出失败：后端返回空内容')
      return
    }
    if (await copyText(text)) {
      message.success(`规范全文已复制（${text.length} 字符），可存入业务仓库作为 api-doc-spec.md`)
    } else {
      specText.value = text
      showSpecModal.value = true
      message.warning('剪贴板不可用，已弹出全文供手动复制')
    }
  } catch (err) {
    message.error(String(err))
  } finally {
    exporting.value = false
  }
}

/** 删除项目：手写级联清 9 张 api_* 表（后端实现），不可恢复 —— 用 n-popconfirm 二次确认 */
async function removeProject() {
  const p = currentProject.value
  if (!p) return
  deletingProject.value = true
  try {
    await DeleteApiProject(p.id)
    message.success(`已删除项目「${p.name}」及其全部接口与变更事件`)
    currentProjectId.value = ''
    currentDomainId.value = ''
    currentEntryId.value = ''
    domains.value = []
    entries.value = []
    await loadProjects()
    if (projects.value.length) {
      currentProjectId.value = projects.value[0].id
      await loadDomains()
      await loadEntries()
    }
  } catch (err) {
    message.error(String(err))
  } finally {
    deletingProject.value = false
  }
}

// ---------------- 跨 tab 跳转 ----------------
/** 手动切 tab：清掉跳转意图（点「变更事件」是要看列表，不是重回上条事件） */
function switchTab(t) {
  if (tab.value === t) return
  tab.value = t
  pendingEventId.value = 0
}

/**
 * ApiEntryDetail 的历史事件行 / ApiDocImport 的结果卡都冒泡到这里（PR4a 留的落点）。
 * eventId 为 0 时只切 tab。ApiChangeEvents 用 :key 挂这个 id，保证「同 tab 内再次跳转」
 * 也会重挂载并消费新意图（正常情况下 tab 从 entries→events 本来就会挂载一次，
 * key 只是兜住 events 已挂载时的重复跳转）。
 */
function openEvent(payload) {
  screen.value = 'browse'
  tab.value = 'events'
  pendingEventId.value = Number(payload?.eventId || 0)
}

/**
 * 事件详情里的「查看接口」→ 切回接口 tab 并选中该条目。
 * 目标条目常被默认过滤挡住（事件明细会引用 internal 端点与已软删的接口），
 * 第一次找不到就自动开两个开关再拉一次，并告诉用户开关被动过了。
 */
async function openEntryFromEvent(payload) {
  const id = payload?.entryId
  tab.value = 'entries'
  pendingEventId.value = 0
  if (!id) return
  currentDomainId.value = ''
  keyword.value = ''
  await loadEntries()
  if (!entries.value.some(e => e.id === id)) {
    includeInternal.value = true
    includeRemoved.value = true
    await loadEntries()
    if (entries.value.some(e => e.id === id)) {
      message.info('已自动开启「显示内部端点 / 显示已删除」以定位该接口')
    }
  }
  if (entries.value.some(e => e.id === id)) {
    currentEntryId.value = id
  } else {
    currentEntryId.value = ''
    message.warning('该接口在项目列表中已不可见（可能属于其他项目或已被彻底删除）')
  }
}

// ---------------- 展示辅助 ----------------
function methodClass(m) {
  const s = (m || '').toUpperCase()
  if (s === 'GET') return 'm-get'
  if (s === 'POST' || s === 'PUT') return 'm-write'
  if (s === 'DELETE') return 'm-del'
  return 'm-other'
}

/** 条目的领域面包屑：domainId==='' 或指向已删领域都渲染成「未归领域」，不许崩（§13.23） */
function domainCrumb(id) {
  if (!id) return '未归领域'
  return domainNameById.value[id] || '未归领域（领域已删除）'
}
</script>

<template>
  <div class="apidoc-view">
    <!-- ===== 导入视图：整块替换浏览视图（返回列表由 @cancel 触发） ===== -->
    <template v-if="screen === 'import'">
      <div class="import-head">
        <n-button size="small" quaternary @click="screen = 'browse'">← 返回列表</n-button>
        <span class="import-title">导入接口变更文档</span>
      </div>
      <ApiDocImport
        class="import-body"
        :projects="projects"
        @done="onImported"
        @refresh="onImportRefresh"
        @cancel="screen = 'browse'"
        @open-event="openEvent"
      />
    </template>

    <!-- ===== 浏览视图：tab + 三栏 ===== -->
    <template v-else>
      <div class="apidoc-head">
        <!-- 顶层 tab：接口 / 变更事件（按钮式，见文件头注释里的取舍） -->
        <div class="tab-row">
          <span class="tab" :class="{active: tab === 'entries'}" @click="switchTab('entries')">接口</span>
          <span class="tab" :class="{active: tab === 'events'}" @click="switchTab('events')">变更事件</span>
        </div>
        <div class="head-actions">
          <n-button size="small" type="primary" ghost @click="screen = 'import'">导入文档</n-button>
          <n-button size="small" :loading="exporting" @click="exportSpec">导出规范</n-button>
          <n-popconfirm
            v-if="currentProject"
            positive-text="删除项目"
            negative-text="取消"
            :positive-button-props="{loading: deletingProject}"
            @positive-click="removeProject"
          >
            <template #trigger>
              <n-button size="small" type="error" ghost :disabled="!currentProject">删除项目</n-button>
            </template>
            将级联清空该项目下的领域、接口条目、字段、命名结构、示例、变更事件、事件明细与
            导入批次（9 张表全清），不可恢复。确定删除「{{ currentProject.name }}」？
          </n-popconfirm>
        </div>
      </div>

      <!-- ---- 变更事件 tab：列表（seq DESC + 按 label 聚合）+ 详情（reason/明细/字段级 diff/原文 <pre>）
             :key 挂跳转意图，保证重复跳转也重新挂载并消费新的 eventId（见 openEvent 注释） ---- -->
      <ApiChangeEvents
        v-if="tab === 'events'"
        :key="'ev-' + pendingEventId"
        :projects="projects"
        :project-id="currentProjectId"
        :initial-event-id="pendingEventId"
        @select-project="selectProjectById"
        @open-entry="openEntryFromEvent"
      />

      <!-- ---- 接口 tab：三栏 flex ---- -->
      <div v-else class="apidoc-cols">
        <!-- ===== 左栏：项目 + 领域 + 搜索 ===== -->
        <aside class="col-left">
          <div class="col-title">项目</div>
          <div class="proj-list">
            <div
              v-for="p in projects" :key="p.id"
              class="row-item" :class="{active: p.id === currentProjectId}"
              @click="selectProject(p)"
            >
              <span class="row-main">{{ p.display || p.name }}</span>
            </div>
            <div v-if="!projects.length" class="col-empty">还没有项目，点右上「导入文档」创建。</div>
          </div>

          <template v-if="currentProjectId">
            <div class="col-title domain-title">领域</div>
            <div class="domain-list">
              <div class="row-item" :class="{active: currentDomainId === ''}" @click="selectDomain('')">
                <span class="row-main">全部领域</span>
              </div>
              <div
                v-for="d in domains" :key="d.id"
                class="row-item" :class="{active: d.id === currentDomainId}"
                @click="selectDomain(d.id)"
              >
                <!-- 改名态：整行换成输入框（Enter 保存 / Esc 取消）；操作按钮全部 @click.stop，
                     否则会冒泡到行上的 selectDomain -->
                <template v-if="renamingId === d.id">
                  <n-input
                    v-model:value="renameValue"
                    size="tiny"
                    placeholder="新领域名"
                    @click.stop
                    @keyup.enter="renameDomain(d)"
                    @keyup.esc="cancelRename"
                  />
                  <n-button size="tiny" text type="primary" :loading="domainSaving" @click.stop="renameDomain(d)">
                    存
                  </n-button>
                  <n-button size="tiny" text @click.stop="cancelRename">取消</n-button>
                </template>
                <template v-else>
                  <span class="row-main" :title="d.name">{{ d.name }}</span>
                  <span v-if="d.internal" class="row-flag">内部</span>
                  <span class="row-acts" @click.stop>
                    <n-button size="tiny" quaternary :disabled="domainSaving" @click="startRename(d)">改名</n-button>
                    <n-popconfirm
                      positive-text="删除领域"
                      negative-text="取消"
                      :positive-button-props="{loading: domainDeleting === d.id}"
                      @positive-click="removeDomain(d)"
                    >
                      <template #trigger>
                        <n-button size="tiny" quaternary type="error" :disabled="!!domainDeleting">删</n-button>
                      </template>
                      只允许删除空领域：领域下还有接口时后端会直接拒绝并告诉你还剩几个。
                      要合并领域请到「导入文档」的预览里做（PRD D19）。
                    </n-popconfirm>
                  </span>
                </template>
              </div>

              <!-- 未归领域：删空领域后残余软删条目的 domainId 会变 ''（spec-draft §13.23），
                   这些条目必须有归属可看，不许从界面上消失 -->
              <div class="row-item" :class="{active: currentDomainId === UNGROUPED}" @click="selectDomain(UNGROUPED)">
                <span class="row-main">未归领域</span>
                <span v-if="ungroupedCount !== null" class="row-count mono">{{ ungroupedCount }}</span>
              </div>

              <div v-if="!domains.length" class="col-empty">该项目暂无领域</div>
            </div>
            <div class="domain-foot">领域由文档一级章节产生，只能改名或删除空领域。</div>

            <div class="col-title domain-title">搜索</div>
            <n-input
              v-model:value="keyword"
              size="small"
              placeholder="接口名称或路径，回车搜索"
              clearable
              @keyup.enter="loadEntries"
              @update:value="(v) => { if (v === '') loadEntries() }"
            />
          </template>
        </aside>

        <!-- ===== 中栏：接口列表 ===== -->
        <section class="col-mid">
          <div class="mid-head">
            <span class="col-title no-margin">接口（{{ entries.length }}）</span>
            <div class="switch-row">
              <n-checkbox v-model:checked="includeInternal" @update:checked="loadEntries">
                显示内部端点
              </n-checkbox>
              <n-checkbox v-model:checked="includeRemoved" @update:checked="loadEntries">
                显示已删除
              </n-checkbox>
            </div>
          </div>
          <div v-if="!currentProjectId" class="col-empty pad">请先在左栏选择项目。</div>
          <template v-else>
            <div v-if="listLoading" class="col-empty pad">加载中…</div>
            <div
              v-for="e in entries" :key="e.id"
              class="entry-row" :class="{active: e.id === currentEntryId, removed: e.status === 'removed'}"
              @click="selectEntry(e)"
            >
              <div class="entry-line1">
                <span class="method-badge" :class="methodClass(e.method)">{{ e.method }}</span>
                <span class="entry-path mono">{{ e.pathRaw }}</span>
              </div>
              <div class="entry-line2">
                <span class="entry-name">{{ e.name || '（无中文名）' }}</span>
                <!-- 领域面包屑：当前列表没按该领域筛选时才显示（避免重复信息），
                     domainId 为空或指向已删领域都渲染「未归领域」，不许崩（§13.23） -->
                <span
                  v-if="currentDomainId !== e.domainId"
                  class="ps ps-domain" :class="{'ps-nodomain': !e.domainId}"
                >{{ domainCrumb(e.domainId) }}</span>
                <!-- parseState 标记：partial/unparsed 必须可见（R10「可见不静默」） -->
                <span v-if="e.parseState === 'partial'" class="ps ps-partial">部分未解析</span>
                <span v-else-if="e.parseState === 'unparsed'" class="ps ps-unparsed">未解析</span>
                <span v-if="e.visibility === 'internal'" class="ps ps-internal">内部</span>
                <span v-if="e.status === 'removed'" class="ps ps-removed">已删除</span>
              </div>
            </div>
            <div v-if="!entries.length && !listLoading" class="col-empty pad">没有符合条件的接口。</div>
          </template>
        </section>

        <!-- ===== 右栏：详情 ===== -->
        <section class="col-right">
          <ApiEntryDetail :entry="currentEntry" @changed="onEntriesChanged" @open-event="openEvent"/>
        </section>
      </div>
    </template>

    <!-- 导出规范降级弹窗：剪贴板不可用时展示全文（v-if + :show="true" 关闭即销毁） -->
    <n-modal
      v-if="showSpecModal"
      :show="true"
      preset="card"
      title="接口变更文档规范（api-doc-spec.md）"
      style="width: 760px"
      :segmented="{content: true}"
      @update:show="showSpecModal = false"
    >
      <div class="spec-body">
        <div class="spec-tip">
          与 chat skill 内置的规范文本同源同字节（PRD D15/AC9）。请保存为业务仓库的
          <code v-pre>api-doc-spec.md</code>，供外部 agent 按规范产出变更文档。
        </div>
        <n-code :code="specText" language="markdown" word-wrap/>
      </div>
    </n-modal>
  </div>
</template>

<style scoped>
/* 根节点：视图挂载在 ChatApp 的 .main（flex:1 + flex-column）下，自己声明高度与滚动（§1.2 注） */
.apidoc-view { display: flex; flex-direction: column; height: 100%; min-width: 0; overflow: hidden; }

/* ---- 顶部：tab + 全局按钮 ---- */
.apidoc-head {
  display: flex; align-items: center; justify-content: space-between; gap: 10px;
  padding: 10px 16px; border-bottom: 1px solid rgba(255,255,255,0.08); flex-shrink: 0;
}
.tab-row { display: flex; gap: 14px; }
.tab {
  font-size: 13px; color: rgba(255,255,255,0.5); cursor: pointer; padding: 4px 2px;
  border-bottom: 2px solid transparent;
}
.tab:hover { color: rgba(255,255,255,0.8); }
.tab.active { color: rgba(99,226,183,1); border-bottom-color: rgba(99,226,183,0.6); font-weight: 600; }
.head-actions { display: flex; gap: 8px; }

/* ---- 两栏/三栏：普通 flex div；每栏自己 overflow，min-width:0 保证省略号生效 ---- */
.apidoc-cols { flex: 1; display: flex; min-height: 0; min-width: 0; }
.col-left {
  width: 240px; flex-shrink: 0; overflow-y: auto; padding: 10px 8px;
  border-right: 1px solid rgba(255,255,255,0.08); display: flex; flex-direction: column; gap: 4px;
}
.col-mid {
  width: 320px; flex-shrink: 0; overflow-y: auto; padding: 10px 8px;
  border-right: 1px solid rgba(255,255,255,0.08); display: flex; flex-direction: column;
}
.col-right { flex: 1; min-width: 0; overflow: hidden; }

.col-title { font-size: 12px; color: rgba(255,255,255,0.45); margin: 2px 0 6px; font-weight: 600; }
.domain-title { margin-top: 14px; }
.no-margin { margin: 0; }
.col-empty { font-size: 12px; color: rgba(255,255,255,0.4); line-height: 1.6; }
.pad { padding: 8px; }

/* 行条目：选中态沿用仓库绿色（ChatApp.vue:871） */
.row-item {
  display: flex; align-items: center; gap: 6px; padding: 6px 8px; border-radius: 6px;
  cursor: pointer; font-size: 12px; min-width: 0;
}
.row-item:hover { background: rgba(255,255,255,0.06); }
.row-item.active { background: rgba(99,226,183,0.15); }
.row-main { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  color: rgba(255,255,255,0.8); }
.row-flag { flex-shrink: 0; font-size: 11px; color: rgba(240,168,48,0.9); }
.row-count { flex-shrink: 0; font-size: 11px; color: rgba(255,255,255,0.45); }

/* 领域行的操作区：改名 / 删除空领域（D19 只有这两个，没有新建入口） */
.row-acts { display: flex; align-items: center; gap: 2px; flex-shrink: 0; }
.row-acts :deep(.n-button) { padding: 0 3px; font-size: 11px; }

/* 领域列表底部说明（一句话讲清能力边界，避免用户去找「+新建领域」按钮） */
.domain-foot { font-size: 11px; color: rgba(255,255,255,0.35); line-height: 1.6; padding: 2px 8px 0; }

/* 改名态的输入框要在窄栏里撑开：n-input 在 flex 容器里默认按内容取宽，会挤成一坨 */
.domain-list :deep(.n-input) { flex: 1; min-width: 0; }
.domain-list .row-item :deep(.n-button) { flex-shrink: 0; }

/* ---- 接口列表行 ---- */
.mid-head { display: flex; flex-direction: column; gap: 6px; margin-bottom: 8px; flex-shrink: 0; }
.switch-row { display: flex; gap: 10px; }
.switch-row :deep(.n-checkbox) { font-size: 12px; }
.entry-row {
  padding: 7px 8px; border-radius: 6px; cursor: pointer; font-size: 12px; min-width: 0;
  border: 1px solid transparent;
}
.entry-row:hover { background: rgba(255,255,255,0.06); }
.entry-row.active { background: rgba(99,226,183,0.15); border-color: rgba(99,226,183,0.35); }
.entry-row.removed { opacity: .6; }
.entry-line1 { display: flex; align-items: center; gap: 6px; min-width: 0; }
.entry-path { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  color: rgba(255,255,255,0.85); font-size: 12px; }
.entry-line2 { display: flex; align-items: center; gap: 6px; margin-top: 3px; min-width: 0; flex-wrap: wrap; }
.entry-name { color: rgba(255,255,255,0.55); overflow: hidden; text-overflow: ellipsis;
  white-space: nowrap; max-width: 100%; }
.mono { font-family: Consolas, Menlo, monospace; }

.method-badge {
  flex-shrink: 0; font-family: Consolas, Menlo, monospace; font-size: 10px; font-weight: 700;
  padding: 1px 5px; border-radius: 4px; letter-spacing: .5px; min-width: 42px; text-align: center;
}
.m-get { background: rgba(99,226,183,0.18); color: rgba(99,226,183,1); }
.m-write { background: rgba(99,179,237,0.18); color: rgba(99,179,237,1); }
.m-del { background: rgba(224,108,117,0.18); color: rgba(224,108,117,1); }
.m-other { background: rgba(240,168,48,0.18); color: rgba(240,168,48,1); }

/* 状态小标记 */
.ps { flex-shrink: 0; font-size: 10px; padding: 1px 5px; border-radius: 4px; }
.ps-partial, .ps-unparsed { background: rgba(240,168,48,0.15); color: rgba(240,168,48,1); }
.ps-internal { background: rgba(99,179,237,0.15); color: rgba(99,179,237,1); }
.ps-removed { background: rgba(224,108,117,0.15); color: rgba(224,108,117,1); }
.ps-domain { background: rgba(255,255,255,0.06); color: rgba(255,255,255,0.5); max-width: 120px;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ps-nodomain { background: rgba(240,168,48,0.12); color: rgba(240,168,48,0.9); }

/* ---- 导入视图 ---- */
.import-head {
  display: flex; align-items: center; gap: 10px; padding: 10px 16px;
  border-bottom: 1px solid rgba(255,255,255,0.08); flex-shrink: 0;
}
.import-title { font-size: 14px; font-weight: 600; color: rgba(255,255,255,0.85); }
/* 滚动交给父侧这个 flex item 承担：子组件根节点不声明 height:100%
   （百分比高度对 flex:1 的 item 不保证生效，会让长文档底部滚不出来） */
.import-body { flex: 1; min-height: 0; overflow-y: auto; }

/* ---- 规范降级弹窗 ---- */
.spec-body { max-height: 70vh; overflow-y: auto; }
.spec-tip { font-size: 12px; color: rgba(255,255,255,0.55); line-height: 1.7; margin-bottom: 10px; }
.spec-tip code { font-family: Consolas, Menlo, monospace; color: rgba(99,226,183,1); }
.spec-body :deep(pre) { font-size: 12px; white-space: pre-wrap; word-break: break-all; }
</style>
