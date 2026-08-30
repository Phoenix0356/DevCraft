# DevCraft（开发工坊）

面向研发团队的 AI 智能助手，通过自然语言对话完成开发、测试、调试、运维工作。当前为第一期 MVP：**运维域**。

支持两种运行形态（同一套代码，构建时区分）：

- **桌面应用**：`wails3 build` —— 原生窗口（WebView2）
- **Web 服务**：`task build:server` —— 无头 HTTP 服务（`-tags server` 构建），浏览器直接访问，可部署到服务器

## 功能（MVP）

- **容器状态查看** —— "查看所有容器"
- **容器性能监控** —— "看看 web-app 的 CPU 占用"
- **智能日志分析** —— "查一下 order-service 为什么报错"（自动提取错误堆栈，LLM 给出根因分析）
- **一键部署（用户自定义流程）** —— 在设置页"技能管理"的部署技能（`deploy_run_flow`）
  介绍弹窗内声明式定义部署流程（命令步骤 + 参数占位符），
  对话中说"部署 xx"触发；执行前聊天内弹出**审批卡片**（完整命令预览），人工批准才执行，
  逐步进度回报 + 执行历史落库。支持本机与 SSH 两种执行通道，参数替换自动引号转义防注入
- 多会话管理（SQLite 持久化）、多 Agent 架构（运维 Agent 预置，可扩展）
- 支持本机与远程 Docker Host（`tcp://` / `ssh://`）

## 快速开始

1. 运行 `task dev`（需先安装 [go-task](https://taskfile.dev)；构建走 Taskfile.yml）
2. 打开 **设置**：
   - API Base URL：OpenAI 兼容端点，如
     - DeepSeek：`https://api.deepseek.com/v1`
     - 通义千问：`https://dashscope.aliyuncs.com/compatible-mode/v1`
   - API Key（AES-GCM 加密存储）
   - 默认模型：建议使用非推理模型，如 `deepseek-chat` / `qwen-plus`
   - Docker Host（三种模式）：
     - 留空 = 本机 daemon
     - `tcp://host:2375` = 直连远程 daemon（需远端开启 TCP API，测试环境用）
     - `ssh://user@host[:port]` = SSH 远程执行模式：登录远端执行 docker CLI 命令，
       无需开端口。前置条件：远端装有 docker CLI；设置页填 SSH 密码，
       或本机已配 `~/.ssh` 免密私钥（密码优先）
3. 新建会话，直接对话即可；`@运维 <指令>` 可强制路由到运维 Agent

## 架构

```
frontend/ (Vue3 + Naive UI)       聊天界面 / 会话侧栏 / Agent 选择 / 设置
frontend/bindings/                wails3 generate bindings 生成的前端桩（勿手改）
app.go / main.go                  Wails 入口与绑定层（Wails v3，双模式同源代码）
Taskfile.yml                      构建任务（wails3 build 走 go-task）
deploy/                           systemd 部署模板
internal/appsvc/                  会话编排、路由、设置
internal/agent/                   Agent 定义（数据化）+ tool-calling Runner
internal/skill/                   Skill 接口 + 注册表（namespace 命名）
internal/skill/ops/               运维域内置技能（ops_*）
internal/skill/deploy/            一键部署技能（生成待审批单，批准后执行）
internal/shellx/                  shell 引号转义与占位符替换（防注入）
internal/dockerx/                 Docker SDK 封装（本机 + 远程）
internal/store/                   SQLite（会话/消息/Agent/设置/部署流程与历史）
internal/llm/                     LLMClient 接口 + OpenAI 兼容 adapter
internal/secrets/                 API Key 加密
internal/sanitize/                日志截断与敏感信息脱敏
```

核心设计：**Agent = system prompt + 挂载的 skill 子集 + 模型**（数据化定义，存 SQLite），
**Skill = 自描述能力单元**（元数据对齐 function-calling schema）。新增 Agent 只需配置，
新增 Skill 只需实现 `skill.Skill` 接口并注册。

通信链路：前端绑定桩 → HTTP `/wails/runtime`；聊天流式推送走 v3 Streams
（`JSONStream('chat')`，桌面模式进程内模拟、服务器模式真 WebSocket，多标签页按
sessionId 过滤互不干扰）。

## 部署（服务器模式）

服务器构建为 CGO-free 静态二进制，Linux 无头机直接运行，无需 GUI 依赖。

```bash
# 1. 本地交叉编译（产物：bin/DevCraft-server-linux）
task build:server:linux

# 2. 上传并安装（详细步骤见 deploy/devcraft.service 文件头注释）
scp bin/DevCraft-server-linux user@server:/opt/devcraft/
sudo cp deploy/devcraft.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now devcraft

# 3. 浏览器访问
#    http://<server-ip>:8080
```

环境变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `WAILS_SERVER_HOST` | `localhost` | 监听地址；对外服务设 `0.0.0.0` |
| `WAILS_SERVER_PORT` | `8080` | 监听端口 |
| `DEVCRAFT_DATA_DIR` | 用户配置目录 | 数据目录（SQLite + secret.key） |
| `DEVCRAFT_LOG` | `info` | 日志级别（`debug` 开调试） |

健康检查：`GET /health` 返回 `{"status":"ok"}`。

> ⚠️ **安全边界**：服务器模式没有登录鉴权，依赖网络隔离。请勿直接暴露公网，
> 用防火墙限制来源，或置于反向代理（nginx 等）之后，由代理负责 HTTPS 与访问控制。
> 多标签页/多人访问共享同一份数据（会话、设置），流式输出按会话过滤不会串台。
>
> ⚠️ **部署能力是高危写操作**：一键部署会在目标机器（本机或 SSH 主机）执行命令。
> 流程模板由你定义（风险自担）；参数值来自 LLM，替换时已做引号转义与可选正则校验，
> 且执行前有聊天内人工审批门禁。但"服务器模式 + 部署能力"意味着任何能访问页面的人
> 都能发起部署——服务器模式务必做好网络隔离，切勿暴露到不可信网络。

## 开发

```bash
task dev                 # 实时开发（vite 热重载 + 桌面窗口）
go test ./...            # 后端单元测试
task build               # 桌面应用（= wails3 build）
task build:server        # 服务器二进制（当前系统）
task build:server:linux  # 交叉编译 Linux amd64
wails3 generate bindings # Go 导出方法变更后重新生成前端桩
```
