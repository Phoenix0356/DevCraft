<!--
  SkillManager.vue —— 技能管理视图（右侧主区三态之一：聊天/技能/智能体，非弹窗）。
  ChatApp 以 v-if 渲染本组件：进入"技能"视图即挂载、离开即卸载。
  布局：卡片网格占满整个视图（响应式网格，列数随宽度自适应）：
  ① 技能卡片：系统全部技能（数据来自后端注册表 ListSkills，前端零硬编码）
     - 卡片展示：名称 + 内置标记 + 一句话摘要；点击弹出详情弹窗
  ② 详情弹窗：描述全文 + 参数说明表（空 schema 兜底"本技能无需参数"）
  ③ 部署技能（deploy_run_flow）特殊：加宽大弹窗 = 介绍 + 内嵌部署流程配置区
     （DeployFlowConfig 组件，列表+新建/编辑/删除——单一入口，能力零缺失）
  ④ 流程增删改后重拉技能列表并同步弹窗展示：部署技能的描述是动态拼接的流程清单
-->
<script setup>
import {computed, onMounted, ref} from 'vue'
import {useMessage} from 'naive-ui'
import {ListSkills} from '../../bindings/DevCraft/app.js'
import DeployFlowConfig from './DeployFlowConfig.vue'

// 部署技能名（后端 internal/skill/deploy.SkillName 常量，前端唯一硬编码的技能名——
// 只用于识别"该技能的详情弹窗需要内嵌流程配置区"这一展示差异，数据仍全来自后端）。
const DEPLOY_SKILL = 'deploy_run_flow'

const message = useMessage()

// ---------------- 技能管理 ----------------
const skills = ref([])   // 技能元数据列表（后端注册表全量，按名排序）
const detail = ref(null) // 当前打开详情弹窗的技能；null = 未打开

/** 当前打开的是否为部署技能（决定弹窗宽度与是否内嵌流程配置区） */
const isDeployDetail = computed(() => detail.value?.name === DEPLOY_SKILL)

/** 从参数 JSON Schema 提取参数表行：{name, type, required, desc}[] */
const detailParams = computed(() => {
  let schema = detail.value?.parameters
  if (typeof schema === 'string') schema = JSON.parse(schema) // 防御：正常路径已是对象
  const propsMap = schema?.properties || {}
  const required = new Set(schema?.required || [])
  return Object.keys(propsMap).map(name => ({
    name,
    type: propsMap[name]?.type || 'any',
    required: required.has(name),
    desc: propsMap[name]?.description || ''
  }))
})

/** 一句话摘要：取描述的第一句（部署技能的描述含动态流程清单，只取首句做卡片摘要） */
function summaryOf(desc) {
  if (!desc) return ''
  const idx = desc.indexOf('。')
  return idx >= 0 ? desc.slice(0, idx + 1) : desc
}

/** 打开技能详情弹窗 */
function openDetail(s) {
  detail.value = s
}

/** 流程增删改后刷新技能列表：部署技能的描述动态拼接流程清单，需重新拉取。
 *  详情数据同步：弹窗有遮罩，重拉期间用户不可能切换卡片——
 *  只需把当前打开的技能指向新列表中的最新对象，弹窗里的描述即为最新值 */
async function onFlowsChanged() {
  await loadSkills()
  const name = detail.value?.name
  if (!name) return
  const fresh = skills.value.find(s => s.name === name)
  if (fresh) detail.value = fresh
}

/** 拉取技能列表（后端注册表全量元数据） */
async function loadSkills() {
  try {
    skills.value = (await ListSkills()) || []
  } catch (err) {
    message.error(String(err)) // Go 端的错误文字直接展示
  }
}

// 视图挂载即加载数据（每次进入技能视图都会拿到最新注册表快照）
onMounted(loadSkills)
</script>

<template>
  <!-- 卡片网格占满整个视图：响应式网格，列数随容器宽度自适应 -->
  <div class="mgr-view">
    <div class="mgr-tip">技能是助手可调用的能力单元；对话中由模型按需自动调用。点击卡片查看详情。</div>
    <div class="skill-grid">
      <div v-for="s in skills" :key="s.name" class="skill-card" @click="openDetail(s)">
        <div class="skill-head">
          <span class="skill-name">{{ s.name }}</span>
          <!-- 内置/自定义分类标记：来自后端注册表侧（ListSkills 的 builtin 字段，
               同源智能体装配区的 AgentSkillInfo.builtin），前端零硬编码、不按名字判断 -->
          <n-tag size="tiny" :type="s.builtin ? 'info' : 'success'">
            {{ s.builtin ? '内置' : '自定义' }}
          </n-tag>
        </div>
        <div class="skill-desc">{{ summaryOf(s.description) }}</div>
      </div>
    </div>
    <n-empty v-if="skills.length === 0" description="暂无已注册技能" size="small" class="empty"/>
  </div>

  <!-- 详情弹窗：普通技能 = 描述全文 + 参数表；部署技能 = 加宽，内嵌部署流程配置区 -->
  <n-modal
    v-if="detail"
    :show="true"
    preset="card"
    :title="detail.name"
    :style="{width: isDeployDetail ? '960px' : '640px'}"
    :segmented="{content: true}"
    @update:show="detail = null"
  >
    <div class="detail-body" :class="{'detail-body-large': isDeployDetail}">
      <!-- 描述全文（部署技能的描述含动态流程清单，保留换行） -->
      <div class="detail-desc">{{ detail.description }}</div>

      <div class="detail-section-title">参数说明</div>
      <table v-if="detailParams.length" class="param-table">
        <thead>
          <tr><th>参数名</th><th>类型</th><th>必填</th><th>说明</th></tr>
        </thead>
        <tbody>
          <tr v-for="p in detailParams" :key="p.name">
            <td class="mono">{{ p.name }}</td>
            <td>{{ p.type }}</td>
            <td>{{ p.required ? '是' : '否' }}</td>
            <td>{{ p.desc }}</td>
          </tr>
        </tbody>
      </table>
      <div v-else class="no-params">本技能无需参数。</div>

      <!-- 部署技能：内嵌部署流程配置区（列表+新建/编辑/删除，单一入口） -->
      <template v-if="isDeployDetail">
        <n-divider title-placement="left">部署流程配置</n-divider>
        <DeployFlowConfig @changed="onFlowsChanged"/>
      </template>
    </div>
  </n-modal>
</template>

<style scoped>
/* ---- 视图容器：卡片网格占满整个视图，内容超高时整体纵向滚动 ---- */
.mgr-view { height: 100%; overflow-y: auto; padding: 16px 20px; }
.mgr-tip { font-size: 12px; color: rgba(255,255,255,0.5); line-height: 1.6; margin-bottom: 12px; }
.empty { margin-top: 40px; }

/* ---- 技能卡片网格：列数随宽度自适应（每列至少 280px） ---- */
.skill-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 12px; }
.skill-card {
  border: 1px solid rgba(255,255,255,0.09); border-radius: 8px; padding: 12px;
  cursor: pointer; transition: border-color .15s, background .15s;
}
.skill-card:hover { border-color: rgba(99,179,237,0.6); background: rgba(99,179,237,0.06); }
.skill-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.skill-name { font-family: Consolas, Menlo, monospace; font-weight: 600; font-size: 13px; }
.skill-desc { font-size: 12px; color: rgba(255,255,255,0.6); margin-top: 6px; line-height: 1.6; }

/* ---- 详情弹窗 ---- */
.detail-body { max-height: 70vh; overflow-y: auto; }
/* 部署技能大弹窗：内嵌流程配置区内容更长，给更高的可视区域（宽度由 :style 控制） */
.detail-body-large { max-height: 80vh; }
.detail-desc { white-space: pre-line; font-size: 13px; line-height: 1.7; color: rgba(255,255,255,0.85); }
.detail-section-title { font-weight: 600; font-size: 13px; margin: 14px 0 8px; }
.param-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.param-table th, .param-table td {
  border: 1px solid rgba(255,255,255,0.09); padding: 6px 10px; text-align: left;
}
.param-table th { color: rgba(255,255,255,0.55); font-weight: 600; }
.param-table td { color: rgba(255,255,255,0.75); }
.mono { font-family: Consolas, Menlo, monospace; }
.no-params { font-size: 12px; color: rgba(255,255,255,0.45); }
</style>
