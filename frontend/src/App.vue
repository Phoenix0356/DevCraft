<!--
  App.vue —— 根组件。
  Vue 单文件组件（SFC）三段式结构，类比 Java 的一个类文件：
    <script setup> = 类的字段 + 方法（本组件无业务逻辑，只做全局配置）
    <template>     = 视图模板（HTML + v-xxx 指令）
    <style>        = 样式（scoped 表示只作用于本组件，本文件无样式）

  本组件职责：提供 Naive UI 的暗色主题与全局 Provider。
  Naive UI 的很多能力（消息提示 useMessage、对话框 useDialog）
  依赖"祖先组件提供上下文"，所以要在根部包一层 Provider——
  类比 Spring 里把 Bean 注册进 ApplicationContext，子组件才能注入使用。
-->
<script setup>
// script setup 是 Vue 3 的组合式 API 语法糖：
// 顶层变量/函数自动成为组件的"成员"，模板里可直接引用（无需 return）。

// darkTheme：Naive UI 内置暗色主题对象；zhCN/dateZhCN：中文文案与日期本地化
import {darkTheme, zhCN, dateZhCN} from 'naive-ui'
// 真正的应用主体（聊天界面），本组件只是它的外壳
import ChatApp from './components/ChatApp.vue'
</script>

<template>
  <!-- n-config-provider：主题/语言全局配置。:theme="..." 中的冒号是 v-bind: 的缩写，
       表示"绑定 JS 表达式"而非字符串（类比 Thymeleaf 的 ${}）。 -->
  <n-config-provider :theme="darkTheme" :locale="zhCN" :date-locale="dateZhCN">
    <!-- n-message-provider：让后代组件能用 useMessage() 弹顶部轻提示 -->
    <n-message-provider>
      <!-- n-dialog-provider：让后代组件能用 useDialog() 弹确认框 -->
      <n-dialog-provider>
        <ChatApp/>
      </n-dialog-provider>
    </n-message-provider>
  </n-config-provider>
</template>
