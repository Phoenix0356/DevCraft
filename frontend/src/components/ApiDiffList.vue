<!--
  ApiDiffList.vue —— 字段级 diff 载荷渲染（apidoc.DiffItem[]）。
  两处复用：导入向导（ApiDocImport 的 updated 展开区）与事件详情（ApiEventDetail 的明细行）。

  渲染契约（spec-draft §13.2 / §13.20，务必遵守）：
    · before/after 经 JSON 落库回读后是**对象**（FieldFragment / SchemaFragment），
      不能当字符串拼接；moved 场景才是字符串。这里统一走 fmtFragment 归一。
    · FieldFragment.required 是 -1/0/1 哨兵（-1 未知 / 0 否 / 1 是），不是布尔值。
    · 展示类型用 fragment.type（= type_raw 显示值），不使用归一化的 type_key。
    · added/restored 明细的 diff 是空数组，本组件收到空数组时显示说明文案。

  props：{diff: Array}  —— apidoc.DiffItem[]
  emit：无（纯展示）
-->
<script setup>
import {computed} from 'vue'

const props = defineProps({
  diff: {type: Array, default: () => []}
})

const OP_LABEL = {added: '新增', removed: '删除', updated: '修改', renamed: '改名', moved: '迁移'}
const SECTION_LABEL = {
  'meta': '接口元信息',
  'path.params': '路径参数',
  'request.query': '请求 Query',
  'request.body': '请求 Body',
  'request.header': '请求 Header',
  'response.body': '响应 Body'
}

/** required 三态哨兵 → 文案（-1 未知绝不猜成「否」） */
function reqText(v) {
  if (v === 1) return '必填'
  if (v === 0) return '非必填'
  return '必填未知'
}

function opLabel(op) {
  return OP_LABEL[op] || op || ''
}
function sectionLabel(s) {
  return SECTION_LABEL[s] || s || '-'
}

/** 单个字段片段 → 一行可读文本：name : type （必填态 @位置 默认值 ——说明） */
function fmtField(f) {
  if (!f || typeof f !== 'object') return String(f ?? '')
  const parts = []
  const name = f.name != null ? String(f.name) : '?'
  const type = f.type != null ? String(f.type) : ''
  parts.push(type ? `${name}: ${type}` : name)
  if ('required' in f) parts.push(reqText(f.required))
  if (f.position) parts.push(`@${f.position}`)
  if (f.default) parts.push(`默认 ${f.default}`)
  const line = parts.join(' · ')
  return f.desc ? `${line} —— ${f.desc}` : line
}

/**
 * before/after 归一成字符串：
 *   null/undefined → '—'
 *   string         → 原样（moved 的载荷是字符串）
 *   对象带 fields[] → SchemaFragment：结构名 + 字段逐行
 *   对象            → FieldFragment：单行
 *   其他           → JSON 串（防御未知形状，绝不 throw）
 */
function fmtFragment(v) {
  if (v === null || v === undefined) return '—'
  if (typeof v === 'string') return v || '—'
  if (typeof v !== 'object') return String(v)
  if (Array.isArray(v.fields)) {
    const head = `${v.name || '（未命名结构）'}（${v.fields.length} 个字段）`
    const body = v.fields.map(f => '  · ' + fmtField(f)).join('\n')
    return body ? `${head}\n${body}` : head
  }
  if (Array.isArray(v)) return v.map(x => fmtFragment(x)).join('\n') || '—'
  return fmtField(v) || '—'
}

const rows = computed(() => (props.diff || []).map(d => ({
  section: sectionLabel(d.section),
  key: d.key || '-',
  op: opLabel(d.op),
  opRaw: d.op || '',
  before: fmtFragment(d.before),
  after: fmtFragment(d.after),
  verdict: d.breaking ? '破坏性' : (d.needsConfirm ? '需人工确认' : '非破坏'),
  isBreak: !!d.breaking,
  isConfirm: !d.breaking && !!d.needsConfirm,
  rule: d.rule || ''
})))
</script>

<template>
  <div class="diff-list">
    <div v-if="!rows.length" class="diff-empty">
      该明细没有字段级 diff（新增 / 恢复的接口由条目本身与文档「变更点」表达，spec-draft §13.20）。
    </div>
    <table v-else class="diff-table">
      <thead>
      <tr><th>分区</th><th>字段</th><th>操作</th><th>改前</th><th>改后</th><th>判定</th></tr>
      </thead>
      <tbody>
      <tr v-for="(r, i) in rows" :key="i">
        <td>{{ r.section }}</td>
        <td class="mono">{{ r.key }}</td>
        <td :class="'op-' + r.opRaw">{{ r.op }}</td>
        <!-- pre-line：SchemaFragment 的多字段展开保换行，但不解释 markdown/HTML（零 v-html） -->
        <td class="mono frag">{{ r.before }}</td>
        <td class="mono frag">{{ r.after }}</td>
        <td>
          <span :class="{'ver-break': r.isBreak, 'ver-confirm': r.isConfirm}">{{ r.verdict }}</span>
          <span v-if="r.rule" class="rule mono" :title="r.rule">{{ r.rule }}</span>
        </td>
      </tr>
      </tbody>
    </table>
  </div>
</template>

<style scoped>
.diff-list { width: 100%; }
.diff-empty { font-size: 12px; color: rgba(255,255,255,0.45); line-height: 1.6; }
.diff-table { width: 100%; border-collapse: collapse; font-size: 12px; table-layout: fixed; }
.diff-table th, .diff-table td {
  border: 1px solid rgba(255,255,255,0.09); padding: 5px 8px; text-align: left;
  vertical-align: top; word-break: break-word; color: rgba(255,255,255,0.75);
}
.diff-table th { color: rgba(255,255,255,0.55); font-weight: 600; background: rgba(255,255,255,0.03); }
.mono { font-family: Consolas, Menlo, monospace; }
.frag { white-space: pre-line; }
.op-added { color: rgba(99,226,183,1); }
.op-removed, .op-moved { color: rgba(224,108,117,1); }
.op-updated, .op-renamed { color: rgba(99,179,237,1); }
.ver-break { color: rgba(224,108,117,1); }
.ver-confirm { color: rgba(240,168,48,1); }
.rule { display: block; margin-top: 2px; font-size: 10px; color: rgba(255,255,255,0.4);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
