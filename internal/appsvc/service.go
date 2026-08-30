// Package appsvc 是编排层（≈ Java 的 Service 层），Wails 绑定层的唯一依赖。
// 职责：设置读写（含密钥加密）、连接测试、会话管理、消息路由、驱动 Agent 回合。
package appsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime/debug" // debug.Stack()：panic/错误时打印堆栈
	"strings"
	"sync"
	"time"

	"github.com/google/uuid" // UUID 生成库（会话 id）

	"DevCraft/internal/agent"
	"DevCraft/internal/dockerx"
	"DevCraft/internal/llm"
	"DevCraft/internal/secrets"
	"DevCraft/internal/skill"
	"DevCraft/internal/skill/deploy" // 会话 id 经 ctx 注入技能（审批单需要会话绑定）
	"DevCraft/internal/store"
)

// settings 表的键名常量（小写开头 = 包内私有）。
const (
	settingLLMBaseURL  = "llm.base_url"
	settingLLMAPIKey   = "llm.api_key" // 存的是密文，永不明文落盘
	settingLLMModel    = "llm.model"
	settingDockerIP    = "docker.ip"    // 留空=本机 daemon；填写则走 SSH
	settingDockerPort  = "docker.port"  // SSH 端口，默认 22
	settingDockerUser  = "docker.user"  // SSH 用户名
	settingDockerHost  = "docker.host"  // 遗留键：仅作旧 ssh:// 配置的迁移来源
	settingSSHPassword = "ssh.password" // SSH 登录密码，密文落盘

	historyLimit = 20 // 发给 LLM 的历史消息条数上限（控制 token 成本）
)

// 推送给前端的事件名（前端用 EventsOn 订阅，见 ChatApp.vue）。
const (
	EventDelta = "chat:delta" // 流式文本增量
	EventTool  = "chat:tool"  // 技能开始/结束
	EventDone  = "chat:done"  // 回合结束（成功带全文，失败带 error）
)

// Emit 是"事件发射器"的函数类型（依赖倒置）：
// 本包只声明"要发事件"，具体怎么发（Wails runtime）由 app.go 注入。
type Emit func(event string, payload map[string]any)

// turnTimeout 单个回合的总上限（兜底：慢吐流/失控工具循环等一切场景）。
// 包级变量（而非常量）是为了让单元测试能临时调小；生产值 3 分钟。
// 与 llm 包的流空闲心跳（90 秒）构成双层超时：空闲心跳抓半开连接，
// 总上限抓"一直有心跳但永远不结束"的极端慢性流。
var turnTimeout = 3 * time.Minute

// turnEntry 进行中回合在取消注册表里的条目。
// 存指针是为了回合结束时能按"指针身份"精确注销，
// 防止误删同一会话后续新回合登记的条目。
type turnEntry struct {
	cancel context.CancelFunc
}

// Service 编排服务本体（≈ @Service Bean），所有依赖经构造函数注入。
type Service struct {
	store  *store.Store                            // 持久层
	agents *agent.Registry                         // Agent 注册表
	skills *skill.Registry                         // 技能注册表
	docker *dockerx.Manager                        // Docker 连接管理
	box    *secrets.Box                            // API Key 加解密器
	emit   Emit                                    // 事件发射器（注入的 Wails 实现）
	newLLM func(apiKey, baseURL string) llm.Client // LLM 客户端工厂（测试可替换成假实现）
	// 部署执行器工厂（按目标组装本机/SSH 通道）；测试可替换成假执行器。
	// 默认指向 deployStepRunner（见 deploy.go）。
	deployRunnerFor func(target string) (StepRunner, string, error)

	// 会话级"当前回合"取消注册表（≈ 按会话隔离的 Future 取消句柄池）：
	// CancelTurn 查表拿到 cancel 函数即可中断进行中的回合。
	// 部署执行（approve 后的异步执行）也登记在这里，"停止"按钮同样能取消它。
	turnsMu sync.Mutex
	turns   map[string]*turnEntry

	// 待审批部署单注册表（内存态，带过期；见 deploy.go 的状态机说明）。
	// 审批门禁：执行路径只能通过 takeApproval 从这里取单，未批准绝不执行。
	approvalsMu sync.Mutex
	approvals   map[string]*Approval
}

// New 构造 Service。newLLM 默认指向真实的 OpenAI 适配器。
func New(st *store.Store, agents *agent.Registry, skills *skill.Registry, docker *dockerx.Manager, box *secrets.Box, emit Emit) *Service {
	s := &Service{
		store:     st,
		agents:    agents,
		skills:    skills,
		docker:    docker,
		box:       box,
		emit:      emit,
		newLLM:    func(apiKey, baseURL string) llm.Client { return llm.NewOpenAIClient(apiKey, baseURL) },
		turns:     map[string]*turnEntry{},
		approvals: map[string]*Approval{},
	}
	s.deployRunnerFor = s.deployStepRunner // 自引用默认值；测试可整体替换
	return s
}

// ==================== 设置 ====================

// Settings 前端提交的设置表单（apiKey/sshPassword 留空表示不修改）。
type Settings struct {
	BaseURL     string `json:"baseUrl"`
	APIKey      string `json:"apiKey"` // 只写字段：UI → 后端；后端永不回传
	Model       string `json:"model"`
	DockerIP    string `json:"dockerIp"`    // 留空=本机；填写则走 SSH
	DockerPort  string `json:"dockerPort"`  // 默认 22
	DockerUser  string `json:"dockerUser"`  // SSH 用户名
	SSHPassword string `json:"sshPassword"` // 只写字段：SSH 登录密码，留空=不修改
}

// SettingsView 返回给前端的设置视图（不含明文 Key，只告知"是否已设置"）。
type SettingsView struct {
	BaseURL        string `json:"baseUrl"`
	APIKeySet      bool   `json:"apiKeySet"`
	Model          string `json:"model"`
	DockerIP       string `json:"dockerIp"`
	DockerPort     string `json:"dockerPort"`
	DockerUser     string `json:"dockerUser"`
	SSHPasswordSet bool   `json:"sshPasswordSet"`
}

// GetSettings 读全部设置。
func (s *Service) GetSettings() (SettingsView, error) {
	view := SettingsView{}
	// Go 没有 Optional，用 (值, 是否存在, 错误) 三返回值表达
	if v, ok, err := s.setting(settingLLMBaseURL); err != nil {
		return view, err
	} else if ok {
		view.BaseURL = v
	}
	if v, ok, err := s.setting(settingLLMModel); err != nil {
		return view, err
	} else if ok {
		view.Model = v
	}
	if v, ok, err := s.setting(settingDockerIP); err != nil {
		return view, err
	} else if ok {
		view.DockerIP = v
	}
	if v, ok, err := s.setting(settingDockerPort); err != nil {
		return view, err
	} else if ok {
		view.DockerPort = v
	}
	if v, ok, err := s.setting(settingDockerUser); err != nil {
		return view, err
	} else if ok {
		view.DockerUser = v
	}
	// 兼容迁移：旧版配置只有 docker.host（ssh://user@ip:port），
	// 首次读取时自动拆解回填到新的表单字段并落库，之后以新字段为准。
	if view.DockerIP == "" {
		if legacy, ok, err := s.setting(settingDockerHost); err == nil && ok && strings.HasPrefix(legacy, "ssh://") {
			if u, perr := url.Parse(legacy); perr == nil {
				view.DockerIP = u.Hostname()
				if p := u.Port(); p != "" {
					view.DockerPort = p
				}
				if u.User != nil {
					view.DockerUser = u.User.Username()
				}
				_ = s.store.SetSetting(settingDockerIP, view.DockerIP)
				_ = s.store.SetSetting(settingDockerPort, view.DockerPort)
				_ = s.store.SetSetting(settingDockerUser, view.DockerUser)
			}
		}
	}
	if enc, ok, err := s.setting(settingLLMAPIKey); err != nil {
		return view, err
	} else if ok && enc != "" {
		view.APIKeySet = true // 只暴露"已设置"这个事实
	}
	if enc, ok, err := s.setting(settingSSHPassword); err != nil {
		return view, err
	} else if ok && enc != "" {
		view.SSHPasswordSet = true
	}
	return view, nil
}

// SaveSettings 保存设置；apiKey 非空时先 AES-GCM 加密再落盘。
func (s *Service) SaveSettings(in Settings) error {
	if err := s.store.SetSetting(settingLLMBaseURL, in.BaseURL); err != nil {
		return err
	}
	if err := s.store.SetSetting(settingLLMModel, in.Model); err != nil {
		return err
	}
	if err := s.store.SetSetting(settingDockerIP, in.DockerIP); err != nil {
		return err
	}
	if err := s.store.SetSetting(settingDockerPort, in.DockerPort); err != nil {
		return err
	}
	if err := s.store.SetSetting(settingDockerUser, in.DockerUser); err != nil {
		return err
	}
	if in.APIKey != "" {
		enc, err := s.box.Encrypt(in.APIKey)
		if err != nil {
			return fmt.Errorf("加密 API Key 失败: %w", err)
		}
		if err := s.store.SetSetting(settingLLMAPIKey, enc); err != nil {
			return err
		}
	}
	if in.SSHPassword != "" {
		enc, err := s.box.Encrypt(in.SSHPassword)
		if err != nil {
			return fmt.Errorf("加密 SSH 密码失败: %w", err)
		}
		if err := s.store.SetSetting(settingSSHPassword, enc); err != nil {
			return err
		}
	}
	slog.Info("设置已保存", "model", in.Model, "dockerIp", in.DockerIP, "dockerUser", in.DockerUser)
	return nil
}

// setting 是读设置的包内小助手（透传三返回值）。
func (s *Service) setting(key string) (string, bool, error) {
	return s.store.GetSetting(key)
}

// llmConfig 组装 LLM 调用三要素（解密后的 Key、Base URL、模型）。
// 未配置 Key 时返回面向用户的中文错误（会直接显示在界面上）。
func (s *Service) llmConfig() (apiKey, baseURL, model string, err error) {
	// Go 支持命名返回值：签名里声明 apiKey 等，函数内可直接赋值
	enc, ok, err := s.setting(settingLLMAPIKey)
	if err != nil {
		return "", "", "", err
	}
	if !ok || enc == "" {
		return "", "", "", fmt.Errorf("尚未配置 LLM API Key，请先在设置中填写")
	}
	apiKey, err = s.box.Decrypt(enc)
	if err != nil {
		return "", "", "", fmt.Errorf("读取 API Key 失败，请重新保存: %w", err)
	}
	baseURL, _, _ = s.setting(settingLLMBaseURL) // 这两项可缺省，忽略 ok/err
	model, _, _ = s.setting(settingLLMModel)
	return apiKey, baseURL, model, nil
}

// dockerSSHHost 由表单字段拼出 ssh:// 连接串；IP 为空返回空串（=本机）。
func (s *Service) dockerSSHHost() string {
	ip, _, _ := s.setting(settingDockerIP)
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	user, _, _ := s.setting(settingDockerUser)
	port, _, _ := s.setting(settingDockerPort)
	if port == "" {
		port = "22"
	}
	return fmt.Sprintf("ssh://%s@%s:%s", user, ip, port)
}

// DockerEndpoint 返回当前 Docker 连接端点（host + SSH 密码），
// 作为回调传给运维技能——技能执行时实时读取，改设置立即生效。
func (s *Service) DockerEndpoint() (host string, sshPassword string) {
	return s.dockerSSHHost(), s.sshPassword()
}

// sshPassword 解密 SSH 密码；未设置或解密失败返回空串（走免密私钥）。
func (s *Service) sshPassword() string {
	enc, ok, err := s.setting(settingSSHPassword)
	if err != nil || !ok || enc == "" {
		return ""
	}
	pass, err := s.box.Decrypt(enc)
	if err != nil {
		return ""
	}
	return pass
}

// ==================== 连接测试 ====================

// TestSSH 用"当前表单值"测试连接（设置页"测试 SSH"按钮）。
// 关键点：参数直接来自输入框而非已保存配置，未保存也能测。
// IP 为空时测本机 daemon。
func (s *Service) TestSSH(ctx context.Context, ip, port, user, password string) error {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		exe, err := s.docker.Executor("", "")
		if err != nil {
			return err
		}
		if err := exe.Ping(ctx); err != nil {
			return fmt.Errorf("本机 Docker 连接失败: %w", err)
		}
		return nil
	}
	if strings.TrimSpace(user) == "" {
		return fmt.Errorf("请填写 SSH 用户名")
	}
	if port == "" {
		port = "22"
	}
	host := fmt.Sprintf("ssh://%s@%s:%s", user, ip, port)
	exe, err := s.docker.Executor(host, password)
	if err != nil {
		slog.Error("SSH 执行器创建失败", "host", host, "err", err, "stack", string(debug.Stack()))
		return err
	}
	if err := exe.Ping(ctx); err != nil {
		slog.Error("测试 SSH 失败", "host", host, "err", err)
		return fmt.Errorf("SSH 连接失败（%s）: %w", host, err)
	}
	slog.Info("测试 SSH 成功", "host", host)
	return nil
}

// TestLLM 发一条 "ping" 验证 LLM 配置是否可用。
func (s *Service) TestLLM(ctx context.Context) error {
	apiKey, baseURL, model, err := s.llmConfig()
	if err != nil {
		return err
	}
	if model == "" {
		return fmt.Errorf("请先在设置中填写模型名称")
	}
	cli := s.newLLM(apiKey, baseURL)
	resp, err := cli.ChatStream(ctx, model, []llm.Message{{Role: llm.RoleUser, Content: "ping"}}, nil, nil)
	if err != nil {
		return fmt.Errorf("LLM 连接失败: %w", err)
	}
	_ = resp // 只验证连通性，不关心回复内容；_ = 显式忽略
	return nil
}

// ==================== 会话 ====================

// ListSessions 会话列表（按活跃时间倒序）。
func (s *Service) ListSessions() ([]store.Session, error) { return s.store.ListSessions() }

// NewSession 建会话，默认绑定内置运维 Agent。
func (s *Service) NewSession() (store.Session, error) {
	sess := store.Session{ID: uuid.NewString(), Title: "新会话", AgentID: agent.BuiltinOpsAgentID}
	if err := s.store.CreateSession(sess); err != nil {
		return sess, err
	}
	return sess, nil
}

// DeleteSession 删除会话及其消息。
func (s *Service) DeleteSession(id string) error { return s.store.DeleteSession(id) }

// SetSessionAgent 给会话切换 Agent（前端 Agent 下拉框）。
func (s *Service) SetSessionAgent(sessionID, agentID string) error {
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		return err
	}
	if _, err := s.agents.Get(agentID); err != nil { // 校验目标 Agent 存在
		return err
	}
	sess.AgentID = agentID
	return s.store.UpdateSession(sess)
}

// Messages 会话全部历史（前端打开会话时加载）。
func (s *Service) Messages(sessionID string) ([]store.Message, error) {
	return s.store.Messages(sessionID, 0) // 0 = 不限条数
}

// ==================== Agents ====================

// ListAgents 所有 Agent 定义（前端 Agent 选择器）。
func (s *Service) ListAgents() ([]store.AgentRow, error) { return s.agents.List() }

// ==================== 聊天主流程 ====================

// SendMessage 执行一个完整用户回合（同步阻塞直到出最终回答；
// 过程中的流式增量通过事件推送）。这是整个应用最核心的方法。
func (s *Service) SendMessage(ctx context.Context, sessionID, text string) error {
	// panic 兜底（≈ 全局 ExceptionHandler）：任何未捕获崩溃都记录完整堆栈
	// 并通知前端，而不是让回合静默卡死
	defer func() {
		if r := recover(); r != nil {
			slog.Error("SendMessage panic", "session", sessionID, "panic", r, "stack", string(debug.Stack()))
			s.emitDone(sessionID, "", fmt.Sprintf("内部错误: %v", r))
		}
	}()

	// 回合总上限（第二层超时）：整个回合——包括所有 LLM 轮次与技能执行——
	// 最长只能跑 turnTimeout。第一层（90 秒流空闲心跳）在 llm 包内部。
	// 同时把 cancel 登记进会话级注册表，供 CancelTurn（前端"停止"按钮）跨层取消；
	// 回合结束（无论正常/异常/panic）按"指针身份"注销，避免误删新回合的条目。
	turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	entry := &turnEntry{cancel: cancel}
	s.turnsMu.Lock()
	s.turns[sessionID] = entry
	s.turnsMu.Unlock()
	defer func() {
		s.turnsMu.Lock()
		if s.turns[sessionID] == entry {
			delete(s.turns, sessionID)
		}
		s.turnsMu.Unlock()
		cancel()
	}()

	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("消息不能为空")
	}
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		return err
	}
	slog.Info("收到用户消息", "session", sessionID, "agent", sess.AgentID, "len", len([]rune(text)))

	// --- 会话级路由（两级）---
	// ① @前缀强制路由："@运维 查看所有容器" → 切到名字/id 匹配的 Agent
	if prefix, rest, ok := parseAgentPrefix(text); ok {
		if a, err := s.matchAgent(prefix); err == nil {
			sess.AgentID = a.ID
			_ = s.store.UpdateSession(sess) // 顺带把会话绑定切过去
			text = rest                     // 去掉前缀，只把真正的问题发给模型
		}
		// 匹配失败时静默忽略前缀，走默认 Agent（容错）
	}
	// ② 无前缀则使用会话绑定的 Agent（sess.AgentID）

	// 用户消息落库
	if err := s.store.AppendMessage(store.Message{SessionID: sessionID, Role: "user", Content: text}); err != nil {
		return err
	}
	// 首条消息自动成为会话标题（≈ 聊天软件惯例）
	if sess.Title == "新会话" || sess.Title == "" {
		sess.Title = truncate(text, 30)
		_ = s.store.UpdateSession(sess)
	}

	// 读 LLM 配置（失败=未配置 Key，发 done 事件让前端提示）
	apiKey, baseURL, defaultModel, err := s.llmConfig()
	if err != nil {
		slog.Error("LLM 配置缺失", "session", sessionID, "err", err)
		s.emitDone(sessionID, "", err.Error())
		return err
	}

	// 取 Agent 定义（含 system prompt 与装配的技能）
	a, err := s.agents.Get(sess.AgentID)
	if err != nil {
		slog.Error("Agent 加载失败", "session", sessionID, "agent", sess.AgentID, "err", err)
		s.emitDone(sessionID, "", err.Error())
		return err
	}

	// 组装发给模型的历史（最近 20 条 user/assistant 消息）
	history, err := s.buildHistory(sessionID)
	if err != nil {
		return err
	}

	// 每回合新建 Runner（LLM 客户端按当前配置现造，改设置立即生效）。
	// ctx 注入会话 id：部署技能靠它把审批单绑定到当前会话（Skill 接口无会话参数）。
	runner := agent.NewRunner(s.newLLM(apiKey, baseURL), s.skills, defaultModel)
	answer, runErr := runner.Run(deploy.WithSessionID(turnCtx, sessionID), a, history, agent.Events{
		// 三个回调都转成 Wails 事件推给前端（载荷带 sessionId 供前端过滤）
		OnDelta: func(delta string) {
			s.emit(EventDelta, map[string]any{"sessionId": sessionID, "content": delta})
		},
		OnToolStart: func(name, args string) {
			s.emit(EventTool, map[string]any{"sessionId": sessionID, "skill": name, "status": "start"})
		},
		OnToolResult: func(name, result string, failed bool) {
			s.emit(EventTool, map[string]any{"sessionId": sessionID, "skill": name, "status": "done", "failed": failed})
		},
	})
	if runErr != nil {
		slog.Error("Agent 回合失败", "session", sessionID, "err", runErr)
		// done 帧带面向用户的中文文案（区分超时/停止/一般错误）；
		// RPC 原样返回底层错误，调用方可用 errors.Is 精确分类
		s.emitDone(sessionID, "", turnErrText(runErr))
		return runErr
	}
	// 最终回答落库（工具轮次不入库）并通知前端收尾
	if err := s.store.AppendMessage(store.Message{SessionID: sessionID, Role: "assistant", Content: answer}); err != nil {
		return err
	}
	slog.Info("Agent 回合结束", "session", sessionID, "answerLen", len([]rune(answer)))
	s.emitDone(sessionID, answer, "")
	return nil
}

// buildHistory 从库里取最近 historyLimit 条消息，转成 LLM 消息序列。
func (s *Service) buildHistory(sessionID string) ([]llm.Message, error) {
	msgs, err := s.store.Messages(sessionID, historyLimit)
	if err != nil {
		return nil, err
	}
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue // 防御性过滤：历史里只会有这两种角色
		}
		out = append(out, llm.Message{Role: m.Role, Content: m.Content})
	}
	return out, nil
}

// matchAgent 按 id 或名称（忽略大小写）找 Agent。
func (s *Service) matchAgent(prefix string) (store.AgentRow, error) {
	agents, err := s.agents.List()
	if err != nil {
		return store.AgentRow{}, err
	}
	for _, a := range agents {
		if strings.EqualFold(a.ID, prefix) || strings.EqualFold(a.Name, prefix) {
			return a, nil
		}
	}
	return store.AgentRow{}, fmt.Errorf("未找到 Agent: %s", prefix)
}

// parseAgentPrefix 解析 "@xxx 正文" 形式的强制路由前缀。
// 返回 (前缀, 剩余正文, 是否匹配)。例如 "@运维 查日志" → ("运维", "查日志", true)。
func parseAgentPrefix(text string) (prefix, rest string, ok bool) {
	if !strings.HasPrefix(text, "@") {
		return "", "", false
	}
	body := text[1:]
	idx := strings.IndexAny(body, " \t\n") // 找第一个空白作为前缀结束
	if idx <= 0 {                          // "@运维"（无正文）不算有效指令
		return "", "", false
	}
	return body[:idx], strings.TrimSpace(body[idx+1:]), true
}

// emitDone 发送回合结束事件（成功带内容，失败带错误文字）。
func (s *Service) emitDone(sessionID, content, errMsg string) {
	s.emit(EventDone, map[string]any{"sessionId": sessionID, "content": content, "error": errMsg})
}

// CancelTurn 取消该会话当前进行中的回合或部署执行（前端"停止"按钮的落点）。
// 幂等：没有进行中的回合/部署时静默返回 nil。取消语义仅限"当前会话的当前活动"：
// cancel 沿 ctx 向下传播 → LLM 流读取中断 / 部署命令中断 → 活动以取消错误收尾。
// 回合照常走 emitDone(error) 路径；部署走 deploy:done（canceled）帧。
// 注意：同一会话同时有回合与部署执行时，注册表只保留后登记者的取消句柄。
func (s *Service) CancelTurn(sessionID string) error {
	s.turnsMu.Lock()
	entry := s.turns[sessionID]
	s.turnsMu.Unlock()
	if entry != nil {
		slog.Info("取消进行中的回合", "session", sessionID)
		entry.cancel()
	}
	return nil
}

// turnErrText 把回合错误分类成面向用户的中文文案（写入 chat:done 的 error 字段）。
// 四类：响应超时（流空闲心跳）/ 回合超时（总上限）/ 已停止（用户取消）/ 一般错误。
// 判定顺序有讲究：先认 llm 包的哨兵错误（空闲取消在底层也表现为 context 取消），
// 再看 deadline / canceled。
func turnErrText(err error) string {
	switch {
	case errors.Is(err, llm.ErrStreamIdleTimeout):
		return "响应超时：LLM 长时间未返回新内容，已中止，请重试"
	case errors.Is(err, context.DeadlineExceeded):
		return "回合超时：本次回答超过最长处理时间，已中止，请重试"
	case errors.Is(err, context.Canceled):
		return "已停止"
	default:
		return err.Error()
	}
}

// truncate 按 rune 截断标题，超长加省略号。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
