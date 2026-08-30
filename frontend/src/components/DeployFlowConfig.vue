<!--
  DeployFlowConfig.vue —— 部署流程配置区（从 SettingsModal 原"部署流程"标签页整体迁移）。
  一键部署的声明式流程管理：列表 + 编辑表单（新建/编辑共用）+ 删除。
    流程 = 名称 + 描述 + 执行目标（本机/SSH）+ 参数声明（可选校验正则）
         + 有序命令步骤（每行一条命令，{{参数名}} 占位符）
    流程只是"定义"；真正执行必须由对话中的审批卡片人工批准。
  现在作为"技能管理"中 deploy_run_flow 技能介绍弹窗的内嵌区使用（单一入口）。
  流程保存/删除成功后 emit('changed')：父组件借此刷新技能列表——
  部署技能的描述是动态拼接的流程清单，需要重新拉取才反映变更。
-->
<script setup>
import {onMounted, ref} from 'vue'
import {useMessage} from 'naive-ui'
import {ListDeployFlows, SaveDeployFlow, DeleteDeployFlow} from '../../bindings/DevCraft/app.js'

const emit = defineEmits(['changed']) // 流程发生增删改时通知父组件
const message = useMessage()

const flows = ref([])      // 流程列表
const editing = ref(null)  // 正在编辑的流程表单；null = 未打开编辑器

/** 空白编辑表单（新建用；编辑时会整体替换） */
function emptyFlowForm() {
  return {
    id: 0,
    name: '',
    description: '',
    target: 'local',
    params: [],   // 动态行：{name, desc, pattern}
    stepsText: '' // 步骤多行文本：每行一条命令
  }
}

/** 拉取流程列表（组件每次挂载都加载，保证弹窗打开即最新） */
async function loadFlows() {
  flows.value = (await ListDeployFlows()) || []
}

onMounted(() => {
  loadFlows().catch(err => message.error(String(err)))
})

/** 打开编辑器（新建 / 编辑现有流程） */
function editFlow(f) {
  if (!f) {
    editing.value = emptyFlowForm()
    return
  }
  editing.value = {
    id: f.id,
    name: f.name,
    description: f.description || '',
    target: f.target || 'local',
    // 深拷贝参数行，避免编辑中直接改动列表数据
    params: (f.params || []).map(p => ({name: p.name, desc: p.desc || '', pattern: p.pattern || ''})),
    stepsText: (f.steps || []).join('\n')
  }
}

function addParamRow() {
  editing.value.params.push({name: '', desc: '', pattern: ''})
}

function removeParamRow(i) {
  editing.value.params.splice(i, 1) // splice 原地删除（保持响应式）
}

/** 保存流程：组装载荷（过滤空参数行、步骤按行拆分）后调绑定 */
async function saveFlow() {
  const f = editing.value
  const payload = {
    id: f.id,
    name: f.name.trim(),
    description: f.description.trim(),
    target: f.target,
    params: f.params
      .map(p => ({name: p.name.trim(), desc: p.desc.trim(), pattern: p.pattern.trim()}))
      .filter(p => p.name !== ''),
    steps: f.stepsText.split('\n').map(s => s.trim()).filter(s => s !== '')
  }
  try {
    await SaveDeployFlow(payload)
    message.success('部署流程已保存')
    editing.value = null
    await loadFlows()
    emit('changed')
  } catch (err) {
    message.error(String(err)) // 后端校验错误（名称重复/正则不合法等）直接展示
  }
}

/** 删除流程（执行历史保留） */
async function removeFlow(f) {
  try {
    await DeleteDeployFlow(f.id)
    message.success(`已删除流程「${f.name}」`)
    await loadFlows()
    emit('changed')
  } catch (err) {
    message.error(String(err))
  }
}
</script>

<template>
  <div class="deploy-config">
    <div class="deploy-toolbar">
      <span class="deploy-tip">
        流程由你声明式定义；对话中说"部署 xx"触发，
        执行前必须经聊天内审批卡片人工批准。
      </span>
      <n-button v-if="!editing" size="small" type="primary" ghost @click="editFlow(null)">+ 新建流程</n-button>
    </div>

    <!-- 编辑器（新建/编辑共用） -->
    <div v-if="editing" class="flow-editor">
      <n-form label-placement="left" label-width="90">
        <n-form-item label="流程名称" required>
          <n-input v-model:value="editing.name" placeholder="如 web-deploy（对话中按名称触发）"/>
        </n-form-item>
        <n-form-item label="描述">
          <n-input v-model:value="editing.description" placeholder="用途说明（会展示给 LLM 帮助其选择流程）"/>
        </n-form-item>
        <n-form-item label="执行目标">
          <n-select
            v-model:value="editing.target"
            :options="[
              {label: '本机执行（本机 shell，注意风险）', value: 'local'},
              {label: 'SSH 远程主机（设置页配置的连接）', value: 'ssh'}
            ]"
          />
        </n-form-item>

        <!-- 参数声明动态行：名称 + 说明 + 可选校验正则 -->
        <n-form-item label="参数声明">
          <div class="param-rows">
            <div v-for="(p, i) in editing.params" :key="i" class="param-row">
              <n-input v-model:value="p.name" size="small" placeholder="参数名（字母开头）" style="width: 140px"/>
              <n-input v-model:value="p.desc" size="small" placeholder="说明（如：版本号）"/>
              <n-input v-model:value="p.pattern" size="small" placeholder="校验正则（可选，如 \d+\.\d+\.\d+）"/>
              <n-button size="small" quaternary @click="removeParamRow(i)">✕</n-button>
            </div>
            <n-button size="small" dashed @click="addParamRow">+ 添加参数</n-button>
            <div class="param-hint">
              声明的参数在触发部署时全部必填；命令步骤中用
              <code v-pre>{{参数名}}</code> 占位符引用。
            </div>
          </div>
        </n-form-item>

        <!-- 步骤多行文本：每行一条命令 -->
        <n-form-item label="命令步骤" required>
          <n-input
            v-model:value="editing.stepsText"
            type="textarea"
            :autosize="{minRows: 4, maxRows: 10}"
            placeholder="每行一条命令，按顺序执行，例如：&#10;docker pull registry/web:{{version}}&#10;docker restart web"
          />
        </n-form-item>
      </n-form>
      <div class="actions">
        <n-button @click="editing = null">取消</n-button>
        <n-button type="primary" @click="saveFlow">保存流程</n-button>
      </div>
    </div>

    <!-- 流程列表 -->
    <div v-else class="flow-list">
      <div v-for="f in flows" :key="f.id" class="flow-item">
        <div class="flow-info">
          <div class="flow-name">
            {{ f.name }}
            <n-tag size="tiny" :type="f.target === 'ssh' ? 'info' : 'warning'">
              {{ f.target === 'ssh' ? 'SSH' : '本机' }}
            </n-tag>
          </div>
          <div class="flow-desc">{{ f.description || '（无描述）' }}</div>
          <div class="flow-meta">
            {{ f.steps?.length || 0 }} 步命令
            <template v-if="f.params?.length"> · 参数: {{ f.params.map(p => p.name).join(', ') }}</template>
          </div>
        </div>
        <div class="flow-actions">
          <n-button size="tiny" @click="editFlow(f)">编辑</n-button>
          <n-button size="tiny" type="error" ghost @click="removeFlow(f)">删除</n-button>
        </div>
      </div>
      <n-empty v-if="flows.length === 0" description="还没有部署流程，点右上角新建" size="small"/>
    </div>

    <div class="security-note">
      安全提示：部署会在目标机器上执行任意命令（高危写操作）。参数值来自 LLM，
      已做引号转义与可选正则校验；命令模板本身由你编写，请自行评估风险。
    </div>
  </div>
</template>

<style scoped>
.deploy-toolbar { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }
.deploy-tip { flex: 1; font-size: 12px; color: rgba(255,255,255,0.5); }
.flow-editor { border: 1px solid rgba(255,255,255,0.1); border-radius: 8px; padding: 14px; }
.param-rows { display: flex; flex-direction: column; gap: 6px; width: 100%; }
.param-row { display: flex; gap: 6px; align-items: center; }
.param-hint { font-size: 12px; color: rgba(255,255,255,0.45); }
.flow-list { display: flex; flex-direction: column; gap: 8px; }
.flow-item {
  display: flex; align-items: center; justify-content: space-between; gap: 10px;
  border: 1px solid rgba(255,255,255,0.09); border-radius: 8px; padding: 10px 12px;
}
.flow-info { min-width: 0; }
.flow-name { display: flex; align-items: center; gap: 8px; font-weight: 600; font-size: 14px; }
.flow-desc { font-size: 12px; color: rgba(255,255,255,0.6); margin-top: 2px; }
.flow-meta { font-size: 12px; color: rgba(255,255,255,0.4); margin-top: 2px; }
.flow-actions { display: flex; gap: 6px; flex-shrink: 0; }
.actions { display: flex; gap: 10px; justify-content: flex-end; margin-top: 12px; }
.security-note {
  margin-top: 14px; padding: 8px 10px; border-radius: 6px;
  background: rgba(240,168,48,0.08); border: 1px solid rgba(240,168,48,0.3);
  font-size: 12px; color: rgba(255,255,255,0.65); line-height: 1.6;
}
</style>
