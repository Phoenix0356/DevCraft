<!--
  ApiChangeEvents.vue —— 「变更事件」页（PRD R9/AC7：前端同学进站看这里就知道这次为什么改）。

  布局结构（tab 内两栏，整块内联，不用弹窗；沿用 ApiDocManager 的普通 flex 三栏风格）：
    顶部条：项目下拉（ListApiProjects 由父侧传入）+ 视图切换（按时间平铺 / 按需求标签分组，D10）
            + label 筛选（ListApiChangeLabels）+ 刷新 + 计数
    左栏（固定 380px）：事件列表行 —— 每行 v{seq} + label + kind 徽标 + 日期 + title
            + reason 摘要（单行省略）+ stats 计数徽标（新增/修改/删除/恢复；moved 恒 0 不展示，
            spec-draft §13.16）+ breaking 三态徽标（0 非破坏 / 1 破坏 / 2 需人工确认）
            · 列表数据里没有 docText（后端 ListApiChangeEvents 的 SQL 刻意不取该大文本列），
              所以这里绝不试图展示原文
    右栏（flex:1）：ApiEventDetail（自取 GetApiChangeEvent + GetApiChangeEventDocText）

  两种视图形态：
    · 按时间：后端返回什么顺序就什么顺序（seq DESC，AC7）
    · 按 label 分组：在前端按 label 聚合，**空 label 归到「未打标」**分组，组内仍按 seq DESC
      （D10「同一需求分几次导入靠 label 聚合视图串起来」；后端没有分组接口，只能前端聚）

  数据来源：ListApiChangeEvents(q{projectId,label,limit}) / ListApiChangeLabels(projectId)；
  props.projects 只为项目下拉提供候选（父侧已拉过，避免重复 RPC）。

  props / emit 契约：
    props: {
      projects: ApiProject[],   —— 项目下拉候选
      projectId: String,        —— 当前项目（父侧选中项）
      initialEventId: Number    —— 父侧「查看该事件」跳转要直接打开的事件（0 = 不指定）
    }
    emit('selectProject', id)      —— 换项目：父侧统一维护 currentProjectId 并同步领域/接口列表
    emit('openEntry', {entryId, domainName, entryName}) —— 详情里的「查看接口」，父侧切回接口 tab

  约定：projectId 变化要重拉（这是数据依赖，与 ApiEntryDetail watch entry.id 同类；
  不是新增 watch(view) 副作用）。跳转打开的事件即使被 label 筛选挡掉也照样能拉详情
  （GetApiChangeEvent 只认 id）。
-->
<script setup>
import {computed, onMounted, ref, watch} from 'vue'
import {useMessage} from 'naive-ui'
import {ListApiChangeEvents, ListApiChangeLabels} from '../../bindings/DevCraft/app.js'
import ApiEventDetail from './ApiEventDetail.vue'

const props = defineProps({
  projects: {type: Array, default: () => []},
  projectId: {type: String, default: ''},
  initialEventId: {type: Number, default: 0}
})
const emit = defineEmits(['selectProject', 'openEntry'])
const message = useMessage()

// ---------------- 状态 ----------------
const events = ref([])
const labels = ref([])
const mode = ref('time')          // 'time' 平铺 | 'label' 按需求标签分组
const labelFilter = ref('')       // '' = 全部（非空时交后端过滤）
const listLoading = ref(false)
const selectedEventId = ref(0)

// ---------------- 加载 ----------------
/** 父侧跳转带进来的事件 id：只在首次加载时用一次，用完即清（否则每次刷新都跳回它） */
const pendingJumpId = ref(props.initialEventId > 0 ? Number(props.initialEventId) : 0)

/** 事件列表：seq DESC 由后端保证（store.ListApiChangeEvents ORDER BY seq DESC） */
async function loadEvents() {
  if (!props.projectId) {
    events.value = []
    labels.value = []
    selectedEventId.value = 0
    return
  }
  listLoading.value = true
  try {
    events.value = (await ListApiChangeEvents({
      projectId: props.projectId,
      label: labelFilter.value,
      limit: 0
    })) || []
  } catch (err) {
    events.value = []
    message.error(String(err)) // 「请先选择接口项目」等中文错误原样展示
  } finally {
    listLoading.value = false
  }
  // 选中项：跳转指定的优先，其次列表第一条（右栏不至于空着）
  const wanted = pendingJumpId.value
  pendingJumpId.value = 0
  if (wanted) {
    selectedEventId.value = wanted
  } else if (!events.value.some(e => e.id === selectedEventId.value)) {
    selectedEventId.value = events.value.length ? events.value[0].id : 0
  }
}

/** label 候选（去重，按最近事件序）：失败不阻断列表，筛选器只是少几个选项 */
async function loadLabels() {
  if (!props.projectId) {
    labels.value = []
    return
  }
  try {
    labels.value = (await ListApiChangeLabels(props.projectId)) || []
  } catch (err) {
    labels.value = []
    message.error(String(err))
  }
}

/** 父侧跳转带进来的事件 id 已在上方声明；这里只挂首次加载与换项目重拉 */
onMounted(() => {
  loadLabels()
  loadEvents()
})

watch(() => props.projectId, () => {
  labelFilter.value = ''
  selectedEventId.value = 0
  pendingJumpId.value = 0 // 换项目后旧跳转意图作废
  loadLabels()
  loadEvents()
})

// ---------------- 展示 ----------------
const projectOptions = computed(() => props.projects.map(p => ({
  label: p.display || p.name, value: p.id
})))

const labelOptions = computed(() => [
  {label: '全部 label', value: ''},
  ...labels.value.map(l => ({label: l, value: l}))
])

const KIND_LABEL = {feature: 'feature', fix: 'fix', refactor: 'refactor', docs: 'docs'}
function kindText(k) {
  return KIND_LABEL[k] || k || '未分类'
}

/** breaking 三态徽标（0 无 / 1 破坏 / 2 需人工确认） */
function breakingOf(b) {
  if (b === 1) return {text: '破坏', cls: 'brk-1'}
  if (b === 2) return {text: '需确认', cls: 'brk-2'}
  return {text: '非破坏', cls: 'brk-0'}
}

/** createdAt 毫秒 → YYYY-MM-DD（D7 的显示形态 v7 · REQ-1024 · 2026-10-18） */
function fmtDay(ms) {
  const n = Number(ms || 0)
  if (!n) return '-'
  const d = new Date(n)
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

/** stats 徽标：只给非 0 的档；moved 恒 0 不展示（§13.16） */
function statList(e) {
  const s = e.stats || {}
  const out = []
  if (s.added) out.push({label: '新增', value: s.added, cls: 'st-add'})
  if (s.updated) out.push({label: '修改', value: s.updated, cls: 'st-upd'})
  if (s.removed) out.push({label: '删除', value: s.removed, cls: 'st-del'})
  if (s.restored) out.push({label: '恢复', value: s.restored, cls: 'st-res'})
  return out
}

/** 按 label 分组：组顺序 = 该 label 最近一次出现的事件顺序（列表本身已 seq DESC） */
const groups = computed(() => {
  if (mode.value !== 'label') return null
  const out = []
  const index = {}
  for (const e of events.value) {
    const key = e.label || '（未打标）'
    if (!(key in index)) {
      index[key] = out.length
      out.push({key, rows: []})
    }
    out[index[key]].rows.push(e)
  }
  return out
})

function selectEvent(e) {
  selectedEventId.value = e.id
}

function onProjectChange(id) {
  emit('selectProject', id || '')
}
</script>

<template>
  <div class="ev-page">
    <!-- ===== 顶部条 ===== -->
    <div class="ev-bar">
      <div class="bar-item">
        <span class="bar-label">项目</span>
        <n-select
          class="proj-select"
          size="small"
          :value="projectId || null"
          :options="projectOptions"
          placeholder="选择接口项目"
          @update:value="onProjectChange"
        />
      </div>
      <div class="bar-item">
        <span class="tab" :class="{active: mode === 'time'}" @click="mode = 'time'">按时间</span>
        <span class="tab" :class="{active: mode === 'label'}" @click="mode = 'label'">按需求标签</span>
      </div>
      <div class="bar-item">
        <span class="bar-label">label</span>
        <n-select
          class="label-select"
          size="small"
          :value="labelFilter"
          :options="labelOptions"
          :disabled="!labels.length"
          @update:value="(v) => { labelFilter = v || ''; loadEvents() }"
        />
      </div>
      <div class="bar-item bar-spacer"></div>
      <span class="bar-count mono">{{ events.length }} 条事件</span>
      <n-button size="small" :loading="listLoading" :disabled="!projectId" @click="loadEvents">刷新</n-button>
    </div>

    <!-- ===== 两栏：列表 + 详情 ===== -->
    <div class="ev-cols">
      <section class="ev-list">
        <div v-if="!projectId" class="col-empty">
          还没有选择项目。先到「接口」tab 导入一份文档（导入会自动建项目），或在这里选择项目。
        </div>
        <div v-else-if="listLoading" class="col-empty">加载中…</div>
        <div v-else-if="!events.length" class="col-empty">
          该项目还没有变更事件。<br>
          同一份文档重复导入会幂等短路（diff 为空即不生成事件，AC2）；手工编辑接口也不记事件。
        </div>

        <template v-else>
          <!-- 按时间平铺 -->
          <template v-if="mode === 'time'">
            <div
              v-for="e in events" :key="e.id"
              class="ev-row" :class="{active: e.id === selectedEventId}"
              @click="selectEvent(e)"
            >
              <div class="row-l1">
                <span class="ev-seq mono">v{{ e.seq }}</span>
                <span v-if="e.label" class="ev-label mono">{{ e.label }}</span>
                <span class="ev-kind mono" :class="'kind-' + (e.kind || 'none')">{{ kindText(e.kind) }}</span>
                <span class="ev-date mono">{{ fmtDay(e.createdAt) }}</span>
                <span class="ev-brk" :class="breakingOf(e.breaking).cls">{{ breakingOf(e.breaking).text }}</span>
              </div>
              <div class="row-title">{{ e.title || '（无标题）' }}</div>
              <div v-if="e.reason" class="row-reason" :title="e.reason">{{ e.reason }}</div>
              <div class="row-l3">
                <span v-for="s in statList(e)" :key="s.label" class="stat-chip" :class="s.cls">
                  {{ s.label }} {{ s.value }}
                </span>
                <span v-if="e.scopeMode === 'init'" class="mini-chip">init</span>
              </div>
            </div>
          </template>

          <!-- 按 label 分组（空 label 归「未打标」） -->
          <template v-else>
            <div v-for="g in groups" :key="g.key" class="ev-group">
              <div class="group-head">
                <span class="group-label mono">{{ g.key }}</span>
                <span class="group-count mono">{{ g.rows.length }} 次导入</span>
              </div>
              <div
                v-for="e in g.rows" :key="e.id"
                class="ev-row" :class="{active: e.id === selectedEventId}"
                @click="selectEvent(e)"
              >
                <div class="row-l1">
                  <span class="ev-seq mono">v{{ e.seq }}</span>
                  <span class="ev-kind mono" :class="'kind-' + (e.kind || 'none')">{{ kindText(e.kind) }}</span>
                  <span class="ev-date mono">{{ fmtDay(e.createdAt) }}</span>
                  <span class="ev-brk" :class="breakingOf(e.breaking).cls">{{ breakingOf(e.breaking).text }}</span>
                </div>
                <div class="row-title">{{ e.title || '（无标题）' }}</div>
                <div v-if="e.reason" class="row-reason" :title="e.reason">{{ e.reason }}</div>
                <div class="row-l3">
                  <span v-for="s in statList(e)" :key="s.label" class="stat-chip" :class="s.cls">
                    {{ s.label }} {{ s.value }}
                  </span>
                </div>
              </div>
            </div>
          </template>
        </template>
      </section>

      <section class="ev-detail">
        <ApiEventDetail :event-id="selectedEventId" @open-entry="(p) => emit('openEntry', p)"/>
      </section>
    </div>
  </div>
</template>

<style scoped>
/* 根节点：在父侧 tab 容器里占满剩余高度并自己分区滚动（同 ApiDocManager 的 .apidoc-cols 习惯） */
.ev-page { flex: 1; min-height: 0; display: flex; flex-direction: column; min-width: 0; }

/* ---- 顶部条 ---- */
.ev-bar {
  display: flex; align-items: center; gap: 14px; padding: 10px 16px; flex-shrink: 0;
  border-bottom: 1px solid rgba(255,255,255,0.08); flex-wrap: wrap;
}
.bar-item { display: flex; align-items: center; gap: 6px; min-width: 0; }
.bar-spacer { flex: 1; }
.bar-label { font-size: 12px; color: rgba(255,255,255,0.45); }
.bar-count { font-size: 12px; color: rgba(255,255,255,0.45); }
.proj-select { width: 180px; }
.label-select { width: 170px; }
.tab { font-size: 12px; color: rgba(255,255,255,0.5); cursor: pointer; padding: 3px 8px;
  border-radius: 5px; border: 1px solid transparent; }
.tab:hover { color: rgba(255,255,255,0.8); }
.tab.active { color: rgba(99,226,183,1); background: rgba(99,226,183,0.12);
  border-color: rgba(99,226,183,0.35); font-weight: 600; }

/* ---- 两栏 ---- */
.ev-cols { flex: 1; display: flex; min-height: 0; min-width: 0; }
.ev-list { width: 380px; flex-shrink: 0; overflow-y: auto; padding: 10px 8px;
  border-right: 1px solid rgba(255,255,255,0.08); }
.ev-detail { flex: 1; min-width: 0; overflow: hidden; }
.col-empty { font-size: 12px; color: rgba(255,255,255,0.4); line-height: 1.7; padding: 8px; }

/* ---- 事件行 ---- */
.ev-group { margin-bottom: 10px; }
.group-head { display: flex; align-items: center; gap: 8px; padding: 4px 8px; }
.group-label { font-size: 12px; font-weight: 600; color: rgba(99,179,237,1); }
.group-count { font-size: 11px; color: rgba(255,255,255,0.4); }

.ev-row {
  padding: 7px 8px; border-radius: 6px; cursor: pointer; font-size: 12px; min-width: 0;
  border: 1px solid transparent;
}
.ev-row:hover { background: rgba(255,255,255,0.06); }
.ev-row.active { background: rgba(99,226,183,0.15); border-color: rgba(99,226,183,0.35); }
.row-l1 { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; min-width: 0; }
.ev-seq { color: rgba(99,226,183,1); font-weight: 700; flex-shrink: 0; }
.ev-label { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: rgba(99,179,237,0.15);
  color: rgba(99,179,237,1); flex-shrink: 0; }
.ev-kind { font-size: 10px; padding: 1px 5px; border-radius: 4px; background: rgba(255,255,255,0.07);
  color: rgba(255,255,255,0.6); flex-shrink: 0; }
.kind-feature { background: rgba(99,226,183,0.15); color: rgba(99,226,183,1); }
.kind-fix { background: rgba(224,108,117,0.15); color: rgba(224,108,117,1); }
.kind-refactor { background: rgba(240,168,48,0.15); color: rgba(240,168,48,1); }
.kind-docs { background: rgba(99,179,237,0.15); color: rgba(99,179,237,1); }
.ev-date { font-size: 11px; color: rgba(255,255,255,0.4); margin-left: auto; flex-shrink: 0; }
.ev-brk { font-size: 10px; padding: 1px 5px; border-radius: 4px; flex-shrink: 0; }
.brk-0 { background: rgba(255,255,255,0.07); color: rgba(255,255,255,0.45); }
.brk-1 { background: rgba(224,108,117,0.18); color: rgba(224,108,117,1); }
.brk-2 { background: rgba(240,168,48,0.18); color: rgba(240,168,48,1); }
.row-title { margin-top: 3px; color: rgba(255,255,255,0.82); overflow: hidden; text-overflow: ellipsis;
  white-space: nowrap; }
.row-reason { margin-top: 2px; color: rgba(255,255,255,0.5); font-size: 11px; overflow: hidden;
  text-overflow: ellipsis; white-space: nowrap; }
.row-l3 { display: flex; gap: 6px; margin-top: 4px; flex-wrap: wrap; }
.stat-chip { font-size: 10px; padding: 1px 6px; border-radius: 4px; background: rgba(255,255,255,0.07);
  color: rgba(255,255,255,0.7); }
.st-add { background: rgba(99,226,183,0.15); color: rgba(99,226,183,1); }
.st-upd { background: rgba(99,179,237,0.15); color: rgba(99,179,237,1); }
.st-del { background: rgba(224,108,117,0.15); color: rgba(224,108,117,1); }
.st-res { background: rgba(240,168,48,0.15); color: rgba(240,168,48,1); }
.mini-chip { font-size: 10px; color: rgba(255,255,255,0.4); }
.mono { font-family: Consolas, Menlo, monospace; }
</style>
