<!--
  SettingsModal.vue —— 设置弹窗（n-tabs 双标签页）：
  ①「通用配置」= LLM 与 Docker 连接配置（原有全部内容原样迁入，保存/测试逻辑不动）
     - IP 留空 = 本机 daemon；填了 IP = SSH 远程执行模式
     - "测试 SSH" 直接用输入框当前值测试（未保存也能测）；"保存" 才持久化
  ②「数据库配置」= PostgreSQL 连接与关注范围（独立表单 + 独立保存按钮）
     - 走独立绑定 GetPgSettings / SavePgSettings / TestPg：
       两个 tab 各自保存，后端字段互不覆盖（这是 PG 设置不并入 SaveSettings 的原因）
     - 密码沿用"已设置则 placeholder 提示、留空不修改"交互（与 API Key 同款语义）
     - 关注数据库 / 关注 Schema 用 n-dynamic-tags 多值输入（回车或失焦生成标签）；
       Agent 的 PG 查询技能只能访问白名单内的库和 schema（后端强制）
  技能管理已迁出为主区视图（SkillManager.vue，侧边栏"技能"按钮切换）。
  父子组件通信：props = 父→子入参；emit = 子→父事件；
  v-model:show 就是两者的组合（父传 :show，子 emit('update:show') 请求关闭）。
-->
<script setup>
import {ref, watch} from 'vue'
import {useMessage} from 'naive-ui'
import {
  GetPgSettings, GetSettings, SavePgSettings, SaveSettings,
  TestLLM, TestPg, TestSSH
} from '../../bindings/DevCraft/app.js'

// ---------------- props / emits 声明 ----------------
// defineProps/defineEmits 是编译器宏（无需 import）：声明本组件的入参与可发事件。
const props = defineProps({show: Boolean}) // 父组件传入的显隐状态
const emit = defineEmits(['update:show'])  // 可发出的事件名列表

const message = useMessage()

// ---------------- 通用配置表单（tab ①，逻辑与迁入前完全一致） ----------------
// form：所有输入框双向绑定的数据对象（端口默认 22）
const form = ref({
  baseUrl: '', apiKey: '', model: '',
  dockerIp: '', dockerPort: '22', dockerUser: '', sshPassword: ''
})
const apiKeySet = ref(false)      // 后端是否已存有 API Key（用于占位提示）
const sshPasswordSet = ref(false) // 后端是否已存有 SSH 密码
const testing = ref('')           // 正在测试哪项：'' | 'llm' | 'ssh'（控制按钮 loading）

// ---------------- 数据库配置表单（tab ②，PG 专用） ----------------
const pgForm = ref({
  host: '', port: 5432, user: '', password: '',
  databases: [], schemas: []
})
const pgPasswordSet = ref(false) // 后端是否已存有 PG 密码（用于占位提示）
const pgTesting = ref(false)     // "测试连接"按钮 loading

// watch 监听 props.show：弹窗每次打开时从后端拉取两个 tab 的最新设置
watch(() => props.show, async (v) => {
  if (!v) return
  const s = await GetSettings()
  form.value = {
    baseUrl: s.baseUrl || '',
    apiKey: '',                    // 密码类字段永不回显，留空=不修改
    model: s.model || '',
    dockerIp: s.dockerIp || '',
    dockerPort: s.dockerPort || '22',
    dockerUser: s.dockerUser || '',
    sshPassword: ''
  }
  apiKeySet.value = s.apiKeySet
  sshPasswordSet.value = s.sshPasswordSet
  // PG 设置走独立绑定拉取（密码同样永不回显）
  const p = await GetPgSettings()
  pgForm.value = {
    host: p.host || '',
    port: p.port || 5432,          // 未配置过端口时用 PG 默认端口
    user: p.user || '',
    password: '',                  // 留空=不修改（后端同款语义）
    databases: p.databases || [],
    schemas: p.schemas || []
  }
  pgPasswordSet.value = p.passwordSet
})

/** 当前表单的完整载荷（保存与静默保存共用） */
function formPayload() {
  return {
    baseUrl: form.value.baseUrl,
    apiKey: form.value.apiKey,
    model: form.value.model,
    dockerIp: form.value.dockerIp,
    dockerPort: form.value.dockerPort,
    dockerUser: form.value.dockerUser,
    sshPassword: form.value.sshPassword
  }
}

/** 保存设置（apiKey/sshPassword 留空则后端保持原值） */
async function save() {
  await SaveSettings(formPayload())
  if (form.value.apiKey) apiKeySet.value = true
  if (form.value.sshPassword) sshPasswordSet.value = true
  form.value.apiKey = ''      // 保存后清空密码框，避免明文停留
  form.value.sshPassword = ''
  message.success('设置已保存')
}

/** 测试 LLM：先静默保存当前表单（确保用最新配置），再发 ping */
async function testLLM() {
  testing.value = 'llm'
  try {
    await SaveSettings(formPayload())
    await TestLLM()
    message.success('LLM 连接正常')
  } catch (err) {
    message.error(String(err)) // Go 端的错误文字直接展示
  } finally {
    testing.value = ''
  }
}

/** 测试 SSH：直接用输入框当前值（不依赖已保存配置）。
 *  IP 留空则测本机 daemon。 */
async function testSSH() {
  testing.value = 'ssh'
  try {
    await TestSSH(form.value.dockerIp, form.value.dockerPort, form.value.dockerUser, form.value.sshPassword)
    message.success(form.value.dockerIp ? 'SSH 连接正常' : '本机 Docker 连接正常')
  } catch (err) {
    message.error(String(err))
  } finally {
    testing.value = ''
  }
}

/** PG 表单载荷（对照 formPayload；databases/schemas 是字符串数组直接透传） */
function pgPayload() {
  return {
    host: pgForm.value.host,
    port: pgForm.value.port || 5432,
    user: pgForm.value.user,
    password: pgForm.value.password,
    databases: pgForm.value.databases,
    schemas: pgForm.value.schemas
  }
}

/** 保存数据库配置（密码留空则后端保持原值——与通用配置的 apiKey 同款语义） */
async function savePg() {
  try {
    await SavePgSettings(pgPayload())
    if (pgForm.value.password) pgPasswordSet.value = true
    pgForm.value.password = ''  // 保存后清空密码框，避免明文停留
    message.success('数据库配置已保存')
  } catch (err) {
    message.error(String(err))
  }
}

/** 测试 PG 连接：直接用输入框当前值（未保存也能测，对照"测试 SSH"）；
 *  密码留空时后端回退到已保存的密码。 */
async function testPg() {
  pgTesting.value = true
  try {
    await TestPg(pgForm.value.host, pgForm.value.port || 5432, pgForm.value.user, pgForm.value.password)
    message.success('PostgreSQL 连接正常')
  } catch (err) {
    message.error(String(err)) // Go 端的中文错误文字直接展示
  } finally {
    pgTesting.value = false
  }
}
</script>

<template>
  <!-- n-modal：模态弹窗。preset="card" = 卡片样式；
       @update:show 把子组件的关闭请求转发给父组件（完成 v-model 闭环） -->
  <n-modal
    :show="props.show"
    preset="card"
    title="设置"
    style="width: 760px"
    @update:show="emit('update:show', $event)"
  >
    <!-- n-tabs：双标签页。两个 tab 的表单与保存动作完全独立（后端也是独立绑定），
         切换标签不会丢失另一侧未保存的输入（组件保持挂载状态） -->
    <n-tabs type="line" animated>
      <n-tab-pane name="general" tab="通用配置">
        <n-form label-placement="left" label-width="110">
          <n-form-item label="API Base URL">
            <!-- v-model:value 双向绑定表单字段 -->
            <n-input v-model:value="form.baseUrl" placeholder="如 https://api.deepseek.com/v1 或 https://dashscope.aliyuncs.com/compatible-mode/v1"/>
          </n-form-item>
          <n-form-item label="API Key">
            <!-- type="password" 密码框；show-password-on="click" = 点眼睛图标可临时明文 -->
            <n-input
              v-model:value="form.apiKey" type="password" show-password-on="click"
              :placeholder="apiKeySet ? '已保存（留空保持不变）' : '填写后加密存储'"
            />
          </n-form-item>
          <n-form-item label="默认模型">
            <n-input v-model:value="form.model" placeholder="如 deepseek-chat / qwen-plus（建议使用非推理模型）"/>
          </n-form-item>

          <!-- Docker 连接区（表单化）：IP 留空 = 本机；填写则 SSH 远程执行。
               部署流程选择 SSH 目标时，复用的就是这套连接配置。 -->
          <n-form-item label="Docker IP">
            <n-input v-model:value="form.dockerIp" placeholder="留空=本机；填写远程机器 IP 走 SSH（部署流程的 SSH 目标也用它）"/>
          </n-form-item>
          <n-form-item label="SSH 端口">
            <n-input v-model:value="form.dockerPort" placeholder="默认 22"/>
          </n-form-item>
          <n-form-item label="SSH 用户名">
            <n-input v-model:value="form.dockerUser" placeholder="如 root / ubuntu"/>
          </n-form-item>
          <n-form-item label="SSH 密码">
            <n-input
              v-model:value="form.sshPassword" type="password" show-password-on="click"
              :placeholder="sshPasswordSet ? '已保存（留空保持不变）' : '留空则尝试本机免密私钥'"
            />
          </n-form-item>
        </n-form>
        <div class="actions">
          <n-button :loading="testing === 'llm'" @click="testLLM">测试 LLM</n-button>
          <!-- 直接用输入框当前值测试，未保存也可以测 -->
          <n-button :loading="testing === 'ssh'" @click="testSSH">测试 SSH</n-button>
          <n-button type="primary" @click="save">保存</n-button>
        </div>
      </n-tab-pane>

      <n-tab-pane name="database" tab="数据库配置">
        <n-form label-placement="left" label-width="110">
          <n-form-item label="主机">
            <n-input v-model:value="pgForm.host" placeholder="PostgreSQL 主机地址，如 192.168.1.10 或 pg.example.com"/>
          </n-form-item>
          <n-form-item label="端口">
            <!-- n-input-number 绑定数字（后端 port 是 int）；限幅 1-65535 -->
            <n-input-number v-model:value="pgForm.port" :min="1" :max="65535" placeholder="默认 5432" style="width: 100%"/>
          </n-form-item>
          <n-form-item label="用户名">
            <n-input v-model:value="pgForm.user" placeholder="如 postgres / readonly（建议使用只读账号）"/>
          </n-form-item>
          <n-form-item label="密码">
            <n-input
              v-model:value="pgForm.password" type="password" show-password-on="click"
              :placeholder="pgPasswordSet ? '已设置，留空不修改' : '填写后加密存储'"
            />
          </n-form-item>
          <n-form-item label="关注数据库">
            <!-- n-dynamic-tags：多值输入（回车/失焦生成标签，点 × 删除），值就是 string[] -->
            <n-dynamic-tags v-model:value="pgForm.databases"/>
          </n-form-item>
          <n-form-item label="关注 Schema">
            <n-dynamic-tags v-model:value="pgForm.schemas"/>
          </n-form-item>
        </n-form>
        <div class="pg-hint">
          Agent 只能查询「关注数据库 / 关注 Schema」白名单内的数据，且仅允许只读查询（SELECT/WITH）；
          白名单在后端强制执行，保存后立即生效。
        </div>
        <div class="actions">
          <!-- 直接用输入框当前值测试，未保存也可以测；密码留空则用已保存的密码 -->
          <n-button :loading="pgTesting" @click="testPg">测试连接</n-button>
          <n-button type="primary" @click="savePg">保存</n-button>
        </div>
      </n-tab-pane>
    </n-tabs>
  </n-modal>
</template>

<style scoped>
.actions { display: flex; gap: 10px; justify-content: flex-end; margin-top: 12px; }
.pg-hint { font-size: 12px; opacity: 0.65; line-height: 1.6; }
</style>
