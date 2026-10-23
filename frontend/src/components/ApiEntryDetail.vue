<!--
  ApiEntryDetail.vue —— 接口条目详情（接口文档视图的右栏，纯展示 + 少量自取数据）。

  布局结构（自上而下单列滚动）：
    ① 头部：method 徽标 + pathRaw + 中文名 + identityKey（可复制）+ visibility + parseState
       （partial/unparsed 时醒目提示「有未解析项，需人工核对」并给出原文段落入口）
    ② 元信息：Content-Type / 认证 / 基础路径断言凭证 / 更新时间
    ③ 请求参数：按 Path / Query / Body / Header 分组的参数表
    ④ 返回字段：一张表 + 命名结构（VO）列表（ambiguous 标「同名结构存在多种定义」）
    ⑤ 说明与备注（白-space: pre-line 原文，不走 markdown 管道 —— 本模块零 v-html）
    ⑥ 示例与 curl：n-code 高亮 + 一键复制（不做在线发请求，PRD D2）
    ⑦ 该接口的历史变更事件（GetApiEntry 返回的 detail.events，反向关联）
    ⑧ 操作区：手工编辑（n-modal 弹窗）/ 软删 / 恢复

  数据来源：props.entry（列表行，来自 ListApiEntries）→ 自取 detail（GetApiEntry(id)，
  含 pathFields/requestFields/responseFields/schemas/examples/events）。
  entry.id 变化时重新拉取；父组件只需换 entry 即可，不必传整个 detail。

  props / emit 契约：
    props: {entry: Object}  —— store.ApiEntry 行对象；null 时本组件显示空态
    emit('changed')         —— 手工保存 / 软删 / 恢复成功后通知父侧重拉列表
    emit('openEvent', {eventId, entryId}) —— 点击历史事件行；本组件只把意图冒泡出去，
                                             跳转由父侧 ApiDocManager 实现（已接好：切
                                             「变更事件」tab 并直接打开该事件详情）

  与后端的两条硬约定（务必遵守）：
    · 展示一律用 typeRaw（typeKey 有 MultipartFile→Multipartfile 归一副作用，只用于比较）
    · required 是 -1/0/1 哨兵（-1 未知 / 0 否 / 1 是），不是布尔值
    · 手工 SaveApiEntry 不产生变更事件 → 编辑弹窗必须明示这一点
-->
<script setup>
import {computed, ref, watch} from 'vue'
import {useMessage} from 'naive-ui'
import {GetApiEntry, SaveApiEntry, DeleteApiEntry, RestoreApiEntry} from '../../bindings/DevCraft/app.js'
import {copyText as writeClipboard} from '../lib/clipboard' // 共用剪贴板 helper（双路降级，见 lib/clipboard.js）

const props = defineProps({
  entry: {type: Object, default: null}
})
const emit = defineEmits(['changed', 'openEvent'])

const message = useMessage()

// ---------------- 详情数据 ----------------
const detail = ref(null)        // appsvc.ApiEntryDetail；null = 未加载/无选中
const showRaw = ref(false)      // 未解析原文（rawMarkdown）展开开关
const loading = ref(false)      // 详情加载中

/** 拉详情：GetApiEntry 一次带回字段/结构/示例/历史事件（后端已聚合，前端不再补拉） */
async function loadDetail(id) {
  if (!id) {
    detail.value = null
    return
  }
  loading.value = true
  // 必须先清空：否则切换条目的瞬间，参数表/示例/历史事件渲染的还是上一个接口的数据
  detail.value = null
  try {
    detail.value = (await GetApiEntry(id)) || null
  } catch (err) {
    detail.value = null
    message.error(String(err)) // Go 端中文错误原样展示
  } finally {
    loading.value = false
  }
}

const entry = computed(() => props.entry || {})
const isRemoved = computed(() => entry.value.status === 'removed')

/** method 徽标配色：仓库习惯是散落 rgba 字面量，这里按语义分三档（读/写/其他） */
const methodClass = computed(() => {
  const m = (entry.value.method || '').toUpperCase()
  if (m === 'GET') return 'm-get'
  if (m === 'POST' || m === 'PUT') return 'm-write'
  if (m === 'DELETE') return 'm-del'
  return 'm-other'
})

/** required 三态哨兵 → 文案（spec-draft §13.2：-1 未知 / 0 否 / 1 是） */
function reqText(v) {
  if (v === 1) return '是'
  if (v === 0) return '否'
  return '未知' // -1 或异常值一律按未知呈现，绝不猜成「否」
}

/** 原文段落必须从 GetApiEntry 的 detail.entry 取：ListApiEntries 刻意不返回
 *  raw_markdown（大文本列，store/api.go:634-637 直接 SELECT ''），列表行里它恒为空串 */
const rawMd = computed(() => detail.value?.entry?.rawMarkdown || props.entry?.rawMarkdown || '')

const parseHint = computed(() => {
  const s = entry.value.parseState || ''
  if (s === 'partial') return '有未解析项（文档缺小节或写法不合规范），需人工核对原文'
  if (s === 'unparsed') return '该条目基本未解析成功，内容不可信，请核对原文后重新导入'
  return ''
})

// ---------------- 参数分组 ----------------
/** 一条参数归属的分组名：优先 position 列（Path|Query|Body|Header），
 *  缺失时退回字段 kind（path|header|request）——存量散文文档常有缺列情况 */
function groupOf(f) {
  const p = (f.position || '').toUpperCase()
  if (p === 'PATH') return 'Path'
  if (p === 'QUERY') return 'Query'
  if (p === 'HEADER') return 'Header'
  if (p === 'BODY') return 'Body'
  if ((f.kind || '') === 'path') return 'Path'
  if ((f.kind || '') === 'header') return 'Header'
  return 'Body'
}

const GROUPS = ['Path', 'Query', 'Body', 'Header']

/** 请求参数按四组分桶（空组不显示）；Path 组并上后端单列的 pathFields */
const requestGroups = computed(() => {
  const d = detail.value
  if (!d) return []
  const all = [...(d.pathFields || []), ...(d.requestFields || [])]
  const buckets = {}
  for (const f of all) {
    const g = groupOf(f)
    ;(buckets[g] = buckets[g] || []).push(f)
  }
  return GROUPS.filter(g => buckets[g]?.length).map(g => ({name: g, fields: buckets[g]}))
})

const responseFields = computed(() => detail.value?.responseFields || [])
const schemas = computed(() => detail.value?.schemas || [])
const examples = computed(() => detail.value?.examples || [])
const events = computed(() => detail.value?.events || [])

/** 示例块的语言：后端存 language（json/bash 为主），curl 兜底成 bash */
function exLanguage(ex) {
  if (ex.language) return ex.language
  if (ex.kind === 'curl') return 'bash'
  return 'json'
}
const EX_KIND_LABEL = {
  req_example: '请求示例',
  res_example: '响应示例',
  curl: 'curl 命令',
  multipart_raw: 'multipart 原始报文',
  error_example: '错误响应示例'
}
function exLabel(ex) {
  return EX_KIND_LABEL[ex.kind] || ex.kind || '示例'
}

/** n-code 的 language 只认 highlight.js 的注册名，未知一律降级为 text（避免高亮报错） */
function codeLanguage(lang) {
  const l = (lang || '').toLowerCase()
  if (['json', 'bash', 'sh', 'shell', 'http', 'sql', 'xml', 'yaml'].includes(l)) return l
  if (l === 'curl') return 'bash'
  return 'text'
}

// ---------------- 复制 ----------------
/** 复制统一入口（薄包装，函数名与三个模板调用点保持不变）：
 *  ① 空内容 → warning「没有可复制的内容」，与改造前逐字一致；
 *  ② 写入走 lib/clipboard.js 的共用 copyText（现代 API → 老式降级双路）——
 *    原来这里只有单路 navigator 直写，服务器模式 http 非安全上下文下点「复制路径」必失败；
 *  ③ 成功 → success(tip || 默认文案)，tip 可选行为不变；两条路都不通 → error 提示，
 *    文案与 ChatApp 块级复制对齐（helper 返回 boolean 不再抛异常，故不带 err 详情）。
 *  组件层从此不再出现剪贴板原生 API 字面量（AC5）。 */
async function copyText(text, tip) {
  const s = text == null ? '' : String(text)
  if (!s) {
    message.warning('没有可复制的内容')
    return
  }
  if (await writeClipboard(s)) {
    message.success(tip || '已复制到剪贴板')
  } else {
    message.error('复制失败：当前环境剪贴板不可用')
  }
}

// ---------------- 手工编辑 ----------------
const editShow = ref(false)
const saving = ref(false)
const form = ref(null) // 编辑表单（深拷贝，避免直接改列表/详情对象）

/** 可手工编辑的字段只有：name / contentType / auth / visibility / description。
 *  后端 appsvc.SaveApiEntry 是「读-改-写白名单」（spec-draft §13.21）：先按 id 取库里既有行，
 *  只覆盖 name/description/contentType/auth/visibility/sort/domainId，其余列（身份列、双 hash、
 *  rawMarkdown、parseState、status、deletedAt…）一律沿用库里值 —— 所以漏传字段不会再被清零。
 *  但白名单里的空值语义必须照顾到：name='' / domainId='' / visibility='' / sort<=0 是「不改」，
 *  而 description / contentType / auth 是「入参为准，传空即清空」。
 *  ⇒ 提交前必须先有库里整行值打底（这里用 GetApiEntry 的 detail.entry 深拷贝，
 *    没拉到详情时退回列表行 props.entry），否则「只想改名字」会把说明清空掉。
 *  method/path/identityKey/projectId 即使在表单里也是只读的（后端拒绝身份变更与跨项目移动）。 */
function openEdit() {
  const e = detail.value?.entry || props.entry
  if (!e) return
  form.value = JSON.parse(JSON.stringify(e))
  editShow.value = true
}

async function saveEntry() {
  if (!form.value) return
  saving.value = true
  try {
    await SaveApiEntry({...form.value})
    message.success('已保存（手工修改不会记入变更事件）')
    editShow.value = false
    await loadDetail(form.value.id)
    emit('changed')
  } catch (err) {
    message.error(String(err)) // 「不支持修改方法/路径」等中文校验错误原样展示
  } finally {
    saving.value = false
  }
}

// ---------------- 软删 / 恢复 ----------------
const removing = ref(false)
const restoring = ref(false)

async function removeEntry() {
  const id = props.entry?.id
  if (!id) return
  removing.value = true
  try {
    await DeleteApiEntry(id)
    message.success('已删除（软删，可用「显示已删除」找回）')
    emit('changed')
  } catch (err) {
    message.error(String(err))
  } finally {
    removing.value = false
  }
}

async function restoreEntry() {
  const id = props.entry?.id
  if (!id) return
  restoring.value = true
  try {
    await RestoreApiEntry(id)
    message.success('已恢复该接口')
    await loadDetail(id)
    emit('changed')
  } catch (err) {
    message.error(String(err))
  } finally {
    restoring.value = false
  }
}

/** 事件行的破坏性标记：三态 0/1/2（2 等同破坏处理，spec-draft §7） */
function eventBreakingText(b) {
  if (b === 1) return '破坏性'
  if (b === 2) return '需确认'
  return ''
}

const CHANGE_TYPE_LABEL = {
  added: '新增', updated: '修改', removed: '删除', restored: '恢复', moved: '迁移', unchanged: '未变更'
}
function changeTypeLabel(t) {
  return CHANGE_TYPE_LABEL[t] || t || ''
}

// watch 必须放在所有 ref 声明之后：immediate:true 会在 setup 同步期执行回调，
// 若写在 editShow 声明之前会踩 const 的暂时性死区（ReferenceError: editShow）。
watch(() => props.entry?.id, (id) => {
  showRaw.value = false
  editShow.value = false
  loadDetail(id)
}, {immediate: true})
</script>

<template>
  <div class="ed">
    <n-empty v-if="!entry.id" description="选择一个接口查看详情" size="small" class="ed-empty"/>

    <template v-else>
      <!-- 加载中/详情拉取失败时，头部仍用列表行数据渲染，不至于整栏空白 -->
      <div v-if="loading" class="ed-loading">正在加载详情…</div>

      <!-- ===== ① 头部 ===== -->
      <div class="ed-head">
        <div class="ed-title-row">
          <span class="method-badge" :class="methodClass">{{ entry.method }}</span>
          <span class="ed-path mono" :title="entry.pathRaw">{{ entry.pathRaw }}</span>
          <n-button size="tiny" quaternary @click="copyText(entry.pathRaw, '路径已复制')">复制路径</n-button>
        </div>
        <div class="ed-name">{{ entry.name || '（无中文名）' }}</div>
        <div class="ed-id-row">
          <span class="ed-id-label">身份键</span>
          <span class="mono ed-id">{{ entry.identityKey }}</span>
          <n-button size="tiny" quaternary @click="copyText(entry.identityKey, '身份键已复制')">复制</n-button>
        </div>
        <div class="ed-tags">
          <n-tag size="tiny" :type="entry.visibility === 'internal' ? 'warning' : 'info'">
            {{ entry.visibility === 'internal' ? '内部端点' : '公开' }}
          </n-tag>
          <n-tag v-if="isRemoved" size="tiny" type="error">已删除</n-tag>
          <n-tag size="tiny" :type="entry.parseState === 'ok' ? 'success' : 'warning'">
            解析: {{ entry.parseState || 'unknown' }}
          </n-tag>
        </div>
      </div>

      <!-- 未解析醒目提示（R10：可见不静默） -->
      <div v-if="parseHint" class="ed-warn">
        <div class="ed-warn-title">⚠ {{ parseHint }}</div>
        <div class="ed-warn-body">
          <n-button size="tiny" quaternary @click="showRaw = !showRaw">
            {{ showRaw ? '收起原文' : '查看该接口原文' }}
          </n-button>
        </div>
        <pre v-if="showRaw" class="ed-raw">{{ rawMd || '（该条目没有留档原文）' }}</pre>
      </div>

      <!-- ===== ② 元信息 ===== -->
      <div class="ed-section">
        <div class="ed-section-title">元信息</div>
        <table class="kv-table">
          <tbody>
            <tr><th>Content-Type</th><td class="mono">{{ entry.contentType || '（未记录）' }}</td></tr>
            <tr><th>认证</th><td>{{ entry.auth || '（未记录）' }}</td></tr>
            <tr><th>基础路径断言</th><td class="mono">{{ entry.basePathAssert || '（无）' }}</td></tr>
            <!-- 领域：domainId 可为 ''（删空领域后残余软删条目会被置空，下次导入自愈重挂，
                 spec-draft §13.23）⇒ 必须渲染成「未归领域」而不是空白或报错。
                 本组件拿不到领域名（props 只有 entry），所以只给 id 与那句人话。 -->
            <tr>
              <th>领域</th>
              <td class="mono">
                <template v-if="entry.domainId">{{ entry.domainId }}</template>
                <template v-else><span class="no-domain">未归领域（领域为空，下次导入自愈）</span></template>
              </td>
            </tr>
            <tr><th>更新时间</th><td class="mono">{{ entry.updatedAt || '-' }}</td></tr>
          </tbody>
        </table>
      </div>

      <!-- ===== ③ 请求参数（按位置分组） ===== -->
      <div class="ed-section">
        <div class="ed-section-title">请求参数</div>
        <div v-if="!requestGroups.length" class="ed-none">该接口没有解析到请求参数（可能确实无参数，也可能文档缺该小节）。</div>
        <div v-for="g in requestGroups" :key="g.name" class="ed-sub">
          <div class="ed-sub-title">{{ g.name }}（{{ g.fields.length }}）</div>
          <table class="param-table">
            <thead>
            <tr><th>名称</th><th>类型</th><th>必填</th><th>默认值</th><th>说明</th></tr>
            </thead>
            <tbody>
            <tr v-for="(f, i) in g.fields" :key="f.name + i">
              <td class="mono">{{ f.name }}</td>
              <td class="mono">{{ f.typeRaw || '-' }}</td>
              <td :class="{'req-yes': f.required === 1, 'req-unknown': f.required === -1}">{{ reqText(f.required) }}</td>
              <td class="mono">{{ f.defaultValue || '-' }}</td>
              <td>{{ f.description || '' }}</td>
            </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- ===== ④ 返回字段 + 命名结构 ===== -->
      <div class="ed-section">
        <div class="ed-section-title">返回字段</div>
        <div v-if="!responseFields.length" class="ed-none">未解析到返回字段。</div>
        <table v-else class="param-table">
          <thead>
          <tr><th>名称</th><th>类型</th><th>必填</th><th>说明</th></tr>
          </thead>
          <tbody>
          <tr v-for="(f, i) in responseFields" :key="f.name + i">
            <td class="mono">{{ f.name }}</td>
            <td class="mono">{{ f.typeRaw || '-' }}</td>
            <td :class="{'req-yes': f.required === 1, 'req-unknown': f.required === -1}">{{ reqText(f.required) }}</td>
            <td>{{ f.description || '' }}</td>
          </tr>
          </tbody>
        </table>

        <template v-if="schemas.length">
          <div class="ed-sub-title schema-title">命名结构（VO，{{ schemas.length }}）</div>
          <div v-for="sd in schemas" :key="sd.schema.id" class="schema-block">
            <div class="schema-head">
              <span class="mono schema-name">{{ sd.schema.name }}</span>
              <span class="schema-scope">{{ sd.schema.ownerScope ? '接口内联结构' : '文档级结构' }}</span>
              <!-- ambiguous=1：同名结构存在多种定义，后端禁止按名自动展开（spec-draft §4） -->
              <n-tag v-if="sd.schema.ambiguous" size="tiny" type="warning">同名结构存在多种定义，未自动展开</n-tag>
            </div>
            <table v-if="sd.fields?.length" class="param-table">
              <thead>
              <tr><th>字段</th><th>类型</th><th>必填</th><th>说明</th></tr>
              </thead>
              <tbody>
              <tr v-for="(f, i) in sd.fields" :key="f.name + i">
                <td class="mono">{{ f.name }}</td>
                <td class="mono">{{ f.typeRaw || '-' }}</td>
                <td :class="{'req-yes': f.required === 1, 'req-unknown': f.required === -1}">{{ reqText(f.required) }}</td>
                <td>{{ f.description || '' }}</td>
              </tr>
              </tbody>
            </table>
            <div v-else-if="!sd.schema.ambiguous" class="ed-none">该结构没有字段（或字段在别处定义）。</div>
            <pre v-if="sd.schema.rawMarkdown" class="schema-raw">{{ sd.schema.rawMarkdown }}</pre>
          </div>
        </template>
      </div>

      <!-- ===== ⑤ 说明 ===== -->
      <div v-if="entry.description" class="ed-section">
        <div class="ed-section-title">说明</div>
        <div class="ed-desc">{{ entry.description }}</div>
      </div>

      <!-- ===== ⑥ 示例与 curl ===== -->
      <div class="ed-section">
        <div class="ed-section-title">示例与 curl</div>
        <div v-if="!examples.length" class="ed-none">没有保存的示例或 curl（示例改动不记入变更事件，PRD D9）。</div>
        <div v-for="ex in examples" :key="ex.id" class="ex-block">
          <div class="ex-head">
            <span class="ex-label">{{ exLabel(ex) }}</span>
            <n-button size="tiny" quaternary @click="copyText(ex.content, exLabel(ex) + ' 已复制')">复制</n-button>
          </div>
          <!-- n-code 的 code 是纯字符串属性，不经 markdown/v-html 管道：外部文本零注入面 -->
          <n-code :code="ex.content || ''" :language="codeLanguage(exLanguage(ex))" word-wrap/>
        </div>
      </div>

      <!-- ===== ⑦ 历史事件 ===== -->
      <div class="ed-section">
        <div class="ed-section-title">变更历史（{{ events.length }}）</div>
        <div v-if="!events.length" class="ed-none">该接口没有变更事件（手工编辑不会记入事件，导入才会）。</div>
        <div v-for="ev in events" :key="ev.eventId + '-' + ev.itemId" class="ev-row"
             @click="emit('openEvent', {eventId: ev.eventId, entryId: entry.id})">
          <span class="ev-seq mono">v{{ ev.seq }}</span>
          <span class="ev-ct" :class="'ct-' + ev.changeType">{{ changeTypeLabel(ev.changeType) }}</span>
          <span class="ev-title">{{ ev.title || '（无标题）' }}</span>
          <span v-if="ev.label" class="ev-label mono">{{ ev.label }}</span>
          <span v-if="eventBreakingText(ev.breaking)" class="ev-brk">{{ eventBreakingText(ev.breaking) }}</span>
        </div>
        <!-- 点击事件行 → emit('openEvent') → 父侧（ApiDocManager）切到「变更事件」tab
             并用该 eventId 直接打开事件详情（含字段级 diff 与当时的导入原文） -->
      </div>

      <!-- ===== ⑧ 操作区 ===== -->
      <div class="ed-actions">
        <n-button size="small" :disabled="!!loading" @click="openEdit">手工编辑</n-button>
        <n-popconfirm
          v-if="!isRemoved"
          positive-text="删除"
          negative-text="取消"
          @positive-click="removeEntry"
        >
          <template #trigger>
            <n-button size="small" type="error" ghost :loading="removing">删除接口</n-button>
          </template>
          软删后列表默认隐藏该接口，可用「显示已删除」找回；删除不会记入变更事件。
        </n-popconfirm>
        <n-button v-else size="small" type="primary" ghost :loading="restoring" @click="restoreEntry">
          恢复接口
        </n-button>
      </div>
      <div class="ed-note">
        提示：本栏「手工编辑」只改当前态，<b>不会记入变更事件</b>（PRD 已知代价：事件链可能有洞）。
        要留下可追溯的变更记录，请走「导入文档」。
      </div>
    </template>

    <!-- 手工编辑弹窗：结构照 AgentManager.vue:117-125（v-if + :show="true"，关闭即销毁） -->
    <n-modal
      v-if="editShow && form"
      :show="true"
      preset="card"
      title="手工编辑接口条目"
      style="width: 640px"
      :segmented="{content: true}"
      @update:show="editShow = false"
    >
      <div class="edit-body">
        <div class="edit-warn">
          手工修改<b>不会记入变更事件</b>；方法 / 路径 / 身份键由导入维护，此处不可改
          （后端会直接拒绝），也不支持跨项目移动。
        </div>
        <n-form label-placement="left" label-width="110">
          <n-form-item label="接口名称">
            <n-input v-model:value="form.name" placeholder="中文名（改动不记为接口变更）"/>
          </n-form-item>
          <n-form-item label="方法 / 路径">
            <div class="ro-line mono">{{ form.method }} {{ form.pathRaw }}</div>
          </n-form-item>
          <n-form-item label="Content-Type">
            <n-input v-model:value="form.contentType" placeholder="如 application/json"/>
          </n-form-item>
          <n-form-item label="认证">
            <n-input v-model:value="form.auth" placeholder="如 需要Token / 无"/>
          </n-form-item>
          <n-form-item label="可见性">
            <n-select
              v-model:value="form.visibility"
              :options="[{label: 'public（公开）', value: 'public'}, {label: 'internal（内部端点）', value: 'internal'}]"
            />
          </n-form-item>
          <n-form-item label="说明">
            <n-input v-model:value="form.description" type="textarea" :autosize="{minRows: 3, maxRows: 10}"
                     placeholder="接口用途说明（中文措辞改动不触发变更事件）"/>
          </n-form-item>
        </n-form>
        <div class="edit-actions">
          <n-button @click="editShow = false">取消</n-button>
          <n-button type="primary" :loading="saving" @click="saveEntry">保存</n-button>
        </div>
      </div>
    </n-modal>
  </div>
</template>

<style scoped>
/* 右栏：整栏纵向滚动，父级给宽度（min-width:0 保证长路径能省略号而非撑破布局） */
.ed { height: 100%; overflow-y: auto; padding: 14px 16px; min-width: 0; font-size: 13px; }
.ed-empty { margin-top: 60px; }
.ed-loading { font-size: 12px; color: rgba(255,255,255,0.45); margin-bottom: 8px; }
.mono { font-family: Consolas, Menlo, monospace; }

/* ---- 头部 ---- */
.ed-head { border-bottom: 1px solid rgba(255,255,255,0.08); padding-bottom: 12px; }
.ed-title-row { display: flex; align-items: center; gap: 8px; min-width: 0; }
.ed-path { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  font-size: 13px; color: rgba(255,255,255,0.9); }
.ed-name { font-size: 14px; font-weight: 600; margin-top: 6px; color: rgba(255,255,255,0.85); }
.ed-id-row { display: flex; align-items: center; gap: 6px; margin-top: 6px; min-width: 0; }
.ed-id-label { font-size: 12px; color: rgba(255,255,255,0.45); flex-shrink: 0; }
.ed-id { font-size: 12px; color: rgba(255,255,255,0.6); overflow: hidden; text-overflow: ellipsis;
  white-space: nowrap; }
.ed-tags { display: flex; gap: 6px; margin-top: 8px; flex-wrap: wrap; }

/* method 徽标：等宽 + 语义底色（读=绿 / 写=蓝 / 改=琥珀 / 删=红） */
.method-badge {
  flex-shrink: 0; font-family: Consolas, Menlo, monospace; font-size: 11px; font-weight: 700;
  padding: 2px 6px; border-radius: 4px; letter-spacing: .5px;
}
.m-get { background: rgba(99,226,183,0.18); color: rgba(99,226,183,1); }
.m-write { background: rgba(99,179,237,0.18); color: rgba(99,179,237,1); }
.m-del { background: rgba(224,108,117,0.18); color: rgba(224,108,117,1); }
.m-other { background: rgba(240,168,48,0.18); color: rgba(240,168,48,1); }

/* ---- 未解析警告块（琥珀，沿用 DeployFlowConfig.vue:390-394 的观感） ---- */
.ed-warn {
  margin-top: 10px; padding: 8px 10px; border-radius: 6px;
  background: rgba(240,168,48,0.08); border: 1px solid rgba(240,168,48,0.3);
  font-size: 12px; color: rgba(255,255,255,0.7); line-height: 1.6;
}
.ed-warn-title { color: rgba(240,168,48,1); font-weight: 600; }
.ed-warn-body { margin-top: 4px; }
.ed-raw {
  margin: 8px 0 0; padding: 8px 10px; border-radius: 6px; max-height: 260px; overflow: auto;
  background: rgba(0,0,0,0.35); border: 1px solid rgba(255,255,255,0.09);
  font-family: Consolas, Menlo, monospace; font-size: 12px; color: rgba(255,255,255,0.75);
  white-space: pre-wrap; word-break: break-all;
}

/* ---- 分区 ---- */
.ed-section { margin-top: 18px; }
.ed-section-title { font-weight: 600; font-size: 13px; margin-bottom: 8px; color: rgba(255,255,255,0.9); }
.ed-sub { margin-bottom: 12px; }
.ed-sub-title { font-size: 12px; color: rgba(255,255,255,0.5); margin: 6px 0; }
.schema-title { margin-top: 12px; }
.ed-none { font-size: 12px; color: rgba(255,255,255,0.45); }
.ed-desc { white-space: pre-line; font-size: 13px; line-height: 1.7; color: rgba(255,255,255,0.85); }

/* ---- 表格：本模块所有表都走 param-table（仓库原无 table 样式，这里补齐） ---- */
.param-table, .kv-table { width: 100%; border-collapse: collapse; font-size: 12px; table-layout: fixed; }
.param-table th, .param-table td,
.kv-table th, .kv-table td {
  border: 1px solid rgba(255,255,255,0.09); padding: 6px 10px; text-align: left;
  vertical-align: top; word-break: break-word;
}
.param-table th, .kv-table th { color: rgba(255,255,255,0.55); font-weight: 600; background: rgba(255,255,255,0.03); }
.param-table td { color: rgba(255,255,255,0.75); }
.kv-table th { width: 110px; }
.kv-table td { color: rgba(255,255,255,0.75); }
.req-yes { color: rgba(224,108,117,0.95); }
.req-unknown { color: rgba(240,168,48,0.95); }
.no-domain { color: rgba(240,168,48,0.9); }

/* ---- 命名结构 ---- */
.schema-block { border: 1px solid rgba(255,255,255,0.09); border-radius: 8px; padding: 10px; margin-bottom: 8px; }
.schema-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-bottom: 6px; }
.schema-name { font-weight: 600; color: rgba(99,179,237,1); }
.schema-scope { font-size: 11px; color: rgba(255,255,255,0.45); }
.schema-raw {
  margin: 8px 0 0; padding: 8px; border-radius: 6px; max-height: 200px; overflow: auto;
  background: rgba(0,0,0,0.3); font-family: Consolas, Menlo, monospace; font-size: 11px;
  color: rgba(255,255,255,0.65); white-space: pre-wrap; word-break: break-all;
}

/* ---- 示例 ---- */
.ex-block { border: 1px solid rgba(255,255,255,0.09); border-radius: 8px; padding: 8px 10px; margin-bottom: 8px; }
.ex-block :deep(pre) { background: transparent; padding: 0; margin-top: 4px; font-size: 12px;
  white-space: pre-wrap; word-break: break-all; }
.ex-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.ex-label { font-size: 12px; color: rgba(255,255,255,0.55); }

/* ---- 历史事件行（选中态沿用仓库绿色：ChatApp.vue:871） ---- */
.ev-row {
  display: flex; align-items: center; gap: 8px; padding: 6px 8px; border-radius: 6px;
  cursor: pointer; font-size: 12px; min-width: 0;
}
.ev-row:hover { background: rgba(255,255,255,0.06); }
.ev-seq { color: rgba(99,226,183,1); flex-shrink: 0; }
.ev-ct { flex-shrink: 0; padding: 1px 5px; border-radius: 4px; background: rgba(255,255,255,0.07);
  color: rgba(255,255,255,0.7); }
.ct-added { color: rgba(99,226,183,1); }
.ct-updated { color: rgba(99,179,237,1); }
.ct-removed { color: rgba(224,108,117,1); }
.ct-restored { color: rgba(240,168,48,1); }
.ev-title { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  color: rgba(255,255,255,0.8); }
.ev-label { color: rgba(255,255,255,0.45); flex-shrink: 0; }
.ev-brk { color: rgba(224,108,117,1); flex-shrink: 0; }

/* ---- 操作区与底部说明 ---- */
.ed-actions { display: flex; gap: 8px; margin-top: 20px; }
.ed-note {
  margin-top: 10px; padding: 8px 10px; border-radius: 6px; font-size: 12px; line-height: 1.6;
  background: rgba(240,168,48,0.08); border: 1px solid rgba(240,168,48,0.3); color: rgba(255,255,255,0.65);
}

/* ---- 编辑弹窗 ---- */
.edit-body { max-height: 70vh; overflow-y: auto; }
.edit-warn {
  padding: 8px 10px; border-radius: 6px; font-size: 12px; line-height: 1.6; margin-bottom: 12px;
  background: rgba(240,168,48,0.08); border: 1px solid rgba(240,168,48,0.3); color: rgba(255,255,255,0.7);
}
.ro-line { font-size: 12px; color: rgba(255,255,255,0.55); word-break: break-all; }
.edit-actions { display: flex; gap: 10px; justify-content: flex-end; margin-top: 8px; }
</style>
