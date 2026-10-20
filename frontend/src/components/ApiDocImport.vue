<!--
  ApiDocImport.vue —— 接口变更文档导入向导（规范驱动导入的界面入口，PRD R8）。
  整体链路照搬 DeployFlowConfig.vue 的导入实现（隐藏 input[type=file] + FileReader /
  内联红底错误列表 / 逐条汇总），但写库由后端单事务完成，前端只负责「预览 → 确认 → 提交」。

  布局结构（单列纵向滚动，三段）：
    ① 输入区：选文件 / 粘贴全文 / 来源路径 / 目标项目 / 初始化勾选 → 「解析预览」
    ② 预览区（PreviewApiDocImport 的只读产物，分栏呈现，PRD Risk 5）：
       汇总 → **meta 对照表**（文档声明 vs 本次覆盖，两栏并排：preview.meta 是原样回显，
       不与覆盖值合并，spec-draft §13.24）→ 错误区 E-*（红，禁用提交）→ 警告区 W-*（琥珀，
       不阻断）→ 变更预览（added/updated/removed/restored 分组，updated 可展开字段级 diff，
       交给 ApiDiffList 渲染）→ 领域候选（可改名/合并）→ init 闸门说明（initBlocked 时给勾选）
    ③ 提交区：needConfirm（破坏性或含删除）必须勾选确认后才带 confirmed=true 提交；
       提交成功后展示 ApiDocImportResult：message / stats / notes（幂等短路的陈旧声明降级提示）
       / createdProject / createdDomains，并在用过改名或合并 override 时探测「旧名空领域」，
       给一行「删除旧领域」按钮（§13.25：绝不高静默自动删）

  数据来源：PreviewApiDocImport / CommitApiDocImport（bindings 桩）；提交后用 ListApiDomains +
  ListApiEntries 只读探测旧名空领域，用户点确认才调 DeleteApiDomain。
  props.projects 由父侧已经拉好的 ListApiProjects 结果传入（只为「目标项目」下拉提供候选，
  避免重复 RPC）。

  props / emit 契约：
    props: {projects: ApiProject[]} —— 只为「目标项目」下拉提供候选（父侧已拉过，避免重复 RPC）
    emit('done')                —— 用户点「返回列表」：父侧切回浏览视图并重拉项目/领域/列表
    emit('refresh')             —— 提交成功后：父侧后台重拉项目/领域/列表，但**不切视图**
                                  （结果卡要留在屏上供核对「查看该事件」）
    emit('cancel')              —— 放弃导入返回
    emit('openEvent', {eventId})—— 「查看该事件」：父侧切到「变更事件」tab 并直接打开该事件详情

  红线：本组件不用 v-html、不引入 markdown 渲染、不试图在前端绕过破坏性确认闸
  （后端 CommitApiDocImport 同样会拒，前端只是把闸显式化）。
-->
<script setup>
import {computed, ref} from 'vue'
import {useMessage} from 'naive-ui'
import {
  PreviewApiDocImport, CommitApiDocImport,
  ListApiDomains, ListApiEntries, DeleteApiDomain
} from '../../bindings/DevCraft/app.js'
import ApiDiffList from './ApiDiffList.vue'
import ApiIssueList from './ApiIssueList.vue'

const props = defineProps({
  projects: {type: Array, default: () => []}
})
const emit = defineEmits(['done', 'cancel', 'refresh', 'openEvent'])
const message = useMessage()

// ---------------- ① 输入区 ----------------
const docText = ref('')          // 文档全文（选文件后也回填到这里，统一入口）
const docRef = ref('')           // 来源路径（可空，仅溯源）
const projectName = ref('')      // 覆盖 meta.project；'' = 用文档 meta 值
const initMode = ref(false)      // 解析档位的唯一权威（§13.26）：无 meta 的散文文档靠它兜底；文档自声明 scope 与它冲突则整批被拒
const allowInit = ref(false)     // 非空项目放行 init（§13.12 闸门键 = allowInitOnNonEmpty）
const previewing = ref(false)
const committing = ref(false)

const fileInput = ref(null)

function pickFile() {
  fileInput.value?.click()
}

/** FileReader 读成文本（沿用 DeployFlowConfig.vue:124-133：读完必须清空 input.value，
 *    否则重选同一文件不再触发 change） */
function onFilePicked(e) {
  const file = e.target.files?.[0]
  e.target.value = ''
  if (!file) return
  const reader = new FileReader()
  reader.onload = () => {
    docText.value = String(reader.result || '')
    if (!docRef.value) docRef.value = file.name // 未手填来源时用地名兜底，便于溯源
  }
  reader.onerror = () => message.error('读取文件失败')
  reader.readAsText(file, 'utf-8')
}

/** 目标项目下拉：候选来自父侧已加载列表；'' = 跟随文档 meta.project */
const projectOptions = computed(() => [
  {label: '跟随文档 meta 的 project', value: ''},
  ...props.projects.map(p => ({label: `${p.display || p.name}（导入到 ${p.name}）`, value: p.name}))
])

const KIND_OPTIONS = [
  {label: '跟随文档 meta 的 change_type', value: ''},
  {label: 'feature（新功能）', value: 'feature'},
  {label: 'fix（缺陷修复）', value: 'fix'},
  {label: 'refactor（重构）', value: 'refactor'},
  {label: 'docs（仅文档）', value: 'docs'}
]

// ---------------- ② 预览结果 ----------------
const preview = ref(null)        // appsvc.ApiDocImportPreview
const domainEdits = ref({})      // rawTitle -> {name, mergeInto}
/** rawTitle -> 文档归一出来的初始终名（第一次预览时的快照，用于识别改名/合并留下的旧名领域行） */
const domainBase = ref({})
const expandedKeys = ref([])     // 展开的 updated 行 key（method|path）
const confirmed = ref(false)     // 破坏性二次确认勾选
const result = ref(null)         // appsvc.ApiDocImportResult（提交后）
const staleDomains = ref([])     // 提交后探测到的「旧名空领域」候选：[{id, name}]
const staleDetecting = ref(false)
const staleDeleting = ref('')    // 正在删除的旧领域 id（行级 loading）

/** meta 覆盖输入（空 = 用文档 meta 值，界面允许改，spec-draft §2） */
const ovTitle = ref('')
const ovReason = ref('')
const ovLabel = ref('')
const ovKind = ref('')

const errors = computed(() => (preview.value?.issues || []).filter(i => i.level === 'E'))
const warnings = computed(() => (preview.value?.issues || []).filter(i => i.level === 'W'))
const notes = computed(() => preview.value?.notes || [])

// ---------------- meta 对照（文档声明 vs 本次覆盖） ----------------
/**
 * preview.meta 是「原样回显」，后端不与界面覆盖值合并（spec-draft §13.24），
 * 所以这里必须两栏并排，让用户自己看清最终生效的是哪一个：
 *   文档声明 = preview.meta.<key>（散文文档没有 ```apidoc-meta 块 → 8 个字段全空串，
 *              判据就是「全空串」，DTO 里没有 present 布尔位）
 *   本次覆盖 = 当前输入框的实时值（project/kind/title/reason/label 五项有覆盖入口，
 *              service/basePath/scope 没有 —— 那三档由文档决定）
 * 生效规则与后端 apiDocFirstNonEmpty 一致：覆盖值非空则优先，否则用文档声明。
 */
const META_FIELDS = [
  {key: 'project', label: 'project（项目标识，D13）', overrideKey: 'project'},
  {key: 'service', label: 'service（项目显示名）', overrideKey: ''},
  {key: 'basePath', label: 'base_path（只断言不剥离，D18）', overrideKey: ''},
  {key: 'scope', label: 'scope（导入档位）', overrideKey: ''},
  {key: 'changeType', label: 'change_type（事件 kind）', overrideKey: 'kind'},
  {key: 'changeTitle', label: 'change_title（事件标题）', overrideKey: 'title'},
  {key: 'changeNote', label: 'change_note（为什么改）', overrideKey: 'reason'},
  {key: 'versionTag', label: 'version_tag（事件 label，D7）', overrideKey: 'label'}
]

const declaredMeta = computed(() => preview.value?.meta || {})

/** 「文档未声明（散文文档）」= 8 个字段全空串（没有 present 位可用，§13.24） */
const metaUndeclared = computed(() =>
  META_FIELDS.every(f => !String(declaredMeta.value[f.key] || '').trim()))

function overrideValueOf(key) {
  if (key === 'project') return projectName.value.trim()
  if (key === 'kind') return ovKind.value.trim()
  if (key === 'title') return ovTitle.value.trim()
  if (key === 'reason') return ovReason.value.trim()
  if (key === 'label') return ovLabel.value.trim()
  return ''
}

const metaRows = computed(() => META_FIELDS.map((f) => {
  const declared = String(declaredMeta.value[f.key] || '').trim()
  const override = f.overrideKey ? overrideValueOf(f.overrideKey) : ''
  return {
    key: f.key,
    label: f.label,
    hasEntry: !!f.overrideKey,
    declared,
    override,
    effective: override || declared,
    overridden: !!override && override !== declared
  }
}))

/** 被本次覆盖改掉的字段数（汇总行给一眼可见的数字，免得用户以为覆盖没生效） */
const overriddenCount = computed(() => metaRows.value.filter(r => r.overridden).length)

/** 变更分组：added/updated/removed/restored/unchanged；
 *  moved 在 MVP 增量导入永不出现（§13.16），因此不给它专门分区，
 *  但兜底分区 other 会把任何未知 changeType 显示出来，绝不静默丢行。 */
const KNOWN_TYPES = ['added', 'updated', 'removed', 'restored', 'unchanged']
const GROUP_LABEL = {
  added: '新增接口', updated: '修改接口', removed: '删除接口',
  restored: '恢复接口', unchanged: '未变更', other: '其他'
}
const changeGroups = computed(() => {
  const changes = preview.value?.changes || []
  const out = []
  for (const t of KNOWN_TYPES) {
    const rows = changes.filter(c => (c.changeType || '') === t)
    if (rows.length) out.push({key: t, label: GROUP_LABEL[t], rows})
  }
  const other = changes.filter(c => !KNOWN_TYPES.includes(c.changeType || ''))
  if (other.length) out.push({key: 'other', label: GROUP_LABEL.other, rows: other})
  return out
})

/** 需要人工过目的危险行：破坏性(1)/需确认(2)/删除 */
const riskRows = computed(() => (preview.value?.changes || [])
  .filter(c => c.breaking === 1 || c.breaking === 2 || c.changeType === 'removed'))

const hasDiff = (c) => (c.changeType === 'updated') && (c.diff || []).length > 0
function rowKey(c) {
  return `${c.method}|${c.path}`
}
function toggleRow(c) {
  const k = rowKey(c)
  const i = expandedKeys.value.indexOf(k)
  if (i >= 0) expandedKeys.value.splice(i, 1)
  else expandedKeys.value.push(k)
}

function breakingText(b) {
  if (b === 1) return '破坏性'
  if (b === 2) return '需人工确认'
  return ''
}
function changeTypeText(t) {
  return {added: '新增', updated: '修改', removed: '删除', restored: '恢复', moved: '迁移', unchanged: '未变更'}[t] || t || ''
}

// ---------------- 请求组装 ----------------
/** 领域改名/合并指令：与「文档归一的初名」比较，只提交真正改过档的项。
 *  绝不能拿 d.name 当基准 —— preview.domains[].name 是**应用 override 后**的终名，
 *  用它比较会让「改完名点一次重新预览」把 override 判成没改动而静默丢弃（提交时改名丢失）。 */
function overridesOf() {
  const edits = domainEdits.value
  const out = []
  for (const d of preview.value?.domains || []) {
    const e = edits[d.rawTitle]
    if (!e) continue
    const name = (e.name || '').trim()
    const mergeInto = (e.mergeInto || '').trim()
    const base = (domainBase.value[d.rawTitle] ?? d.name ?? '').trim()
    if (mergeInto) {
      if (mergeInto !== base) out.push({rawTitle: d.rawTitle, name: '', mergeInto})
    } else if (name && name !== base) {
      out.push({rawTitle: d.rawTitle, name, mergeInto: ''})
    }
  }
  return out
}

/** 组装 ApiDocImportRequest（预览与提交共用同一份输入；confirmed 只在提交时为 true） */
function buildRequest(withConfirm) {
  return {
    docText: docText.value,
    docRef: docRef.value.trim(),
    projectName: projectName.value.trim(),
    initMode: initMode.value,
    allowInitOnNonEmpty: allowInit.value,
    confirmed: !!withConfirm,
    title: ovTitle.value.trim(),
    reason: ovReason.value.trim(),
    label: ovLabel.value.trim(),
    kind: ovKind.value.trim(),
    domainOverrides: overridesOf()
  }
}

/** 预览（只读，绝不写库）：清掉上一次的提交结果与确认勾选。
 *  silent=true 给勾选框联动用：文档还没内容时静默返回，不弹「请先选择文件」的多余提示 */
async function doPreview(silent) {
  if (!docText.value.trim()) {
    if (!silent) message.warning('请先选择 .md 文件或粘贴文档全文')
    return
  }
  previewing.value = true
  try {
    preview.value = (await PreviewApiDocImport(buildRequest(false))) || null
    confirmed.value = false
    result.value = null
    staleDomains.value = []
    expandedKeys.value = []
    // 记下「文档自己归一出来的终名」：preview.domains[].name 是应用 override 后的终名，
    // 只有首次见到的值才是改名/合并前的旧名 —— 旧名领域行是否残留要靠它来判（§13.25）
    for (const d of preview.value?.domains || []) {
      if (!(d.rawTitle in domainBase.value)) domainBase.value[d.rawTitle] = d.name || ''
    }
    if (errors.value.length) message.warning(`解析发现 ${errors.value.length} 个阻断问题，需修正文档后重新预览`)
  } catch (err) {
    preview.value = null
    message.error(String(err)) // 后端中文错误原样展示（整份文档不合格式等）
  } finally {
    previewing.value = false
  }
}

/** 领域改名/合并后重新预览：领域终名会影响变更分区里的 domain，必须让后端重算 */
function applyDomainEdits() {
  return doPreview(false)
}

// ---------------- 提交闸 ----------------
const canSubmit = computed(() => {
  const p = preview.value
  if (!p || previewing.value || committing.value) return false
  if (!p.ok) return false                                  // E-* 或 init 闸门未放行
  if (p.needConfirm && !confirmed.value) return false      // D16 二次确认未勾选
  return !!docText.value.trim()
})

/** 按钮禁用时的人类可读原因（绝不只把按钮变灰不解释） */
const blockReason = computed(() => {
  const p = preview.value
  if (!p) return '请先「解析预览」。'
  if (errors.value.length) return `存在 ${errors.value.length} 个 E-* 阻断问题：整批不写入，请修正文档后重新预览。`
  if (p.initBlocked) return '初始化模式被拦截：勾选「仍以初始化模式导入」并重新预览。'
  if (!p.ok) return '当前预览结果不允许提交。'
  if (p.needConfirm && !confirmed.value) return '本次导入含破坏性变更或删除，勾选下方确认后才能写入。'
  return ''
})

const commitLabel = computed(() => {
  const p = preview.value
  if (p?.needConfirm) return '我已核对，确认写入（破坏性/删除）'
  return '导入并生成变更事件'
})

async function doCommit() {
  if (!canSubmit.value) {
    if (blockReason.value) message.warning(blockReason.value)
    return
  }
  committing.value = true
  try {
    result.value = (await CommitApiDocImport(buildRequest(true))) || null
    message.success(result.value?.message || '导入完成')
    emit('refresh')   // 后台刷新列表，留在本页展示结果卡（切视图由用户点「返回列表并刷新」）
    await detectStaleDomains() // 改名/合并 override 会留下旧名的空领域行（§13.25）
  } catch (err) {
    // 后端在服务层强制确认闸/init 闸门；这里的中文错误就是「你绕不过去」的答案
    message.error(String(err))
  } finally {
    committing.value = false
  }
}

/**
 * 用过改名/合并 override 时，库里的旧名领域行会残留成空领域（PR3 既有行为：导入按终名 upsert
 * 领域，旧行不动）。后端不会自动删（§13.25「不做静默自动删除」），所以结果卡给一行提示
 * + 一个按钮，由用户决定是否调 DeleteApiDomain。
 * 判据两步：① 旧名在 ListApiDomains 里确实有行；② 该行下 active 条目为 0
 * （ListApiEntries 默认过滤 removed，正与后端 DeleteApiDomain 的 active>0 拒删口径一致）。
 * 探测失败只影响「有没有这条建议」，绝不影响已经写入的导入结果。
 */
function staleCandidateNames() {
  const out = []
  for (const d of preview.value?.domains || []) {
    const base = (domainBase.value[d.rawTitle] || '').trim() // 文档归一的旧名
    if (!base) continue
    const finalName = (d.name || '').trim()                  // 本次落库的终名（含改名/合并）
    if (finalName && finalName !== base && !out.includes(base)) out.push(base)
  }
  return out
}

async function detectStaleDomains() {
  staleDomains.value = []
  const pid = result.value?.projectId || ''
  if (!pid) return
  const olds = staleCandidateNames()
  if (!olds.length) return
  staleDetecting.value = true
  try {
    const ds = (await ListApiDomains(pid)) || []
    for (const name of olds) {
      const d = ds.find(x => x.name === name)
      if (!d) continue // 库里没有旧名领域行（首次导入就用了新名）→ 没有残留要清理
      const alive = (await ListApiEntries({
        projectId: pid, domainId: d.id, keyword: '', includeInternal: true, includeRemoved: false
      })) || []
      if (!alive.length) staleDomains.value.push({id: d.id, name: d.name})
    }
  } catch (err) {
    // 探测只是「给建议」，失败就收起这一行，不冒充成导入失败
    message.warning('旧领域检查未完成：' + String(err))
  } finally {
    staleDetecting.value = false
  }
}

async function removeStaleDomain(d) {
  if (staleDeleting.value) return
  staleDeleting.value = d.id
  try {
    await DeleteApiDomain(d.id)
    staleDomains.value = staleDomains.value.filter(x => x.id !== d.id)
    message.success(`已删除空领域「${d.name}」`)
    emit('refresh')
  } catch (err) {
    message.error(String(err)) // 后端会告诉你还剩几个接口，原样展示
  } finally {
    staleDeleting.value = ''
  }
}

/** stats 徽标（预览与结果共用同一套档位；moved 恒 0（§13.16 MVP 增量导入不产生），刻意不进 UI 展示） */
function chipsOf(s) {
  if (!s) return []
  const out = []
  if (s.added) out.push({label: '新增', value: s.added, cls: 'st-add'})
  if (s.updated) out.push({label: '修改', value: s.updated, cls: 'st-upd'})
  if (s.removed) out.push({label: '删除', value: s.removed, cls: 'st-del'})
  if (s.restored) out.push({label: '恢复', value: s.restored, cls: 'st-res'})
  if (s.unchanged) out.push({label: '未变更', value: s.unchanged, cls: 'st-un'})
  return out
}

const statsChips = computed(() => chipsOf(preview.value?.stats))
/** 结果卡单独用自己的 stats：提交是后端重算的一次独立裁决（幂等短路时 stats 会全 0），
 *  不能拿预览的数字冒充提交结果 */
const resultStatsChips = computed(() => chipsOf(result.value?.stats))

function backToList() {
  emit('done')
}

function viewEvent() {
  const id = result.value?.eventId || 0
  if (!id) {
    message.info('本次导入没有生成事件（无变更或初始化导入）')
    return
  }
  // 父侧（ApiDocManager）切到「变更事件」tab 并用这个 id 直接打开详情
  emit('openEvent', {eventId: id})
}

const resultIsNoop = computed(() => result.value && !result.value.eventId)
/** 提交结果的中文提示（幂等短路的陈旧声明降级就在这里，不新增错误码） */
const resultNotes = computed(() => result.value?.notes || [])
</script>

<template>
  <div class="imp">
    <!-- ===================== ① 输入区 ===================== -->
    <div class="imp-card">
      <div class="imp-title">① 文档输入</div>
      <div class="imp-sub">
        输入必须遵循 DevCraft 接口变更文档规范（三段式 + <code v-pre>```apidoc-meta</code> 块）。
        规范全文可在列表页点「导出规范」获取；导入 = 更新接口定义 + 生成一条变更事件（PRD D4/D10）。
      </div>

      <div class="file-row">
        <n-button size="small" @click="pickFile">选择 .md 文件…</n-button>
        <input ref="fileInput" type="file" accept=".md,.markdown,.txt" class="file-input" @change="onFilePicked"/>
        <span class="hint">选中后自动读入下方文本框；也可以直接粘贴全文</span>
      </div>

      <n-input
        v-model:value="docText"
        class="doc-textarea"
        type="textarea"
        :autosize="{minRows: 8, maxRows: 18}"
        placeholder="粘贴接口变更文档全文（Markdown）"
      />

      <div class="form-grid">
        <div class="fg-item">
          <span class="fg-label">来源路径（可选，仅溯源）</span>
          <n-input v-model:value="docRef" size="small" placeholder="如 docs/apidoc/2026-10-18-user.md"/>
        </div>
        <div class="fg-item">
          <span class="fg-label">目标项目</span>
          <n-select v-model:value="projectName" size="small" :options="projectOptions"/>
        </div>
        <div class="fg-item">
          <span class="fg-label">变更类型覆盖</span>
          <n-select v-model:value="ovKind" size="small" :options="KIND_OPTIONS"/>
        </div>
        <div class="fg-item">
          <span class="fg-label">事件标题 / label 覆盖</span>
          <div class="two-in-row">
            <n-input v-model:value="ovTitle" size="small" placeholder="标题（空 = 用 meta.change_title）"/>
            <n-input v-model:value="ovLabel" size="small" placeholder="label，如 REQ-1024"/>
          </div>
        </div>
        <div class="fg-item span2">
          <span class="fg-label">变更原因覆盖（为什么改）</span>
          <n-input v-model:value="ovReason" size="small" placeholder="空 = 用 meta.change_note"/>
        </div>
      </div>

      <div class="check-row">
        <n-checkbox v-model:checked="initMode" @update:checked="doPreview(true)">
          初始化模式（解析档位的唯一权威，spec-draft §13.26：文档声明 scope: init 时必须勾，
          声明 incremental/full 时勾了会被拒；无 meta 的存量散文文档靠它按 init 解析，推断点逐条打警告）
        </n-checkbox>
      </div>

      <div class="actions">
        <n-button @click="emit('cancel')">返回列表</n-button>
        <n-button type="primary" :loading="previewing" @click="doPreview(false)">解析预览</n-button>
      </div>
    </div>

    <!-- ===================== ② 预览区 ===================== -->
    <template v-if="preview">
      <!-- 汇总 -->
      <div class="imp-card">
        <div class="sum-row">
          <span class="imp-title no-mb">② 解析结果</span>
          <span class="sum-mode mono">{{ preview.mode || '-' }}</span>
          <span class="sum-ok" :class="preview.ok ? 'ok-y' : 'ok-n'">{{ preview.ok ? '可提交' : '不可提交' }}</span>
        </div>
        <div class="sum-msg">{{ preview.message }}</div>
        <div class="meta-grid">
          <span>项目：<b class="mono">{{ preview.projectName || '（未识别）' }}</b></span>
          <span>将分配版本：<b class="mono">{{ preview.nextSeq > 0 ? 'v' + preview.nextSeq : '不生成事件' }}</b></span>
          <span>破坏性：<b :class="{'txt-del': preview.breaking !== 0}">{{ preview.breaking === 1 ? '破坏' : preview.breaking === 2 ? '需人工确认' : '无' }}</b></span>
        </div>
        <div v-if="statsChips.length" class="stat-row">
          <span v-for="s in statsChips" :key="s.label" class="stat-chip" :class="s.cls">
            {{ s.label }} {{ s.value }}
          </span>
        </div>
        <div v-if="notes.length" class="note-block">
          <div v-for="(n, i) in notes" :key="i">· {{ n }}</div>
        </div>
      </div>

      <!-- meta 对照：文档声明 vs 本次覆盖（preview.meta 是原样回显，不与覆盖值合并，§13.24） -->
      <div class="imp-card">
        <div class="imp-title">③ meta 对照（文档声明 vs 本次覆盖）</div>
        <div class="hint">
          后端把 <code v-pre>```apidoc-meta</code> 原样回显，不与界面覆盖值合并 ——
          「最终生效」这一列才是事件里落下来的值。覆盖值取自当前输入框，
          改动输入后请重新「解析预览」让整份结果同步。
        </div>
        <div v-if="metaUndeclared" class="zone zone-warn meta-none">
          <div class="zone-title">文档未声明（散文文档）</div>
          <div class="zone-body">
            这份文档没有 <code v-pre>```apidoc-meta</code> 块：8 个字段全是空串，
            「为什么改 / 标题 / label」只能靠上面的覆盖输入框补 —— 而 <code v-pre>change_note</code>
            是本模块的核心价值，规范模式下缺它是 E-META-FIELD-MISSING 阻断。
          </div>
        </div>
        <table class="meta-table">
          <thead>
          <tr><th>字段</th><th>文档声明</th><th>本次覆盖</th><th>最终生效</th></tr>
          </thead>
          <tbody>
          <tr v-for="r in metaRows" :key="r.key">
            <td class="mono meta-key">{{ r.label }}</td>
            <td class="mono">{{ r.declared || '（未声明）' }}</td>
            <td class="mono">
              <span v-if="!r.hasEntry" class="txt-dim">（无覆盖入口）</span>
              <span v-else-if="r.override" class="txt-ovr">{{ r.override }}</span>
              <span v-else class="txt-dim">（不覆盖）</span>
            </td>
            <td class="mono">
              <b :class="{'txt-ovr': r.overridden}">{{ r.effective || '（空）' }}</b>
            </td>
          </tr>
          </tbody>
        </table>
        <div class="hint">
          本次覆盖生效 {{ overriddenCount }} 项；scope/service/base_path 由文档决定，
          project/kind/title/reason/label 可在上方输入区覆盖。
          唯一例外：change_title 若「文档声明」和「本次覆盖」都是空，后端会兜底用文档的
          <code v-pre># 一级标题</code>（再兜底「接口文档导入」），所以那一行显示（空）并不代表事件没标题。
        </div>
      </div>

      <!-- init 闸门拦截说明（§13.12：项目非空 + 未勾选放行） -->
      <div v-if="preview.initBlocked" class="zone zone-warn">
        <div class="zone-title">初始化模式被拦截</div>
        <div class="zone-body">
          该项目下已有接口条目，而这份文档要求（或被勾选）以 <code v-pre>scope: init</code> 导入。
          init 会按「现存文档全量重述」处理并跳过严格校验，因此默认禁止 —— 它只该用来灌存量散文文档。
        </div>
        <div class="zone-foot">
          <n-checkbox v-model:checked="allowInit" @update:checked="doPreview(true)">
            仍以初始化模式导入（allowInitOnNonEmpty，会放宽校验并逐条打推断警告）
          </n-checkbox>
        </div>
      </div>

      <!-- 错误区 E-*（阻断提交）与警告区 W-*（不阻断）：抽成 ApiIssueList 复用。
           Issue 是「预览产物」：后端只把它用在 E-*/W-* 校验输出，事件 DTO 里没有 issues 字段，
           所以事件详情页（ApiEventDetail）复用的是 ApiDiffList 而不是这个组件。 -->
      <ApiIssueList :issues="preview.issues" level="E"/>
      <ApiIssueList :issues="preview.issues" level="W"/>

      <!-- 文档自身问题与「变更」严格分栏（PRD Risk 5：文档 bug 不许污染事件列表） -->
      <div v-if="errors.length || warnings.length" class="docbug-note">
        以上属于「文档自身不合规范」，与下面的「接口变更」是两栏：文档 bug 不会写进变更事件明细。
      </div>

      <!-- 变更预览：按 changeType 分区 -->
      <div class="imp-card">
        <div class="imp-title">④ 变更预览</div>
        <div v-if="!changeGroups.length" class="hint">
          没有计算出任何变更（同一份文档重复导入即幂等短路，不生成事件、不改定义，AC2）。
        </div>
        <div v-for="g in changeGroups" :key="g.key" class="chg-group">
          <div class="chg-title" :class="'gt-' + g.key">{{ g.label }}（{{ g.rows.length }}）</div>
          <div v-for="c in g.rows" :key="rowKey(c)" class="chg-item">
            <div class="chg-row">
              <span class="mono chg-method">{{ c.method }}</span>
              <span class="mono chg-path">{{ c.path }}</span>
              <span class="chg-name">{{ c.name }}</span>
              <span v-if="c.domain" class="chg-domain">{{ c.domain }}</span>
              <span v-if="breakingText(c.breaking)" class="chg-brk">{{ breakingText(c.breaking) }}</span>
              <span v-if="c.docOnly" class="chg-doconly">仅措辞/示例同步</span>
              <n-button v-if="hasDiff(c)" size="tiny" quaternary @click="toggleRow(c)">
                {{ expandedKeys.includes(rowKey(c)) ? '收起 diff' : `展开 diff（${c.diff.length}）` }}
              </n-button>
            </div>
            <div class="chg-meta">
              <span>文档声明：<b>{{ c.declared || '（未声明）' }}</b></span>
              <span>程序判定：<b>{{ changeTypeText(c.changeType) }}</b></span>
              <span v-if="c.parseState && c.parseState !== 'ok'">解析：<b class="txt-warn">{{ c.parseState }}</b></span>
            </div>
            <div v-if="c.declaredNote" class="chg-note">变更点：{{ c.declaredNote }}</div>
            <div v-if="hasDiff(c) && expandedKeys.includes(rowKey(c))" class="chg-diff">
              <ApiDiffList :diff="c.diff"/>
            </div>
          </div>
        </div>
      </div>

      <!-- 领域候选：改名 / 合并（override 组装成 ApiDomainOverride） -->
      <div v-if="preview.domains?.length" class="imp-card">
        <div class="imp-title">⑤ 领域候选（可改名 / 合并）</div>
        <div class="hint">
          文档的一级 <code v-pre>##</code> 章节即领域（PRD D1）；改名或合并后需重新预览才会生效。
          注意改名/合并的落库形态：新名（或合并目标名）承接全部接口，<b>旧名的领域行会残留成空领域</b>
          —— 后端不会静默自动删，提交后结果卡会给一行「删除旧领域」的确认（spec-draft §13.25）。
        </div>
        <table class="dom-table">
          <thead>
          <tr><th>文档原标题</th><th>终名</th><th>合并到</th><th>接口数</th><th></th></tr>
          </thead>
          <tbody>
          <tr v-for="d in preview.domains" :key="d.rawTitle">
            <td class="mono">{{ d.rawTitle }}</td>
            <td>
              <n-input
                :value="domainEdits[d.rawTitle]?.name ?? d.name"
                size="small"
                placeholder="领域名"
                @update:value="(v) => { domainEdits[d.rawTitle] = {name: v, mergeInto: domainEdits[d.rawTitle]?.mergeInto || ''} }"
              />
            </td>
            <td>
              <n-select
                :value="domainEdits[d.rawTitle]?.mergeInto || null"
                size="small"
                clearable
                :options="preview.domains.filter(x => x.rawTitle !== d.rawTitle)
                            .map(x => ({label: domainEdits[x.rawTitle]?.name ?? x.name, value: domainEdits[x.rawTitle]?.name ?? x.name}))"
                placeholder="不合并"
                @update:value="(v) => { domainEdits[d.rawTitle] = {name: domainEdits[d.rawTitle]?.name || d.name, mergeInto: v || ''} }"
              />
            </td>
            <td class="mono">{{ d.entryCount }}</td>
            <td><span v-if="d.internal" class="ps-internal">内部</span></td>
          </tr>
          </tbody>
        </table>
        <div class="actions">
          <n-button size="small" :loading="previewing" @click="applyDomainEdits">应用领域调整并重新预览</n-button>
        </div>
      </div>

      <!-- ===================== ③ 提交区 ===================== -->
      <div class="imp-card">
        <div class="imp-title">⑥ 写入</div>

        <!-- D16 破坏性确认闸：显式勾选 + 列出将删/将破坏的接口 -->
        <template v-if="preview.needConfirm">
          <div class="confirm-zone">
            <div class="confirm-title">需要二次确认：本次含破坏性变更或删除</div>
            <ul v-if="riskRows.length" class="risk-list">
              <li v-for="r in riskRows" :key="rowKey(r)">
                <span class="mono">{{ r.method }} {{ r.path }}</span>
                <span>{{ r.name }}</span>
                <span class="risk-tag">{{ r.changeType === 'removed' ? '将被删除（软删）' : breakingText(r.breaking) }}</span>
              </li>
            </ul>
            <n-checkbox v-model:checked="confirmed">
              我已逐条核对以上影响，确认写入（删除为软删，可在「显示已删除」里找回）
            </n-checkbox>
          </div>
        </template>

        <div v-if="blockReason" class="block-reason">{{ blockReason }}</div>

        <div class="actions">
          <n-button @click="emit('cancel')">取消</n-button>
          <n-button type="primary" :disabled="!canSubmit" :loading="committing" @click="doCommit">
            {{ commitLabel }}
          </n-button>
        </div>
      </div>
    </template>

    <!-- ===================== 提交结果 ===================== -->
    <div v-if="result" class="imp-card res-card">
      <div class="imp-title">{{ resultIsNoop ? '导入结果（无变更）' : '导入完成' }}</div>
      <div class="sum-msg">{{ result.message }}</div>
      <div class="meta-grid">
        <span>事件 ID：<b class="mono">{{ result.eventId || '未生成' }}</b></span>
        <span>版本：<b class="mono">{{ result.seq > 0 ? 'v' + result.seq : '-' }}</b></span>
        <span>项目：<b class="mono">{{ result.projectId || '-' }}</b></span>
        <span>模式：<b class="mono">{{ result.mode || '-' }}</b></span>
        <span>破坏性：<b :class="{'txt-del': result.breaking !== 0}">
          {{ result.breaking === 1 ? '破坏' : result.breaking === 2 ? '需人工确认' : '无' }}</b></span>
      </div>
      <div v-if="resultStatsChips.length" class="stat-row">
        <span v-for="s in resultStatsChips" :key="s.label" class="stat-chip" :class="s.cls">
          {{ s.label }} {{ s.value }}
        </span>
      </div>
      <!-- notes：幂等短路时「陈旧声明」降级提示从这里出（后端与预览同源，不新增错误码） -->
      <div v-if="resultNotes.length" class="note-block">
        <div class="note-title">后端提示</div>
        <div v-for="(n, i) in resultNotes" :key="i">· {{ n }}</div>
      </div>

      <!-- 本次新建了什么：结果卡要说清，别让用户返回列表后靠猜 -->
      <div v-if="result.createdProject || result.createdDomains?.length" class="meta-grid">
        <span v-if="result.createdProject">本次新建项目：<b class="mono">{{ result.projectId }}</b></span>
        <span v-if="result.createdDomains?.length">本次新建领域（override 后的落库终名）：
          <b v-for="n in result.createdDomains" :key="n" class="dom-tag">{{ n }}</b>
        </span>
      </div>

      <!-- 旧名空领域清理（§13.25：后端不静默自动删，这里给一行提示 + 一个按钮） -->
      <div v-if="staleDomains.length" class="zone zone-warn">
        <div class="zone-title">旧领域已空，可删除</div>
        <div class="zone-body">
          本次用了领域改名/合并 override，落库的是 override 后的终名；下面这些旧名领域行已经没有任何
          接口，后端不会静默自动删除，需要你点一下确认。
        </div>
        <div v-for="d in staleDomains" :key="d.id" class="stale-row">
          <span class="mono stale-name">{{ d.name }}</span>
          <n-button size="tiny" type="error" ghost :loading="staleDeleting === d.id"
                    :disabled="!!staleDeleting" @click="removeStaleDomain(d)">
            删除旧领域
          </n-button>
        </div>
      </div>
      <div v-else-if="staleDetecting" class="hint">正在检查改名前的旧领域是否残留…</div>

      <div class="actions">
        <n-button @click="backToList">返回列表并刷新</n-button>
        <n-button type="primary" ghost @click="viewEvent">查看该事件</n-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 纵向单列卡片流；滚动由父侧 .import-body 承担（见 ApiDocManager 样式注释），
   这里不声明 height:100%/overflow，避免百分比高度在 flex item 上失效 */
.imp { padding: 14px 16px; display: flex; flex-direction: column; gap: 12px; }
.imp-card {
  border: 1px solid rgba(255,255,255,0.09); border-radius: 8px; padding: 12px 14px;
  display: flex; flex-direction: column; gap: 8px; flex-shrink: 0;
}
.imp-title { font-size: 13px; font-weight: 600; color: rgba(255,255,255,0.9); }
.no-mb { margin: 0; }
.imp-sub, .hint { font-size: 12px; color: rgba(255,255,255,0.5); line-height: 1.7; }
.imp-sub code, .hint code, .zone-body code { font-family: Consolas, Menlo, monospace;
  color: rgba(99,226,183,1); }
.mono { font-family: Consolas, Menlo, monospace; }

/* ---- 输入区 ---- */
.file-row { display: flex; align-items: center; gap: 10px; }
.file-input { display: none; } /* 原生文件框无法自定义外观：藏起来由按钮代触发 */
.doc-textarea :deep(textarea) { font-family: Consolas, Menlo, monospace; font-size: 12px; }
.form-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px; }
.fg-item { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.fg-item.span2 { grid-column: span 2; }
.fg-label { font-size: 12px; color: rgba(255,255,255,0.5); }
.two-in-row { display: flex; gap: 6px; }
.check-row { display: flex; flex-direction: column; gap: 4px; }
.actions { display: flex; gap: 10px; justify-content: flex-end; }

/* ---- 汇总 ---- */
.sum-row { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.sum-mode { font-size: 12px; color: rgba(255,255,255,0.5); }
.sum-ok { font-size: 12px; padding: 1px 6px; border-radius: 4px; }
.ok-y { background: rgba(99,226,183,0.15); color: rgba(99,226,183,1); }
.ok-n { background: rgba(224,108,117,0.15); color: rgba(224,108,117,1); }
.sum-msg { font-size: 13px; color: rgba(255,255,255,0.8); }
.meta-grid { display: flex; flex-wrap: wrap; gap: 6px 18px; font-size: 12px; color: rgba(255,255,255,0.6); }
.txt-del { color: rgba(224,108,117,1); }
.txt-warn { color: rgba(240,168,48,1); }
.stat-row { display: flex; gap: 6px; flex-wrap: wrap; }
.stat-chip { font-size: 11px; padding: 2px 8px; border-radius: 4px; background: rgba(255,255,255,0.07);
  color: rgba(255,255,255,0.7); }
.st-add { background: rgba(99,226,183,0.15); color: rgba(99,226,183,1); }
.st-upd { background: rgba(99,179,237,0.15); color: rgba(99,179,237,1); }
.st-del { background: rgba(224,108,117,0.15); color: rgba(224,108,117,1); }
.st-res { background: rgba(240,168,48,0.15); color: rgba(240,168,48,1); }
.st-un { color: rgba(255,255,255,0.45); }
.note-block { font-size: 12px; color: rgba(255,255,255,0.6); line-height: 1.7;
  padding: 6px 8px; border-radius: 6px; background: rgba(255,255,255,0.04); }
.note-title { font-size: 12px; font-weight: 600; color: rgba(255,255,255,0.75); margin-bottom: 4px; }

/* ---- meta 对照表（文档声明 vs 本次覆盖 vs 最终生效，§13.24） ---- */
.meta-table { width: 100%; border-collapse: collapse; font-size: 12px; table-layout: fixed; }
.meta-table th, .meta-table td { border: 1px solid rgba(255,255,255,0.09); padding: 5px 8px;
  text-align: left; vertical-align: top; word-break: break-word; }
.meta-table th { color: rgba(255,255,255,0.55); font-weight: 600; background: rgba(255,255,255,0.03); }
.meta-table td { color: rgba(255,255,255,0.75); }
.meta-key { color: rgba(255,255,255,0.55); }
.txt-dim { color: rgba(255,255,255,0.35); }
.txt-ovr { color: rgba(99,226,183,1); }
.meta-none { margin-bottom: 2px; }

/* ---- 本次新建的领域名标签 ---- */
.dom-tag { display: inline-block; margin: 0 4px; padding: 1px 6px; border-radius: 4px;
  background: rgba(99,226,183,0.15); color: rgba(99,226,183,1); font-family: Consolas, Menlo, monospace; }

/* ---- 旧名空领域清理行 ---- */
.stale-row { display: flex; align-items: center; gap: 10px; margin-top: 6px; }
.stale-name { color: rgba(255,255,255,0.8); word-break: break-all; }

/* ---- init 闸门分区（琥珀）；E 码 / W 码问题分区样式在 ApiIssueList.vue 内 ---- */
.zone { border-radius: 6px; padding: 10px 12px; font-size: 12px; flex-shrink: 0; }
.zone-warn { background: rgba(240,168,48,0.08); border: 1px solid rgba(240,168,48,0.3);
  color: rgba(255,255,255,0.75); }
.zone-title { font-weight: 600; margin-bottom: 6px; color: rgba(240,168,48,1); }
.zone-body { line-height: 1.7; }
.zone-foot { margin-top: 8px; }
.docbug-note { font-size: 12px; color: rgba(255,255,255,0.5); padding: 0 2px; }

/* ---- 变更分区 ---- */
.chg-group { margin-top: 10px; }
.chg-title { font-size: 12px; font-weight: 600; margin-bottom: 6px; }
.gt-added { color: rgba(99,226,183,1); }
.gt-updated { color: rgba(99,179,237,1); }
.gt-removed { color: rgba(224,108,117,1); }
.gt-restored { color: rgba(240,168,48,1); }
.gt-unchanged, .gt-other { color: rgba(255,255,255,0.5); }
.chg-item { border: 1px solid rgba(255,255,255,0.09); border-radius: 6px; padding: 8px 10px;
  margin-bottom: 6px; }
.chg-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; min-width: 0; font-size: 12px; }
.chg-method { font-weight: 700; color: rgba(255,255,255,0.85); }
.chg-path { color: rgba(255,255,255,0.85); word-break: break-all; }
.chg-name { color: rgba(255,255,255,0.6); }
.chg-domain { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: rgba(255,255,255,0.07);
  color: rgba(255,255,255,0.6); }
.chg-brk { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: rgba(224,108,117,0.18);
  color: rgba(224,108,117,1); }
.chg-doconly { font-size: 11px; color: rgba(255,255,255,0.4); }
.chg-meta { display: flex; gap: 14px; margin-top: 4px; font-size: 11px; color: rgba(255,255,255,0.5); }
.chg-note { margin-top: 4px; font-size: 12px; color: rgba(255,255,255,0.65); line-height: 1.6; }
.chg-diff { margin-top: 8px; }

/* ---- 领域候选表 ---- */
.dom-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.dom-table th, .dom-table td { border: 1px solid rgba(255,255,255,0.09); padding: 5px 8px;
  text-align: left; vertical-align: top; }
.dom-table th { color: rgba(255,255,255,0.55); font-weight: 600; background: rgba(255,255,255,0.03); }
.ps-internal { font-size: 11px; padding: 1px 5px; border-radius: 4px; background: rgba(99,179,237,0.15);
  color: rgba(99,179,237,1); }

/* ---- 确认闸 ---- */
.confirm-zone {
  border-radius: 6px; padding: 10px 12px; display: flex; flex-direction: column; gap: 8px;
  background: rgba(224,108,117,0.08); border: 1px solid rgba(224,108,117,0.35);
}
.confirm-title { font-size: 12px; font-weight: 600; color: rgba(224,108,117,1); }
.risk-list { margin: 0; padding-left: 18px; font-size: 12px; color: rgba(255,255,255,0.72);
  display: flex; flex-direction: column; gap: 3px; }
.risk-tag { color: rgba(224,108,117,1); margin-left: 6px; }
.block-reason { font-size: 12px; color: rgba(240,168,48,1); }

/* ---- 结果卡 ---- */
.res-card { border-color: rgba(99,226,183,0.35); }
</style>
