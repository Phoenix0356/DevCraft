package main

import (
	"context" // 承载超时/取消信号；这里持有应用生命周期 ctx 供长耗时业务调用
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync" // RWMutex：保护聊天流连接注册表

	"github.com/wailsapp/wails/v3/pkg/application" // Wails v3 应用框架（Streams/生命周期）

	"DevCraft/internal/agent"        // Agent 引擎：Agent 定义注册表 + tool-calling 循环
	"DevCraft/internal/appsvc"       // 编排层：会话/路由/设置的业务逻辑（本文件只做转发）
	"DevCraft/internal/dockerx"      // Docker SDK 封装
	"DevCraft/internal/secrets"      // API Key 加解密
	"DevCraft/internal/skill"        // Skill 接口与注册表
	"DevCraft/internal/skill/deploy" // 一键部署技能（生成待审批单，批准后由 appsvc 执行）
	"DevCraft/internal/skill/ops"    // 运维域内置技能
	"DevCraft/internal/store"        // SQLite 持久层
)

// chatStreamName 是聊天流式推送的 v3 Stream 通道名。
// 前端用 Stream('chat') 建立连接；服务器模式下是真 WebSocket（每标签页独立连接，
// 天然按客户端隔离），桌面模式下由框架在进程内模拟同等语义。
const chatStreamName = "chat"

// App 是 Wails 绑定对象（≈ Controller）。
// 大写开头的方法会被自动生成前端绑定；每个方法只做一件事：转发给 svc（薄 Controller 原则）。
type App struct {
	wailsApp *application.App // Wails 应用实例（事件/流推送入口），由 bind() 注入
	ctx      context.Context  // 应用生命周期 ctx（ServiceStartup 传入），供长耗时调用使用
	svc      *appsvc.Service  // 真正的业务服务（手动依赖注入，Go 没有 Spring）

	// 聊天流连接注册表：所有打开的 chat Stream 连接。
	// 业务层发事件时向全部连接扇出（前端按 sessionId 过滤，与 v2 行为一致）。
	streamsMu sync.RWMutex
	streams   map[*application.StreamConn]struct{}
}

// NewApp 是构造函数惯例写法（Go 无构造器语法）。
// 只返回空壳，依赖组装推迟到 ServiceStartup。
func NewApp() *App {
	return &App{streams: map[*application.StreamConn]struct{}{}}
}

// bind 由 main 在 app.Run() 之前调用：注入应用实例并注册聊天流处理器。
// 小写开头 = 不会暴露给前端绑定。
func (a *App) bind(app *application.App) {
	a.wailsApp = app
	// 每个前端连接（桌面窗口 / 浏览器标签页各一条）触发一次 handler，
	// 在自己的 goroutine 里运行：注册进扇出表，连接关闭时自动注销。
	app.HandleStream(chatStreamName, func(c *application.StreamConn) {
		a.streamsMu.Lock()
		a.streams[c] = struct{}{}
		a.streamsMu.Unlock()

		<-c.Context().Done() // 阻塞到连接关闭（页面刷新/关闭/进程退出）

		a.streamsMu.Lock()
		delete(a.streams, c)
		a.streamsMu.Unlock()
	})
}

// emitChat 把业务层事件以 JSON 帧推送给所有打开的聊天流连接。
// 帧格式：{"event": "chat:delta|chat:tool|chat:done", "payload": {...}}
func (a *App) emitChat(event string, payload map[string]any) {
	frame, err := json.Marshal(map[string]any{"event": event, "payload": payload})
	if err != nil {
		slog.Error("聊天事件序列化失败", "event", event, "err", err)
		return
	}
	a.streamsMu.RLock()
	defer a.streamsMu.RUnlock()
	for c := range a.streams {
		if err := c.Send(frame); err != nil {
			slog.Debug("聊天流推送失败（连接可能已关闭）", "event", event, "err", err)
		}
	}
}

// ServiceStartup 是 v3 生命周期钩子（≈ @PostConstruct），应用启动时调用一次。
// 实现 application.ServiceStartup 接口；该方法名在绑定生成器中被自动排除。
func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	a.ctx = ctx
	svc, err := buildService(a.emitChat)
	if err != nil {
		slog.Error("DevCraft 初始化失败", "err", err, "stack", string(debug.Stack()))
		return err
	}
	a.svc = svc
	slog.Info("DevCraft 初始化完成")
	return nil
}

// buildService 是"手动组装依赖"的工厂函数（≈ 手工版 @Configuration + @Bean）。
// 组装顺序有讲究：store → secrets → docker → skill 注册表 → agent 注册表（播种内置 Agent）
// → appsvc 服务 → 注册运维技能（需要 svc.DockerEndpoint 回调读实时配置）。
func buildService(emit appsvc.Emit) (*appsvc.Service, error) {
	// 1. 数据目录：默认用户配置目录（Windows: %APPDATA%）；
	//    DEVCRAFT_DATA_DIR 可覆盖（服务器/systemd 部署时指向独立目录，如 /var/lib/devcraft）
	dataDir := os.Getenv("DEVCRAFT_DATA_DIR")
	if dataDir == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("locate config dir: %w", err) // %w 保留原始错误链（≈ cause）
		}
		dataDir = filepath.Join(cfg, "DevCraft")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil { // 0700 = 仅当前用户可读写
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// 2. 打开 SQLite（文件不存在会自动创建），见 internal/store
	st, err := store.Open(filepath.Join(dataDir, "devcraft.db"))
	if err != nil {
		return nil, err
	}
	// 3. 密钥盒：首次运行会在 dataDir 生成 secret.key（32 字节随机密钥），用于加密 API Key
	box, err := secrets.NewBox(dataDir)
	if err != nil {
		return nil, err
	}

	// 4. Docker 客户端管理器（按 host 缓存连接）+ Skill 注册表（全局能力池）
	docker := dockerx.NewManager()
	skills := skill.NewRegistry()

	// 5. Agent 注册表并播种内置"运维 Agent"（幂等：已存在则跳过）
	agents := agent.NewRegistry(st, skills)
	if err := agents.SeedDefaults(); err != nil {
		return nil, fmt.Errorf("seed builtin agents: %w", err)
	}

	// 6. 编排服务；svc.DockerEndpoint 作为回调传给运维技能——技能执行时才读"当前"
	// 配置的 host 与 SSH 密码（ssh:// 走 SSH 命令执行模式，其余走 SDK 直连）
	svc := appsvc.New(st, agents, skills, docker, box, emit)
	if err := ops.Register(skills, docker, svc.DockerEndpoint); err != nil {
		return nil, err
	}
	// 7. 一键部署技能：技能本身只校验参数并生成待审批单（绝不执行）；
	// svc 同时充当流程数据源（FlowSource）与审批单宿主（ApprovalSubmitter）。
	// 依赖方向：deploy 只定义接口，由这里注入实现，无循环依赖。
	if err := deploy.Register(skills, svc, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

// --- Wails 绑定方法（前端可调用的"接口"）---

// svcOrErr 防御初始化失败的情况（ServiceStartup 出错时 svc 为 nil）。
func (a *App) svcOrErr() (*appsvc.Service, error) {
	if a.svc == nil {
		return nil, fmt.Errorf("服务未初始化，请重启应用")
	}
	return a.svc, nil
}

// NewSession 创建新会话（默认绑定运维 Agent）。前端：NewSession()
func (a *App) NewSession() (store.Session, error) {
	svc, err := a.svcOrErr()
	if err != nil {
		return store.Session{}, err // Go 没有 null 对象，出错时返回"零值 struct"
	}
	return svc.NewSession()
}

// ListSessions 会话列表（按更新时间倒序）。前端：ListSessions()
func (a *App) ListSessions() ([]store.Session, error) {
	svc, err := a.svcOrErr()
	if err != nil {
		return nil, err
	}
	return svc.ListSessions()
}

// DeleteSession 删除会话及其全部消息。前端：DeleteSession(id)
func (a *App) DeleteSession(id string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.DeleteSession(id)
}

// SetSessionAgent 切换会话绑定的 Agent。前端：SetSessionAgent(sessionID, agentID)
func (a *App) SetSessionAgent(sessionID, agentID string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.SetSessionAgent(sessionID, agentID)
}

// GetMessages 取会话历史消息。前端：GetMessages(sessionID)
func (a *App) GetMessages(sessionID string) ([]store.Message, error) {
	svc, err := a.svcOrErr()
	if err != nil {
		return nil, err
	}
	return svc.Messages(sessionID)
}

// SendMessage 发送一条用户消息并同步执行完整 Agent 回合。
// 注意：这是阻塞调用——前端 await 它直到整个回合结束；
// 期间的流式增量通过聊天流（chat:delta / chat:tool / chat:done 帧）异步推送。
func (a *App) SendMessage(sessionID, text string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.SendMessage(a.ctx, sessionID, text)
}

// CancelTurn 取消该会话进行中的回合（前端流式期间的"停止"按钮）。
// 幂等：没有进行中回合时静默成功。前端：CancelTurn(sessionID)
func (a *App) CancelTurn(sessionID string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.CancelTurn(sessionID)
}

// ListAgents 所有 Agent 定义（含挂载的技能列表）。前端：ListAgents()
func (a *App) ListAgents() ([]store.AgentRow, error) {
	svc, err := a.svcOrErr()
	if err != nil {
		return nil, err
	}
	return svc.ListAgents()
}

// --- 智能体管理（信息编辑 + 技能装配）---

// ListAgentsDetail 智能体管理明细列表（基本信息 + 已装配技能 + 可选技能全集，
// 技能带内置/自定义分类标记）。前端：ListAgentsDetail()
func (a *App) ListAgentsDetail() ([]appsvc.AgentDetail, error) {
	svc, err := a.svcOrErr()
	if err != nil {
		return nil, err
	}
	return svc.ListAgentsDetail()
}

// SaveAgent 保存智能体信息（名称/模型/系统提示词；名称非空校验在编排层）。
// 前端：SaveAgent(id, name, model, systemPrompt)
func (a *App) SaveAgent(id, name, model, systemPrompt string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.SaveAgent(id, name, model, systemPrompt)
}

// SetAgentSkills 整组替换智能体的技能装配；落库即生效（Runner 每回合实时读，
// 下轮对话工具列表自动变化，无需通知聊天侧）。前端：SetAgentSkills(id, skillNames)
func (a *App) SetAgentSkills(id string, skillNames []string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.SetAgentSkills(id, skillNames)
}

// ListSkills 全部技能的元数据（名称/描述/参数 Schema，按名排序）。
// 设置页"技能管理"区的数据源；部署技能的描述为实时动态值（含当前流程清单）。
// 前端：ListSkills()
func (a *App) ListSkills() ([]appsvc.SkillInfo, error) {
	svc, err := a.svcOrErr()
	if err != nil {
		return nil, err
	}
	return svc.ListSkills(), nil
}

// GetSettings 读设置（API Key 只回传"是否已设置"，绝不回传明文）。
func (a *App) GetSettings() (appsvc.SettingsView, error) {
	svc, err := a.svcOrErr()
	if err != nil {
		return appsvc.SettingsView{}, err
	}
	return svc.GetSettings()
}

// SaveSettings 保存设置；apiKey 为空字符串表示"保持原值不变"。
func (a *App) SaveSettings(in appsvc.Settings) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.SaveSettings(in)
}

// TestSSH 用表单当前值测试连接（IP 留空测本机；否则 SSH 登录远端执行 docker version）。
func (a *App) TestSSH(ip, port, user, password string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.TestSSH(a.ctx, ip, port, user, password)
}

// TestLLM 测试 LLM 连接（发一条 "ping"）。
func (a *App) TestLLM() error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.TestLLM(a.ctx)
}

// --- 一键部署绑定（流程 CRUD + 审批）---

// ListDeployFlows 部署流程列表（设置页管理区）。
func (a *App) ListDeployFlows() ([]store.DeployFlow, error) {
	svc, err := a.svcOrErr()
	if err != nil {
		return nil, err
	}
	return svc.ListDeployFlows()
}

// SaveDeployFlow 新建/更新部署流程（ID=0 表示新建）。保存前做业务校验。
func (a *App) SaveDeployFlow(flow store.DeployFlow) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.SaveDeployFlow(flow)
}

// DeleteDeployFlow 删除部署流程（执行历史保留）。
func (a *App) DeleteDeployFlow(id int64) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.DeleteDeployFlow(id)
}

// ApproveDeployment 批准部署审批单：校验门禁通过后异步执行，进度经聊天流帧推送。
// 未批准/已拒绝/已过期的单子在这里都会被拒绝（代码强制门禁）。
func (a *App) ApproveDeployment(id string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.ApproveDeployment(id)
}

// RejectDeployment 拒绝部署审批单（绝不执行）。
func (a *App) RejectDeployment(id string) error {
	svc, err := a.svcOrErr()
	if err != nil {
		return err
	}
	return svc.RejectDeployment(id)
}
