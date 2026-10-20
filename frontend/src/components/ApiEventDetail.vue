<!--
  ApiEventDetail.vue —— 单条变更事件的详情（「变更事件」页的右栏）。

  布局结构（自上而下单列滚动）：
    ① 事件头：v{seq} + label + title + kind 徽标 + breaking 三态徽标
    ② 「为什么改」大字块（本模块的核心价值，PRD 用户原话「要让前端知道我更新接口是为了什么」，
       所以它排在所有明细之前并且视觉权重最高）
    ③ 统计徽标（stats：新增/修改/删除/恢复；moved 恒 0 不展示，spec-draft §13.16）
    ④ 元信息表（source / scopeMode / docRef / 文档指纹 / 时间）
    ⑤ 涉及接口明细：按 changeType 分区。每行 = domainName+entryName 冗余快照 +
       method+path + changeType + breaking 徽标 + declaredNote（文档「变更点」原文）+
       展开后的字段级 diff（复用 ApiDiffList，不重写第三份渲染）
       · visibility='internal' 的明细默认折叠（PRD R11 / spec-draft §3.2）
       · added/restored 的 diff 是空数组（§13.20）⇒ 不渲染展开按钮
    ⑥ 「查看当时导入的原文」：点击时才调 GetApiChangeEventDocText（列表与详情 SQL 都不带
       doc_text 大文本列），结果缓存在本组件；用 <pre> 只读展示 —— 零 v-html、不走 marked

  数据来源：props.eventId → 自取 GetApiChangeEvent(eventId)（appsvc.ApiChangeEventDetail：
  event + items，含字段级 diff 载荷）；原文自取 GetApiChangeEventDocText(eventId)。
  eventId 变化时重拉（照 ApiEntryDetail 的 props.id watch 模式）。

  props / emit 契约：
    props: {eventId: Number}        —— 0/null 时显示空态
    emit('openEntry', {entryId, domainName, entryName})
                                     —— 点明细行的「查看接口」；跳转由父侧实现（本组件不跨组件抓数据）

  渲染红线（spec-draft §13）：
    · diff 载荷的 before/after 是对象不是字符串，required 是 -1/0/1 哨兵 —— 全部交给
      ApiDiffList 处理，本组件不自行拼接
    · 展示类型用 typeRaw（ApiDiffList 内部已遵守）
    · Go 端中文错误原样 message.error(String(err))
-->
<script setup>
import {computed, ref, watch} from 'vue'
import {useMessage} from 'naive-ui'
import {GetApiChangeEvent, GetApiChangeEventDocText} from '../../bindings/DevCraft/app.js'
import ApiDiffList from './ApiDiffList.vue'

const props = defineProps({
  eventId: {type: Number, default: 0}
})
const emit = defineEmits(['openEntry'])
const message = useMessage()

// ---------------- 详情数据 ----------------
const event = ref(null)        // store.ApiChangeEvent（事件头；docText 永远为空串，列表/详情都不取该列）
const items = ref([])          // store.ApiChangeItem[]
const loading = ref(false)
const failed = ref(false)      // 拉取失败：给一句人话而不是整栏空白

const showInternal = ref(false) // internal 明细默认折叠（R11）
const expanded = ref([])        // 展开 diff 的 item id
const showDoc = ref(false)      // 原文展开开关
const docText = ref(null)       // 已拉取的原文缓存；null = 还没拉过
const loadingDoc = ref(false)

async function loadDetail(id) {
  if (!id) {
    event.value = null
    items.value = []
    failed.value = false
    return
  }
  loading.value = true
  failed.value = false
  // 必须先清空：否则切换事件的瞬间，右栏渲染的还是上一条事件的 reason 与明细
  event.value = null
  items.value = []
  showInternal.value = false
  expanded.value = []
  showDoc.value = false
  docText.value = null
  try {
    const d = await GetApiChangeEvent(id)
    event.value = d?.event || null
    items.value = d?.items || []
  } catch (err) {
    failed.value = true
    message.error(String(err)) // 「变更事件不存在（id=N）」等中文错误原样展示
  } finally {
    loading.value = false
  }
}

watch(() => props.eventId, (id) => {
  loadDetail(id)
}, {immediate: true})

// ---------------- 展示辅助 ----------------
const CHANGE_TYPE_LABEL = {
  added: '新增', updated: '修改', removed: '删除', restored: '恢复', moved: '迁移', unchanged: '未变更'
}
function changeTypeText(t) {
  return CHANGE_TYPE_LABEL[t] || t || '-'
}

/** breaking 三态：0 无 / 1 自动判破坏 / 2 需人工确认（等同破坏处理，D16 确认闸的判据） */
function breakingOf(b) {
  if (b === 1) return {text: '破坏性', cls: 'brk-1'}
  if (b === 2) return {text: '需人工确认', cls: 'brk-2'}
  return {text: '非破坏', cls: 'brk-0'}
}

const KIND_LABEL = {feature: 'feature 新功能', fix: 'fix 修复', refactor: 'refactor 重构', docs: 'docs 文档'}
function kindText(k) {
  return KIND_LABEL[k] || k || '（未记录）'
}

const SCOPE_LABEL = {incremental: '增量导入', init: '初始化导入', full: '全量（二期未实现）'}
function scopeText(s) {
  return SCOPE_LABEL[s] || s || '-'
}

const SOURCE_LABEL = {import: '文档导入', manual: '手工记录'}
function sourceText(s) {
  return SOURCE_LABEL[s] || s || '-'
}

/** createdAt 是毫秒时间戳（store 侧 time.Now().UnixMilli()）；D7 的显示形态 v7 · REQ-1024 · 2026-10-18 */
function fmtDate(ms) {
  const n = Number(ms || 0)
  if (!n) return '-'
  const d = new Date(n)
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

const breakingHead = computed(() => breakingOf(event.value?.breaking || 0))

/** stats 徽标：moved 恒 0（§13.16 MVP 增量导入不产生迁移），刻意不进 UI */
const statChips = computed(() => {
  const s = event.value?.stats
  if (!s) return []
  const out = []
  if (s.added) out.push({label: '新增', value: s.added, cls: 'st-add'})
  if (s.updated) out.push({label: '修改', value: s.updated, cls: 'st-upd'})
  if (s.removed) out.push({label: '删除', value: s.removed, cls: 'st-del'})
  if (s.restored) out.push({label: '恢复', value: s.restored, cls: 'st-res'})
  if (!out.length) out.push({label: '无字段级明细', value: '', cls: 'st-none'})
  return out
})

/** 文档指纹（event.meta 的 JSON 扩展槽，appsvc 写入 docSha256）：溯源用，读不到就不显示行 */
const docSha = computed(() => {
  const m = event.value?.meta
  const v = m && m.docSha256
  return v ? String(v) : ''
})

// ---------------- 明细分区 ----------------
/** 公开明细按 changeType 分区（新增/修改/删除/恢复/其他）；internal 明细单独一个分区，默认折叠 */
const GROUP_ORDER = ['added', 'updated', 'removed', 'restored']

function partition(list) {
  const out = []
  for (const t of GROUP_ORDER) {
    const rows = list.filter(i => (i.changeType || '') === t)
    if (rows.length) out.push({key: t, label: changeTypeText(t), rows, internal: false})
  }
  const other = list.filter(i => !GROUP_ORDER.includes(i.changeType || ''))
  if (other.length) out.push({key: 'other', label: '其他', rows: other, internal: false})
  return out
}

const internalItems = computed(() => (items.value || []).filter(i => i.visibility === 'internal'))
const publicCount = computed(() => (items.value || []).length - internalItems.value.length)

/** 一个数组渲染到底：公开分区 + 末尾一个「内部端点明细」折叠分区（行模板只有一份） */
const sections = computed(() => {
  const out = partition(items.value.filter(i => i.visibility !== 'internal'))
  if (internalItems.value.length) {
    out.push({key: 'internal', label: '内部端点明细', rows: internalItems.value, internal: true})
  }
  return out
})

/** added/restored 的 diff 是空数组（§13.20）→ 不给展开按钮，免得点出一个空表 */
function hasDiff(it) {
  return (it.diff || []).length > 0
}
function toggleItem(it) {
  const i = expanded.value.indexOf(it.id)
  if (i >= 0) expanded.value.splice(i, 1)
  else expanded.value.push(it.id)
}

function openEntry(it) {
  if (!it.entryId) {
    message.warning('该明细没有记录接口条目 id（早期导入的历史行），无法跳转')
    return
  }
  emit('openEntry', {entryId: it.entryId, domainName: it.domainName, entryName: it.entryName})
}

// ---------------- 原文（D12 输入凭证） ----------------
/** 只在展开时拉：这是大文本列，后端刻意把它从列表与详情 SQL 里摘出去了 */
async function toggleDocText() {
  showDoc.value = !showDoc.value
  if (!showDoc.value || docText.value !== null || !props.eventId) return
  loadingDoc.value = true
  try {
    docText.value = (await GetApiChangeEventDocText(props.eventId)) || ''
  } catch (err) {
    docText.value = ''
    message.error(String(err))
  } finally {
    loadingDoc.value = false
  }
}
</script>

<template>
  <div class="evd">
    <n-empty v-if="!eventId" description="选择左侧事件查看变更详情" size="small" class="evd-empty"/>

    <div v-else-if="loading" class="evd-loading">正在加载事件详情…</div>

    <div v-else-if="failed && !event" class="evd-loading evd-fail">
      事件详情加载失败，请在左侧重新选择，或查看轻提示里的中文原因。
    </div>

    <template v-else-if="event">
      <!-- ===== ① 事件头 ===== -->
      <div class="evd-head">
        <div class="evd-title-row">
          <span class="evd-seq mono">v{{ event.seq }}</span>
          <span v-if="event.label" class="evd-label mono">{{ event.label }}</span>
          <span class="evd-kind mono" :class="'kind-' + (event.kind || 'none')">{{ kindText(event.kind) }}</span>
          <span class="evd-brk" :class="breakingHead.cls">{{ breakingHead.text }}</span>
        </div>
        <div class="evd-title">{{ event.title || '（无标题）' }}</div>
        <div class="evd-sub">
          <span class="mono">{{ fmtDate(event.createdAt) }}</span>
          <span>{{ scopeText(event.scopeMode) }}</span>
          <span>{{ sourceText(event.source) }}</span>
        </div>
      </div>

      <!-- ===== ② 为什么改（核心价值，视觉权重最高） ===== -->
      <div class="evd-reason">
        <div class="evd-reason-label">为什么改</div>
        <div class="evd-reason-text">
          {{ event.reason || '（这条事件没有记录原因：文档未声明 change_note 且导入时也没补填）' }}
        </div>
      </div>

      <!-- ===== ③ 统计 ===== -->
      <div class="evd-stats">
        <span v-for="s in statChips" :key="s.label" class="stat-chip" :class="s.cls">
          {{ s.label }}<b v-if="s.value !== ''" class="mono"> {{ s.value }}</b>
        </span>
      </div>

      <!-- ===== ④ 元信息 ===== -->
      <div class="evd-section">
        <div class="evd-section-title">事件元信息</div>
        <table class="kv-table">
          <tbody>
          <tr><th>版本</th><td class="mono">v{{ event.seq }}（项目内自增，D7）</td></tr>
          <tr><th>需求标签</th><td class="mono">{{ event.label || '（未打标）' }}</td></tr>
          <tr><th>事件 ID</th><td class="mono">{{ event.id }}</td></tr>
          <tr><th>项目 ID</th><td class="mono">{{ event.projectId || '-' }}</td></tr>
          <tr><th>来源文档</th><td class="mono">{{ event.docRef || '（文档未记录来源路径）' }}</td></tr>
          <tr v-if="docSha"><th>文档指纹</th><td class="mono">{{ docSha }}</td></tr>
          <tr><th>发生时间</th><td class="mono">{{ fmtDate(event.createdAt) }}</td></tr>
          </tbody>
        </table>
      </div>

      <!-- ===== ⑤ 涉及接口明细 ===== -->
      <div class="evd-section">
        <div class="evd-section-title">涉及接口（共 {{ items.length }} 条，公开 {{ publicCount }} 条）</div>

        <div v-if="!items.length" class="evd-none">
          这条事件没有明细（后端只在 diff 载荷非空时才生成事件，spec-draft §13.1）。
        </div>

        <!-- 公开分区 + 末尾的「内部端点明细」折叠分区共用这一套行模板 -->
        <div v-for="g in sections" :key="g.key" class="ct-group" :class="{'internal-zone': g.internal}">
          <div class="ct-group-title" :class="g.internal ? 'gt-internal' : 'gt-' + g.key">
            <span>
              {{ g.internal
                ? `内部端点明细（${g.rows.length}）—— 默认折叠（R11）`
                : `${g.label}（${g.rows.length}）` }}
            </span>
            <n-button v-if="g.internal" size="tiny" quaternary @click="showInternal = !showInternal">
              {{ showInternal ? '收起' : '展开查看' }}
            </n-button>
          </div>
          <template v-if="!g.internal || showInternal">
            <div v-for="it in g.rows" :key="it.id" class="ct-item" :class="{'ct-item-internal': g.internal}">
              <div class="ct-row">
                <span class="ct-badge" :class="'ct-' + it.changeType">{{ changeTypeText(it.changeType) }}</span>
                <span class="mono ct-method">{{ it.method }}</span>
                <span class="mono ct-path">{{ it.path }}</span>
                <span v-if="it.visibility === 'internal'" class="ps-internal">内部</span>
                <span v-if="breakingOf(it.breaking).text !== '非破坏'" class="ct-brk">
                  {{ breakingOf(it.breaking).text }}
                </span>
              </div>
              <div class="ct-belong">
                <!-- domainName/entryName 是事件明细里的冗余快照：领域后来被改名/合并也不影响历史可读性 -->
                <span class="ct-domain">{{ it.domainName || '未归领域' }}</span>
                <span class="ct-entry">{{ it.entryName || '（无中文名）' }}</span>
                <n-button size="tiny" quaternary @click="openEntry(it)">查看接口</n-button>
                <n-button v-if="hasDiff(it)" size="tiny" quaternary @click="toggleItem(it)">
                  {{ expanded.includes(it.id) ? '收起 diff' : `展开 diff（${it.diff.length}）` }}
                </n-button>
              </div>
              <div v-if="it.declaredNote" class="ct-note">变更点（文档原文）：{{ it.declaredNote }}</div>
              <div v-else class="ct-note ct-note-missing">变更点：文档未声明（新增接口允许不写）</div>
              <div v-if="hasDiff(it) && expanded.includes(it.id)" class="ct-diff">
                <ApiDiffList :diff="it.diff"/>
              </div>
            </div>
          </template>
        </div>
      </div>

      <!-- ===== ⑥ 当时导入的原文（D12） ===== -->
      <div class="evd-section">
        <div class="evd-section-title">输入凭证</div>
        <div class="doc-line">
          <n-button size="small" :loading="loadingDoc" @click="toggleDocText">
            {{ showDoc ? '收起导入原文' : '查看当时导入的原文' }}
          </n-button>
          <span class="doc-hint">
            事件保存的是输入文档全文（不是接口历史快照）；纯文本展示，不做 Markdown 渲染。
          </span>
        </div>
        <!-- 零 v-html：导入的是外部文档，marked 不 sanitize，PRD 已裁决用 <pre> 只读展示 -->
        <pre v-if="showDoc" class="evd-doc">{{ loadingDoc || docText === null ? '正在读取原文…' : (docText || '（这条事件没有留档原文）') }}</pre>
      </div>
    </template>

    <div v-else class="evd-loading">没有可显示的事件数据。</div>
  </div>
</template>

<style scoped>
/* 右栏：整栏纵向滚动（观感与 ApiEntryDetail 对齐） */
.evd { height: 100%; overflow-y: auto; padding: 14px 16px; min-width: 0; font-size: 13px; }
.evd-empty { margin-top: 60px; }
.evd-loading { font-size: 12px; color: rgba(255,255,255,0.45); margin-bottom: 8px; }
.evd-fail { color: rgba(224,108,117,0.9); }
.mono { font-family: Consolas, Menlo, monospace; }

/* ---- 事件头 ---- */
.evd-head { border-bottom: 1px solid rgba(255,255,255,0.08); padding-bottom: 12px; }
.evd-title-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.evd-seq { font-size: 16px; font-weight: 700; color: rgba(99,226,183,1); }
.evd-label { font-size: 12px; padding: 1px 6px; border-radius: 4px; background: rgba(99,179,237,0.15);
  color: rgba(99,179,237,1); }
.evd-kind { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: rgba(255,255,255,0.07);
  color: rgba(255,255,255,0.65); }
.kind-feature { background: rgba(99,226,183,0.15); color: rgba(99,226,183,1); }
.kind-fix { background: rgba(224,108,117,0.15); color: rgba(224,108,117,1); }
.kind-refactor { background: rgba(240,168,48,0.15); color: rgba(240,168,48,1); }
.kind-docs { background: rgba(99,179,237,0.15); color: rgba(99,179,237,1); }
.evd-brk { font-size: 11px; padding: 1px 6px; border-radius: 4px; }
.brk-0 { background: rgba(255,255,255,0.07); color: rgba(255,255,255,0.5); }
.brk-1 { background: rgba(224,108,117,0.18); color: rgba(224,108,117,1); }
.brk-2 { background: rgba(240,168,48,0.18); color: rgba(240,168,48,1); }
.evd-title { font-size: 15px; font-weight: 600; margin-top: 6px; color: rgba(255,255,255,0.88); }
.evd-sub { display: flex; gap: 12px; margin-top: 6px; font-size: 12px; color: rgba(255,255,255,0.5); }

/* ---- 为什么改 ---- */
.evd-reason {
  margin-top: 12px; padding: 12px 14px; border-radius: 8px;
  background: rgba(99,226,183,0.08); border: 1px solid rgba(99,226,183,0.35);
}
.evd-reason-label { font-size: 12px; font-weight: 600; color: rgba(99,226,183,1); margin-bottom: 6px; }
.evd-reason-text { font-size: 14px; line-height: 1.7; color: rgba(255,255,255,0.9); white-space: pre-line; }

/* ---- 统计 ---- */
.evd-stats { display: flex; gap: 6px; flex-wrap: wrap; margin-top: 12px; }
.stat-chip { font-size: 11px; padding: 2px 8px; border-radius: 4px; background: rgba(255,255,255,0.07);
  color: rgba(255,255,255,0.7); }
.st-add { background: rgba(99,226,183,0.15); color: rgba(99,226,183,1); }
.st-upd { background: rgba(99,179,237,0.15); color: rgba(99,179,237,1); }
.st-del { background: rgba(224,108,117,0.15); color: rgba(224,108,117,1); }
.st-res { background: rgba(240,168,48,0.15); color: rgba(240,168,48,1); }
.st-none { color: rgba(255,255,255,0.45); }

/* ---- 分区 ---- */
.evd-section { margin-top: 18px; }
.evd-section-title { font-weight: 600; font-size: 13px; margin-bottom: 8px; color: rgba(255,255,255,0.9); }
.evd-none { font-size: 12px; color: rgba(255,255,255,0.45); }

.kv-table { width: 100%; border-collapse: collapse; font-size: 12px; table-layout: fixed; }
.kv-table th, .kv-table td { border: 1px solid rgba(255,255,255,0.09); padding: 6px 10px;
  text-align: left; vertical-align: top; word-break: break-word; }
.kv-table th { width: 96px; color: rgba(255,255,255,0.55); font-weight: 600; background: rgba(255,255,255,0.03); }
.kv-table td { color: rgba(255,255,255,0.75); }

/* ---- 明细 ---- */
.ct-group { margin-top: 12px; }
.ct-group-title { display: flex; align-items: center; gap: 10px; font-size: 12px; font-weight: 600;
  margin-bottom: 6px; }
.gt-added { color: rgba(99,226,183,1); }
.gt-updated { color: rgba(99,179,237,1); }
.gt-removed { color: rgba(224,108,117,1); }
.gt-restored { color: rgba(240,168,48,1); }
.gt-other { color: rgba(255,255,255,0.5); }
.gt-internal { color: rgba(99,179,237,1); }
.ct-item { border: 1px solid rgba(255,255,255,0.09); border-radius: 6px; padding: 8px 10px; margin-bottom: 6px; }
.ct-item-internal { border-color: rgba(99,179,237,0.25); }
.ct-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; min-width: 0; font-size: 12px; }
.ct-badge { flex-shrink: 0; font-size: 11px; padding: 1px 6px; border-radius: 4px;
  background: rgba(255,255,255,0.07); color: rgba(255,255,255,0.7); }
.ct-added { color: rgba(99,226,183,1); }
.ct-updated { color: rgba(99,179,237,1); }
.ct-removed { color: rgba(224,108,117,1); }
.ct-restored { color: rgba(240,168,48,1); }
.ct-method { font-weight: 700; color: rgba(255,255,255,0.85); }
.ct-path { color: rgba(255,255,255,0.85); word-break: break-all; }
.ct-brk { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: rgba(224,108,117,0.18);
  color: rgba(224,108,117,1); }
.ct-belong { display: flex; align-items: center; gap: 8px; margin-top: 4px; flex-wrap: wrap; }
.ct-domain { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: rgba(255,255,255,0.07);
  color: rgba(255,255,255,0.6); }
.ct-entry { font-size: 12px; color: rgba(255,255,255,0.6); }
.ct-note { margin-top: 4px; font-size: 12px; color: rgba(255,255,255,0.7); line-height: 1.6; }
.ct-note-missing { color: rgba(255,255,255,0.4); }
.ct-diff { margin-top: 8px; }
.ps-internal { font-size: 11px; padding: 1px 5px; border-radius: 4px; background: rgba(99,179,237,0.15);
  color: rgba(99,179,237,1); }

/* internal 分区：挂在 .ct-group 上的修饰类（折叠态只留标题行 + 展开按钮） */
.internal-zone {
  margin-top: 12px; padding: 8px 10px; border-radius: 6px;
  background: rgba(99,179,237,0.06); border: 1px solid rgba(99,179,237,0.25);
}

/* ---- 原文 ---- */
.doc-line { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.doc-hint { font-size: 12px; color: rgba(255,255,255,0.45); }
.evd-doc {
  margin: 8px 0 0; padding: 10px; border-radius: 6px; max-height: 420px; overflow: auto;
  background: rgba(0,0,0,0.35); border: 1px solid rgba(255,255,255,0.09);
  font-family: Consolas, Menlo, monospace; font-size: 12px; color: rgba(255,255,255,0.75);
  white-space: pre-wrap; word-break: break-all; line-height: 1.6;
}
</style>
