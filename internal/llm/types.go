// Package llm 把"聊天补全大模型"抽象成一个最小接口 Client，
// 让 Agent 引擎与任何具体 SDK（go-openai / 官方 SDK / LangChainGo）解耦。
// 类比 Java：这里定义了 SPI 接口，openai.go 是它的一个实现。
// 将来换供应商，只需新增一个实现文件，Agent 循环一行不用改。
package llm

import (
	"context"
	"encoding/json"
)

// 对话角色常量（OpenAI 协议规定的四种角色）。
// Go 没有 enum，惯例是用一组 const + iota 或直接字符串常量。
const (
	RoleSystem    = "system"    // 系统提示词：定义 Agent 人设与规则
	RoleUser      = "user"      // 用户输入
	RoleAssistant = "assistant" // 模型回复（可能携带 tool_calls）
	RoleTool      = "tool"      // 工具执行结果，必须带 ToolCallID 回指
)

// ToolCall 表示模型决定调用某个工具（function-calling 协议）。
type ToolCall struct {
	ID        string `json:"id"`        // 本次调用的唯一 id，回填结果时要对上
	Name      string `json:"name"`      // 技能名，如 ops.analyze_logs
	Arguments string `json:"arguments"` // 参数的原始 JSON 字符串（模型生成的）
}

// Message 对话中的一条消息。
// 同一条消息在不同角色下使用不同字段（类似一个"联合体"）：
//   - user/assistant 文本消息：只用 Content
//   - assistant 决定调工具：Content 可能为空，ToolCalls 有值
//   - tool 结果消息：Content=结果文本，ToolCallID 指向对应的 ToolCall
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"` // omitempty = 空值时 JSON 里省略该字段
	ToolCallID string     `json:"toolCallId,omitempty"`
	ToolName   string     `json:"toolName,omitempty"`
}

// ToolSpec 是一个技能的"说明书"，直接对齐 OpenAI function-calling 的 tool 定义。
// 把 Skill 的元数据原样塞给模型，模型才知道有哪些工具可用、参数长什么样。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"` // 写给 LLM 看的"何时该用我"
	Parameters  json.RawMessage `json:"parameters"`  // json.RawMessage = 延迟解析的原始 JSON（这里是参数 JSON Schema）
}

// Response 一轮补全的结果（流式接收完毕后的汇总）。
type Response struct {
	Message Message // 最终的 assistant 消息（含可能的 ToolCalls）
}

// Client 是本包的核心接口：Agent 循环与大模型之间唯一的缝。
// 只有一个方法，越小越好换实现。
//
// ChatStream 发起一次流式补全：
//   - model:    模型名（deepseek-chat / qwen-plus ...）
//   - messages: 完整对话历史（system + 历史 + 本轮）
//   - tools:    允许模型调用的工具清单
//   - onDelta:  回调函数（≈ Java 的 Consumer<String>）：模型每吐出一小段文字就调用一次，
//     前端借此实现"打字机"流式效果。注意：工具调用的参数分片不走这个回调。
//
// 返回值是整轮结束后的汇总消息；若包含 ToolCalls，调用方负责执行工具后再发起下一轮。
type Client interface {
	ChatStream(ctx context.Context, model string, messages []Message, tools []ToolSpec, onDelta func(delta string) error) (*Response, error)
}
