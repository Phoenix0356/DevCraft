<!--
  SettingsModal.vue —— 设置弹窗（纯基础设置）：
  LLM 与 Docker 连接配置（表单化设计）
     - IP 留空 = 本机 daemon；填了 IP = SSH 远程执行模式
     - "测试 SSH" 直接用输入框当前值测试（未保存也能测）；"保存" 才持久化
  技能管理已迁出为独立弹窗（SkillManager.vue，侧边栏"技能"按钮入口）。
  父子组件通信：props = 父→子入参；emit = 子→父事件；
  v-model:show 就是两者的组合（父传 :show，子 emit('update:show') 请求关闭）。
-->
<script setup>
import {ref, watch} from 'vue'
import {useMessage} from 'naive-ui'
import {GetSettings, SaveSettings, TestLLM, TestSSH} from '../../bindings/DevCraft/app.js'

// ---------------- props / emits 声明 ----------------
// defineProps/defineEmits 是编译器宏（无需 import）：声明本组件的入参与可发事件。
const props = defineProps({show: Boolean}) // 父组件传入的显隐状态
const emit = defineEmits(['update:show'])  // 可发出的事件名列表

const message = useMessage()

// ---------------- 基础设置表单 ----------------
// form：所有输入框双向绑定的数据对象（端口默认 22）
const form = ref({
  baseUrl: '', apiKey: '', model: '',
  dockerIp: '', dockerPort: '22', dockerUser: '', sshPassword: ''
})
const apiKeySet = ref(false)      // 后端是否已存有 API Key（用于占位提示）
const sshPasswordSet = ref(false) // 后端是否已存有 SSH 密码
const testing = ref('')           // 正在测试哪项：'' | 'llm' | 'ssh'（控制按钮 loading）

// watch 监听 props.show：弹窗每次打开时从后端拉取最新设置
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
  </n-modal>
</template>

<style scoped>
.actions { display: flex; gap: 10px; justify-content: flex-end; margin-top: 12px; }
</style>
