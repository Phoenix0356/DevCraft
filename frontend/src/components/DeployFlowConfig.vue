<!--
  DeployFlowConfig.vue —— 部署流程配置区（从 SettingsModal 原"部署流程"标签页整体迁移）。
  一键部署的声明式流程管理：列表 + 编辑表单（新建/编辑共用）+ 删除 + 导入。
    流程 = 名称 + 描述 + 执行目标（本机/SSH）+ 参数声明（可选校验正则）
         + 有序命令步骤（每行一条命令，{{参数名}} 占位符）
    流程只是"定义"；真正执行必须由对话中的审批卡片人工批准。
  导入 = JSON 配置文件（选 .json 文件或粘贴文本）→ 归一化 → 逐条 SaveDeployFlow；
  同名流程覆盖更新（按 name 匹配已加载列表拿 id），免去表单里逐字段手敲长命令。
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

// ---------------- 导入（JSON 文件 / 粘贴文本，两种输入共用一套解析逻辑） ----------------

const showImport = ref(false)     // 导入弹窗开关
const importText = ref('')        // 待导入的 JSON 文本（选文件后也回填到这里，统一入口）
const importErrors = ref([])      // 最近一次导入的中文错误明细（弹窗内展示，便于修改重试）
const importFileInput = ref(null) // 隐藏的原生 <input type="file"> 的模板引用

function openImport() {
  importErrors.value = []
  showImport.value = true
}

/** 用按钮触发隐藏的原生文件选择框：input[type=file] 无法自定义外观，
    惯例是 display:none 藏起来，再由 n-button 的 click 里调它的 click() */
function pickImportFile() {
  importFileInput.value?.click()
}

/** 选中文件后用 FileReader 异步读成文本（≈ Java 的线程回调，onload 即"读取完成"回调） */
function onImportFilePicked(e) {
  const file = e.target.files?.[0]
  e.target.value = '' // 清空 input 值：否则重选同一文件不会再触发 change 事件
  if (!file) return
  const reader = new FileReader()
  reader.onload = () => { importText.value = String(reader.result || '') }
  reader.onerror = () => message.error('读取文件失败')
  reader.readAsText(file, 'utf-8') // 显式指定 UTF-8，避免中文描述乱码
}

/**
 * 归一化单个流程对象 → 与 saveFlow 相同口径的 payload（id 由 doImport 覆盖决定）。
 * 形状不合法直接 throw 中文错误；未知字段自然被忽略（只挑认识的字段组装）。
 */
function normalizeFlowItem(item, idx) {
  if (!item || typeof item !== 'object' || Array.isArray(item)) {
    throw new Error(`第 ${idx + 1} 条不是流程对象（应为 {name, description, target, params, steps}）`)
  }
  const name = typeof item.name === 'string' ? item.name.trim() : ''
  if (name === '') throw new Error(`第 ${idx + 1} 条流程缺少 name 字段`)

  // target 缺省 local；只认 local / ssh（与后端 validateDeployFlow 一致）
  const target = item.target == null ? 'local' : String(item.target)
  if (target !== 'local' && target !== 'ssh') {
    throw new Error(`流程「${name}」的 target 无效：${target}（可选值: local / ssh）`)
  }

  // steps 容错：字符串数组，或 \n 分隔的单个字符串（方便手写配置）；统一拆行、trim、丢空行
  let rawSteps = item.steps
  if (typeof rawSteps === 'string') rawSteps = rawSteps.split('\n')
  if (!Array.isArray(rawSteps)) rawSteps = []
  const steps = rawSteps.map(s => String(s).trim()).filter(s => s !== '')
  if (steps.length === 0) {
    throw new Error(`流程「${name}」至少需要一条命令步骤（steps 为空）`)
  }

  const params = (Array.isArray(item.params) ? item.params : [])
    .filter(p => p && typeof p === 'object')
    .map(p => ({
      name: String(p.name ?? '').trim(),
      desc: String(p.desc ?? '').trim(),
      pattern: String(p.pattern ?? '').trim()
    }))
    .filter(p => p.name !== '') // 与 saveFlow 相同：丢掉 name 为空的参数行

  return {id: 0, name, description: String(item.description ?? '').trim(), target, params, steps}
}

/** 解析导入文本 → payload 数组。先整体解析+形状校验，任何一条不合法就全部拒绝（不部分写入） */
function parseImportPayloads(text) {
  if (!text || !text.trim()) throw new Error('导入内容为空：请选择 .json 文件或粘贴 JSON 文本')
  let data
  try {
    data = JSON.parse(text)
  } catch (e) {
    throw new Error(`JSON 解析失败：${e.message}`)
  }
  // 单个流程对象自动包装成数组：两种形状共用下面同一套逐条处理
  const items = Array.isArray(data) ? data : [data]
  if (items.length === 0) throw new Error('导入内容为空数组：至少需要一个流程对象')
  return items.map((item, i) => normalizeFlowItem(item, i))
}

/** 执行导入：形状校验通过后逐条 SaveDeployFlow，单条失败不影响其余，最后汇总 */
async function doImport() {
  importErrors.value = []
  let payloads
  try {
    payloads = parseImportPayloads(importText.value)
  } catch (err) {
    importErrors.value = [String(err.message || err)] // 形状错误：整体拒绝，不写任何数据
    return
  }
  const errors = []
  let ok = 0
  for (const p of payloads) {
    // 重名 = 覆盖更新：按 name 在已加载列表里找现有流程，命中则带其 id（更新），否则 id=0（新建）
    const hit = flows.value.find(f => f.name === p.name)
    try {
      await SaveDeployFlow({...p, id: hit ? hit.id : 0})
      ok++
    } catch (err) {
      errors.push(`「${p.name}」：${err}`) // 后端 validateDeployFlow 的中文错误原样展示
    }
  }
  if (ok > 0) {
    showImport.value = false
    importText.value = ''
    await loadFlows()
    emit('changed')
    message.success(`导入完成：成功 ${ok} 个${errors.length ? `，失败 ${errors.length} 个` : ''}`)
    if (errors.length) message.error(errors.join('；'), {duration: 8000}) // 失败明细多停留一会
  } else {
    importErrors.value = errors // 全部失败：保留弹窗与输入内容，就地展示错误供修改重试
    message.error(`导入失败：${errors.length} 个流程均未写入`)
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
      <n-button v-if="!editing" size="small" type="primary" ghost @click="openImport">导入</n-button>
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

    <!-- 导入弹窗：选 .json 文件或直接粘贴文本，共用 importText → doImport 一条链路。
         v-if + :show="true" 的写法与 SkillManager 详情弹窗一致（关闭即销毁内容） -->
    <n-modal
      v-if="showImport"
      :show="true"
      preset="card"
      title="导入部署流程"
      :style="{width: '640px'}"
      :segmented="{content: true}"
      @update:show="showImport = false"
    >
      <div class="import-body">
        <!-- 文件选择行：原生 input[type=file] 藏起来（见样式），由按钮触发 click -->
        <div class="import-file-row">
          <n-button size="small" @click="pickImportFile">选择 .json 文件…</n-button>
          <input
            ref="importFileInput"
            type="file"
            accept=".json"
            class="import-file-input"
            @change="onImportFilePicked"
          />
          <span class="import-hint">选中后自动读入下方文本框；也可以直接粘贴 JSON</span>
        </div>

        <n-input
          v-model:value="importText"
          class="import-textarea"
          type="textarea"
          :autosize="{minRows: 8, maxRows: 16}"
          placeholder='粘贴流程 JSON：单个对象或对象数组，例如 {"name": "web-deploy", "steps": ["docker restart web"]}'
        />

        <pre class="import-format">字段：name（必填）、description、target（local / ssh，缺省 local）、params[]（{name, desc, pattern}）、steps[]（命令数组，也接受 \n 分隔的单个字符串）。同名流程将被覆盖更新，未知字段忽略。示例：
{
  "name": "web-deploy",
  "description": "拉取镜像并重启",
  "target": "local",
  "params": [],
  "steps": ["docker pull registry/web:latest", "docker restart web"]
}</pre>

        <!-- 错误明细：形状校验失败 / 全部条目保存失败时展示，弹窗内容保留供修改重试 -->
        <div v-if="importErrors.length" class="import-errors">
          <div v-for="(e, i) in importErrors" :key="i">{{ e }}</div>
        </div>

        <div class="actions">
          <n-button @click="showImport = false">取消</n-button>
          <n-button type="primary" @click="doImport">导入</n-button>
        </div>
      </div>
    </n-modal>
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

/* ---- 导入弹窗 ---- */
.import-file-row { display: flex; align-items: center; gap: 10px; margin-bottom: 10px; }
/* 原生文件选择框无法自定义外观：藏起来，由"选择 .json 文件"按钮代为触发 click() */
.import-file-input { display: none; }
.import-hint { font-size: 12px; color: rgba(255,255,255,0.45); }
/* :deep() 穿透 scoped：给 n-input 内部真实的 textarea 换等宽字体，长命令更易读 */
.import-textarea :deep(textarea) { font-family: Consolas, Menlo, monospace; font-size: 12px; }
.import-format {
  margin: 10px 0 0; padding: 8px 10px; border-radius: 6px;
  background: rgba(255,255,255,0.04); border: 1px solid rgba(255,255,255,0.09);
  font-family: Consolas, Menlo, monospace; font-size: 12px;
  color: rgba(255,255,255,0.55); line-height: 1.6;
  white-space: pre-wrap; word-break: break-all; /* 示例 JSON 保持换行，长行折行不撑破弹窗 */
}
.import-errors {
  margin-top: 10px; padding: 8px 10px; border-radius: 6px;
  background: rgba(224,108,117,0.08); border: 1px solid rgba(224,108,117,0.35);
  font-size: 12px; color: rgba(255,255,255,0.75); line-height: 1.7;
}
</style>
