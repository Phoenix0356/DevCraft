// Package agent 包含三部分：
//  1. Agent 的数据化定义（Agent = system prompt + 挂载技能 + 模型，全部存 SQLite）
//  2. Registry：Agent 注册表（读写 store，启动时播种内置 Agent）
//  3. Runner：tool-calling 循环引擎——整个 AI 能力的发动机
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug" // debug.Stack()：出错时打印 goroutine 堆栈（Go 的 error 不自带堆栈）
	"time"

	"DevCraft/internal/llm"
	"DevCraft/internal/skill"
	"DevCraft/internal/store"
)

// BuiltinOpsAgentID 内置运维 Agent 的 id。
const BuiltinOpsAgentID = "ops"

// opsSystemPrompt 运维 Agent 的人设提示词（会作为 system 消息发给 LLM）。
// 反引号原始字符串，多行原样保留。
const opsSystemPrompt = `你是 DevCraft 的运维 Agent，负责帮助用户查看和管理 Docker 容器，以及执行用户自定义的部署流程。

你可以使用挂载的运维技能来查询容器状态、资源占用和日志，使用部署技能触发部署流程。规则：
1. 涉及容器操作时，先调用合适的技能获取真实数据，不要凭空编造。
2. 用户要求部署/发布/上线时，确认流程名与所需参数（缺参数先向用户询问），再调用部署技能；
   该技能只会生成待审批单，必须等用户在聊天卡片中点击批准后才会真正执行，切勿声称部署已完成。
3. 拿到日志或错误信息后，给出简明的根因分析和可执行的修复建议。
4. 技能执行失败时，把错误原因用易懂的语言告诉用户，并给出排查方向。
5. 回答使用中文，重点信息可用列表呈现。`

// Registry Agent 注册表：定义数据存 SQLite，本对象负责读写与校验。
type Registry struct {
	store  *store.Store    // 持久层依赖（构造注入）
	skills *skill.Registry // 技能注册表：装配校验时检查技能是否真实存在
}

// NewRegistry 构造注册表。
func NewRegistry(st *store.Store, skills *skill.Registry) *Registry {
	return &Registry{store: st, skills: skills}
}

// SeedDefaults 确保内置运维 Agent 存在并保持最新（幂等）。
// 每次启动都 upsert：既负责首次"出厂预装"，也把旧版点号命名的
// 技能装配迁移为下划线命名（LLM 工具名字符集不允许点号），
// 并把新增技能（如 deploy_run_flow）补进旧库的装配列表。
func (r *Registry) SeedDefaults() error {
	return r.store.UpsertAgent(store.AgentRow{
		ID:           BuiltinOpsAgentID,
		Name:         "运维 Agent",
		SystemPrompt: opsSystemPrompt,
		Model:        "", // 留空 = 跟随全局默认模型
		Builtin:      true,
		Skills: []string{ // 装配的运维三技能 + 一键部署技能
			"ops_list_containers",
			"ops_container_stats",
			"ops_analyze_logs",
			"deploy_run_flow",
		},
	})
}

// List 列出全部 Agent 定义（前端 Agent 选择器用）。
func (r *Registry) List() ([]store.AgentRow, error) { return r.store.ListAgents() }

// Get 按 id 取 Agent 定义。
func (r *Registry) Get(id string) (store.AgentRow, error) { return r.store.GetAgent(id) }

// Upsert 保存 Agent 定义；保存前校验挂载的每个技能都已注册（fail-fast）。
func (r *Registry) Upsert(a store.AgentRow) error {
	if _, err := r.skills.Resolve(a.Skills); err != nil {
		return err
	}
	return r.store.UpsertAgent(a)
}

// Events 是 Runner 向调用方（appsvc）汇报过程的回调集合。
// Go 没有 Java 的监听器接口惯例，直接用"函数字段"当回调（字段本身是函数类型）。
type Events struct {
	OnDelta      func(delta string)                     // 模型吐出一小段文字（流式）
	OnToolStart  func(name, argsJSON string)            // 开始执行某技能
	OnToolResult func(name, result string, failed bool) // 技能执行结束（含成败）
}

// Runner 执行引擎：一次用户回合 = 若干轮 LLM 调用 + 工具执行的循环。
type Runner struct {
	llm    llm.Client      // LLM 客户端（接口隔离，可换实现）
	skills *skill.Registry // 全局技能池，按 Agent 装配取子集
	model  string          // 全局默认模型（Agent 未指定模型时使用）
}

// NewRunner 构造执行引擎。
func NewRunner(c llm.Client, skills *skill.Registry, defaultModel string) *Runner {
	return &Runner{llm: c, skills: skills, model: defaultModel}
}

// maxIterations 工具调用轮次上限——防止模型陷入"调工具→再调工具"死循环烧 token。
const maxIterations = 6

// Run 处理一个用户回合。history 已包含本次用户消息（由 appsvc 组装）。
// 返回最终的 assistant 文本回答。
//
// 核心循环（tool-calling 协议）：
//
//	┌─► 调 LLM（携带全部历史 + 工具清单）
//	│     ├─ 返回纯文本 → 结束，这就是最终回答
//	│     └─ 返回 tool_calls → 逐个执行技能
//	│            └─ 把每个结果作为 tool 消息追加到历史
//	└─────────── 带着新历史再调 LLM
func (r *Runner) Run(ctx context.Context, a store.AgentRow, history []llm.Message, ev Events) (string, error) {
	// --- 1. 组装消息序列：system prompt 打头，其后是完整历史 ---
	msgs := make([]llm.Message, 0, len(history)+1)
	if a.SystemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: a.SystemPrompt})
	}
	msgs = append(msgs, history...) // history... 展开追加（≈ addAll）

	// --- 2. 把该 Agent 装配的技能转成工具清单（每 Agent 只暴露自己的子集）---
	tools, err := r.toolSpecs(a.Skills)
	if err != nil {
		return "", err
	}

	// --- 3. 决定模型：Agent 专属模型 > 全局默认模型 ---
	model := a.Model
	if model == "" {
		model = r.model
	}
	if model == "" {
		return "", fmt.Errorf("未配置模型：请在设置中填写默认模型，或为该 Agent 指定模型")
	}

	slog.Info("Agent 回合开始", "agent", a.ID, "model", model, "tools", len(tools), "history", len(history))

	// --- 4. 主循环 ---
	for iter := 0; iter < maxIterations; iter++ {
		// 每轮开始前显式检查取消/超时（回合总上限到期、用户点"停止"都会走这里）：
		// 立即终止，不再发起新的 LLM 调用。ChatStream 返回的错误同样原样透传，
		// 循环内任何一处失败都不会被吞掉。
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// 调一轮 LLM；onDelta 回调把流式增量透传给上层（最终变成前端事件）
		resp, err := r.llm.ChatStream(ctx, model, msgs, tools, func(delta string) error {
			if ev.OnDelta != nil {
				ev.OnDelta(delta)
			}
			return nil
		})
		if err != nil {
			// 出错必打堆栈：Go 的 error 不像 Java 异常自带 stacktrace，
			// 用 debug.Stack() 抓当前 goroutine 的调用栈辅助定位
			slog.Error("LLM 调用失败", "agent", a.ID, "model", model, "err", err, "stack", string(debug.Stack()))
			return "", err
		}
		// assistant 消息（含可能的 tool_calls）必须原样放回历史，
		// 下一轮模型才能对上自己刚才的调用决定
		msgs = append(msgs, resp.Message)

		// 没有工具调用 = 模型给出最终回答，循环结束
		if len(resp.Message.ToolCalls) == 0 {
			slog.Info("Agent 回合完成", "agent", a.ID, "rounds", iter+1, "answerLen", len(resp.Message.Content))
			return resp.Message.Content, nil
		}

		// 逐个执行模型要求的工具调用
		for _, tc := range resp.Message.ToolCalls {
			start := time.Now()
			result, failed := r.dispatch(ctx, tc)
			slog.Info("技能调用", "skill", tc.Name, "failed", failed, "elapsed", time.Since(start).String())
			// 通过事件回调汇报工具状态（前端显示"ops_analyze_logs 执行中…"标签）
			if ev.OnToolStart != nil {
				ev.OnToolStart(tc.Name, tc.Arguments)
			}
			if ev.OnToolResult != nil {
				ev.OnToolResult(tc.Name, result, failed)
			}
			// 结果作为 tool 消息回填：ToolCallID 必须与调用对上（协议要求）
			msgs = append(msgs, llm.Message{
				Role:       llm.RoleTool,
				Content:    result,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
			})
		}
		// 带着工具结果进入下一轮 LLM 调用
	}
	slog.Error("工具调用轮次超限", "agent", a.ID, "max", maxIterations)
	return "", fmt.Errorf("工具调用轮次超过上限（%d），已中止", maxIterations)
}

// toolSpecs 把技能名列表解析成工具说明书列表（发给 LLM 的 tools 参数）。
func (r *Runner) toolSpecs(names []string) ([]llm.ToolSpec, error) {
	ss, err := r.skills.Resolve(names)
	if err != nil {
		return nil, err
	}
	out := make([]llm.ToolSpec, 0, len(ss))
	for _, s := range ss {
		out = append(out, llm.ToolSpec{Name: s.Name(), Description: s.Description(), Parameters: s.Parameters()})
	}
	return out, nil
}

// dispatch 执行一次工具调用。
// 设计决策：失败不抛断整个回合——把错误文字作为 tool 结果回填，
// 让 LLM 用自己的话向用户解释（错误兜底体验），第二返回值标记是否失败。
func (r *Runner) dispatch(ctx context.Context, tc llm.ToolCall) (string, bool) {
	s, ok := r.skills.Get(tc.Name)
	if !ok { // 模型幻觉出一个不存在的技能名（偶发）
		return fmt.Sprintf("技能 %s 未注册，无法执行。", tc.Name), true
	}
	args := tc.Arguments
	if !json.Valid([]byte(args)) { // 防御模型偶尔吐出的坏 JSON
		args = "{}"
	}
	result, err := s.Execute(ctx, json.RawMessage(args))
	if err != nil {
		slog.Error("技能执行失败", "skill", tc.Name, "args", args, "err", err, "stack", string(debug.Stack()))
		return fmt.Sprintf("技能执行失败: %v", err), true
	}
	return result, false
}
