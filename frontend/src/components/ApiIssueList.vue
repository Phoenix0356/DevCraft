<!--
  ApiIssueList.vue —— 结构化校验输出（apidoc.Issue[]）分区渲染。
  本轮用于导入向导的「阻断问题 E-*」（红底）与「警告 W-*」（琥珀底）两栏。
  事件详情页（ApiEventDetail）不需要它：Issue 是「预览产物」，后端事件 DTO 里没有 issues 字段
  （文档自身问题只在校验/预览阶段存在，PRD Risk 5 的分栏在预览侧已经落实）。

  内联红底列表的观感照抄 DeployFlowConfig.vue:359-361 + 样式 :410-414
  （一条文档可能几十个问题，toast 一闪即失，所以问题明细不走 toast）。

  props：{issues: Issue[], level: 'E' | 'W'}  —— level 只决定配色与标题措辞
  emit：无（纯展示）
-->
<script setup>
import {computed} from 'vue'

const props = defineProps({
  issues: {type: Array, default: () => []},
  level: {type: String, default: 'E'}
})

const rows = computed(() => (props.issues || []).filter(i => (i.level || '') === props.level))
const isErr = computed(() => props.level === 'E')
const title = computed(() => isErr.value
  ? `阻断问题 E-*（${rows.value.length}）—— 整批不写入，事务回滚`
  : `警告 W-*（${rows.value.length}）—— 不阻断导入，但请人工核对`)
</script>

<template>
  <div v-if="rows.length" class="zone" :class="isErr ? 'zone-err' : 'zone-warn'">
    <div class="zone-title">{{ title }}</div>
    <div v-for="(e, i) in rows" :key="i" class="issue">
      <div class="issue-head">
        <span class="code mono">{{ e.code }}</span>
        <span class="line mono">第 {{ e.line }} 行</span>
      </div>
      <div v-if="e.snippet" class="snippet mono">{{ e.snippet }}</div>
      <div v-if="e.want" class="want">期望写法：{{ e.want }}</div>
      <div v-if="e.hint" class="hint-line">修复提示：{{ e.hint }}</div>
    </div>
  </div>
</template>

<style scoped>
.zone { border-radius: 6px; padding: 10px 12px; font-size: 12px; flex-shrink: 0; }
.zone-err { background: rgba(224,108,117,0.08); border: 1px solid rgba(224,108,117,0.35);
  color: rgba(255,255,255,0.78); }
.zone-warn { background: rgba(240,168,48,0.08); border: 1px solid rgba(240,168,48,0.3);
  color: rgba(255,255,255,0.75); }
.zone-title { font-weight: 600; margin-bottom: 6px; }
.zone-err .zone-title { color: rgba(224,108,117,1); }
.zone-warn .zone-title { color: rgba(240,168,48,1); }
.issue { padding: 6px 0; border-top: 1px dashed rgba(255,255,255,0.12); line-height: 1.7; }
.issue:first-of-type { border-top: none; }
.issue-head { display: flex; gap: 10px; align-items: center; }
.code { font-weight: 700; color: rgba(255,255,255,0.9); }
.line { color: rgba(255,255,255,0.5); }
.snippet { margin-top: 2px; color: rgba(255,255,255,0.6); word-break: break-all; white-space: pre-wrap; }
.want, .hint-line { margin-top: 2px; color: rgba(255,255,255,0.65); }
.mono { font-family: Consolas, Menlo, monospace; }
</style>
