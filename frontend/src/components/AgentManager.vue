<!--
  AgentManager.vue —— 智能体管理弹窗（独立入口，侧边栏"技能"按钮下方）。
  智能体 = 人设信息（名称/模型/系统提示词）+ 技能装配，两者均可在此编辑：
  ① 智能体卡片列表：数据来自后端 ListAgentsDetail（前端零硬编码）
     - 卡片展示：名称 + 内置标记 + 已装配技能数；点击进入详情弹窗（二级）
  ② 详情弹窗上部 = 信息编辑（名称/模型/系统提示词），保存调 SaveAgent
  ③ 详情弹窗下部 = 技能装配区：复选列表按"内置/自定义"分组（分类标记来自
     后端注册表侧，前端不按名字判断），保存调 SetAgentSkills 整组替换；
     装配落库即生效——Runner 每回合实时读装配，下轮对话工具列表自动变化，
     无需通知聊天侧。装配为 0 合法：退化为纯聊天智能体，界面给出提示。
  内置保护：内置智能体不可删除（本期无删除入口；新建下期做）。
  父子组件通信同 SkillManager：props = 父→子入参；emit = 子→父事件。
-->
<script setup>
import {computed, ref, watch} from 'vue'
import {useMessage} from 'naive-ui'
import {ListAgentsDetail, SaveAgent, SetAgentSkills} from '../../bindings/DevCraft/app.js'

// ---------------- props / emits 声明 ----------------
const props = defineProps({show: Boolean}) // 父组件传入的显隐状态
const emit = defineEmits(['update:show'])  // 可发出的事件名列表

const message = useMessage()

// ---------------- 智能体列表 ----------------
const agents = ref([])   // 智能体明细列表（含已装配技能与可选技能全集）
const detail = ref(null) // 当前打开详情弹窗的智能体；null = 未打开

// ---------------- 详情编辑状态 ----------------
// 信息编辑表单（打开详情时从明细拷贝，保存成功后重拉）
const form = ref({name: '', model: '', systemPrompt: ''})
// 装配勾选状态：已勾选的技能名数组（与 n-checkbox-group 双向绑定）
const checked = ref([])
const savingInfo = ref(false)   // 信息保存中（防重复点击）
const savingSkills = ref(false) // 装配保存中

/** 可选技能按分类标记分组（标记来自后端注册表侧；本期全部内置，自定义组预留） */
const builtinSkills = computed(() => (detail.value?.availableSkills || []).filter(s => s.builtin))
const customSkills = computed(() => (detail.value?.availableSkills || []).filter(s => !s.builtin))

/** 一句话摘要：取提示词的第一句（卡片摘要用） */
function summaryOf(text) {
  if (!text) return ''
  const idx = text.indexOf('。')
  return idx >= 0 ? text.slice(0, idx + 1) : text
}

/** 拉取智能体明细列表 */
async function loadAgents() {
  try {
    agents.value = (await ListAgentsDetail()) || []
  } catch (err) {
    message.error(String(err)) // Go 端的错误文字直接展示
  }
}

/** 打开详情弹窗：拷贝信息表单 + 回显已装配的勾选状态 */
function openDetail(a) {
  detail.value = a
  form.value = {name: a.name, model: a.model || '', systemPrompt: a.systemPrompt || ''}
  checked.value = a.skills.map(s => s.name)
}

/** 保存信息（名称/模型/系统提示词）：成功后刷新列表并同步当前详情对象 */
async function saveInfo() {
  savingInfo.value = true
  try {
    await SaveAgent(detail.value.id, form.value.name, form.value.model, form.value.systemPrompt)
    message.success('智能体信息已保存')
    await reloadDetail()
  } catch (err) {
    message.error(String(err)) // 后端校验错误（名称为空等）直接展示
  } finally {
    savingInfo.value = false
  }
}

/** 保存装配（整组替换）：勾选状态即最终装配，空勾选 = 纯聊天智能体 */
async function saveSkills() {
  savingSkills.value = true
  try {
    await SetAgentSkills(detail.value.id, checked.value)
    message.success('技能装配已保存，下轮对话生效')
    await reloadDetail()
  } catch (err) {
    message.error(String(err))
  } finally {
    savingSkills.value = false
  }
}

/** 重拉列表并把当前详情指向最新数据（保持编辑面板打开） */
async function reloadDetail() {
  const id = detail.value.id
  await loadAgents()
  const fresh = agents.value.find(a => a.id === id)
  if (!fresh) {
    detail.value = null
    return
  }
  detail.value = fresh
  form.value = {name: fresh.name, model: fresh.model || '', systemPrompt: fresh.systemPrompt || ''}
  checked.value = fresh.skills.map(s => s.name)
}

// watch 监听 props.show：打开时拉取最新列表；
// 关闭时收起详情弹窗（避免下次打开残留，同 SkillManager 模式）。
watch(() => props.show, (v) => {
  if (v) {
    loadAgents()
  } else {
    detail.value = null
  }
})
</script>

<template>
  <!-- 管理主弹窗：智能体卡片列表 -->
  <n-modal
    :show="props.show"
    preset="card"
    title="智能体管理"
    style="width: 760px"
    @update:show="emit('update:show', $event)"
  >
    <div class="agents-tip">
      智能体 = 人设提示词 + 装配的技能；对话中按会话绑定的智能体执行。点击卡片编辑信息与装配。
    </div>
    <div class="agent-grid">
      <div v-for="a in agents" :key="a.id" class="agent-card" @click="openDetail(a)">
        <div class="agent-head">
          <span class="agent-name">{{ a.name }}</span>
          <n-tag size="tiny" :type="a.builtin ? 'info' : 'success'">{{ a.builtin ? '内置' : '自定义' }}</n-tag>
        </div>
        <div class="agent-desc">{{ summaryOf(a.systemPrompt) || '（无提示词）' }}</div>
        <div class="agent-meta">已装配 {{ a.skills.length }} 个技能</div>
      </div>
    </div>
    <n-empty v-if="agents.length === 0" description="暂无智能体" size="small"/>
  </n-modal>

  <!-- 详情弹窗（二级，叠加在管理弹窗之上）：上部信息编辑 + 下部技能装配 -->
  <n-modal
    v-if="detail"
    :show="true"
    preset="card"
    :title="`智能体：${detail.name}`"
    style="width: 860px"
    :segmented="{content: true}"
    @update:show="detail = null"
  >
    <div class="detail-body">
      <!-- ===== 上部：信息编辑 ===== -->
      <n-form label-placement="left" label-width="90">
        <n-form-item label="名称" required>
          <n-input v-model:value="form.name" placeholder="智能体名称"/>
        </n-form-item>
        <n-form-item label="模型">
          <n-input v-model:value="form.model" placeholder="留空 = 跟随全局默认模型"/>
        </n-form-item>
        <n-form-item label="系统提示词">
          <n-input
            v-model:value="form.systemPrompt"
            type="textarea"
            :autosize="{minRows: 4, maxRows: 12}"
            placeholder="定义智能体的人设与规则（建议填写，决定它的行为风格）"
          />
        </n-form-item>
      </n-form>
      <div class="section-actions">
        <n-button type="primary" size="small" :loading="savingInfo" @click="saveInfo">保存信息</n-button>
      </div>

      <!-- ===== 下部：技能装配 ===== -->
      <n-divider title-placement="left">技能装配</n-divider>
      <div class="skills-tip">
        勾选即装配；保存后即时生效，下轮对话的工具列表自动变化。
      </div>

      <!-- 单个 checkbox-group 包住两个分组：勾选状态共享同一个 checked 数组，
           跨组勾选才不会互相覆盖（每组建独立 group 会各自整组回写） -->
      <n-checkbox-group v-model:value="checked">
        <div class="skill-group-title">内置技能</div>
        <div class="skill-check-list">
          <n-checkbox v-for="s in builtinSkills" :key="s.name" :value="s.name" class="skill-check">
            <span class="cb-name">{{ s.name }}</span>
            <span class="cb-desc">{{ summaryOf(s.description) }}</span>
          </n-checkbox>
        </div>

        <div class="skill-group-title">自定义技能</div>
        <div v-if="customSkills.length === 0" class="custom-empty">暂无自定义技能，敬请期待</div>
        <div v-else class="skill-check-list">
          <n-checkbox v-for="s in customSkills" :key="s.name" :value="s.name" class="skill-check">
            <span class="cb-name">{{ s.name }}</span>
            <span class="cb-desc">{{ summaryOf(s.description) }}</span>
          </n-checkbox>
        </div>
      </n-checkbox-group>

      <!-- 装配为 0：退化为纯聊天智能体（合法状态，给出提示） -->
      <div v-if="checked.length === 0" class="zero-tip">
        未装配任何技能：该智能体将作为纯聊天智能体，无任何工具。
      </div>

      <div class="section-actions">
        <span class="checked-count">已勾选 {{ checked.length }} 个技能</span>
        <n-button type="primary" size="small" :loading="savingSkills" @click="saveSkills">保存装配</n-button>
      </div>
    </div>
  </n-modal>
</template>

<style scoped>
/* ---- 智能体卡片列表 ---- */
.agents-tip { font-size: 12px; color: rgba(255,255,255,0.5); margin-bottom: 10px; }
.agent-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
.agent-card {
  border: 1px solid rgba(255,255,255,0.09); border-radius: 8px; padding: 12px;
  cursor: pointer; transition: border-color .15s, background .15s;
}
.agent-card:hover { border-color: rgba(99,179,237,0.6); background: rgba(99,179,237,0.06); }
.agent-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.agent-name { font-weight: 600; font-size: 14px; }
.agent-desc {
  font-size: 12px; color: rgba(255,255,255,0.6); margin-top: 6px; line-height: 1.6;
  display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden;
}
.agent-meta { font-size: 12px; color: rgba(255,255,255,0.4); margin-top: 6px; }

/* ---- 详情弹窗 ---- */
.detail-body { max-height: 75vh; overflow-y: auto; }
.section-actions { display: flex; align-items: center; gap: 10px; justify-content: flex-end; margin: 4px 0 6px; }
.checked-count { margin-right: auto; font-size: 12px; color: rgba(255,255,255,0.5); }
.skills-tip { font-size: 12px; color: rgba(255,255,255,0.5); margin-bottom: 10px; }
.skill-group-title { font-weight: 600; font-size: 13px; margin: 12px 0 8px; }
.skill-check-list { display: flex; flex-direction: column; gap: 8px; }
.skill-check { width: 100%; align-items: flex-start; }
.cb-name { font-family: Consolas, Menlo, monospace; font-size: 13px; font-weight: 600; }
.cb-desc { margin-left: 8px; font-size: 12px; color: rgba(255,255,255,0.5); }
.custom-empty {
  font-size: 12px; color: rgba(255,255,255,0.45);
  border: 1px dashed rgba(255,255,255,0.15); border-radius: 6px; padding: 10px 12px;
}
.zero-tip {
  margin-top: 12px; padding: 8px 10px; border-radius: 6px;
  background: rgba(240,168,48,0.08); border: 1px solid rgba(240,168,48,0.3);
  font-size: 12px; color: rgba(255,255,255,0.65);
}
</style>
