package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	// go-openai：社区最主流的 OpenAI 协议 Go 客户端（零传递依赖）。
	// 所有 OpenAI 兼容端点（DeepSeek / 通义千问 compatible-mode / 本地 vLLM）都能用。
	openai "github.com/sashabaranov/go-openai"
)

// ErrStreamIdleTimeout 是"流空闲超时"的哨兵错误（≈ Java 的专用异常类型）：
// SSE 流长时间没有任何片段到达（半开连接）时，ChatStream 返回包装它的错误，
// 上层可用 errors.Is 识别并翻译成"响应超时"文案。
var ErrStreamIdleTimeout = errors.New("LLM 响应超时")

// streamIdleTimeout 流空闲心跳阈值：连续这么久没收到任何片段就取消流。
// 包级变量（而非常量）是为了让单元测试能临时调小；生产值 90 秒。
var streamIdleTimeout = 90 * time.Second

// OpenAIClient 是 Client 接口的 go-openai 实现（适配器模式）。
type OpenAIClient struct {
	apiKey  string
	baseURL string
	client  *openai.Client // SDK 客户端实例，内部复用 HTTP 连接
}

// NewOpenAIClient 构造适配器。baseURL 为空则用 OpenAI 官方地址；
// 填 DeepSeek/通义千问的兼容端点即可切换供应商。
func NewOpenAIClient(apiKey, baseURL string) *OpenAIClient {
	cfg := openai.DefaultConfig(apiKey) // 用 API Key 生成默认配置
	if baseURL != "" {
		cfg.BaseURL = baseURL // 覆盖服务端点（供应商切换的关键）
	}
	// 为 HTTP 传输配置合理的连接级超时（默认配置用 http.DefaultClient，
	// 拨号/TLS 握手都没有超时）。注意：绝不能设置 http.Client.Timeout——
	// 它覆盖整个响应体的读取，会把正常的 SSE 长流也拦腰掐断；
	// 流中途假死由 ChatStream 的空闲看门狗负责兜底。
	cfg.HTTPClient = &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second, // 等待首个响应头的上限
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	return &OpenAIClient{apiKey: apiKey, baseURL: baseURL, client: openai.NewClientWithConfig(cfg)}
}

// ChatStream 实现 Client 接口：把内部 Message 翻译成 SDK 请求，
// 消费 SSE 流，把文本增量转发给 onDelta，把工具调用分片拼装回完整 ToolCall。
func (c *OpenAIClient) ChatStream(ctx context.Context, model string, messages []Message, tools []ToolSpec, onDelta func(string) error) (*Response, error) {
	// --- 1. 组装请求 ---
	req := openai.ChatCompletionRequest{Model: model}
	for _, m := range messages {
		msg := openai.ChatCompletionMessage{Role: m.Role, Content: m.Content}
		if m.Role == RoleTool {
			msg.ToolCallID = m.ToolCallID // tool 消息必须声明它回的是哪次调用
		}
		for _, tc := range m.ToolCalls {
			// assistant 历史消息里携带的"我调了哪些工具"记录
			msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range tools {
		// 把技能说明书注册为模型可调用的 function
		req.Tools = append(req.Tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters, // JSON Schema 原样透传
			},
		})
	}

	// --- 2. 发起流式请求（底层是 SSE：服务端持续推送小片段）---
	// 空闲看门狗：派生一个可取消的 ctx，每收到一个片段就重置计时器；
	// streamIdleTimeout 内没有任何片段（半开连接的典型症状）就主动取消，
	// 让阻塞中的 stream.Recv() 立刻带着错误返回——回合绝不永久挂起。
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idleTimedOut atomic.Bool // 看门狗触发标记：区分"空闲超时"与"上层取消"
	idleTimer := time.AfterFunc(streamIdleTimeout, func() {
		idleTimedOut.Store(true)
		cancel()
	})
	defer idleTimer.Stop()

	stream, err := c.client.CreateChatCompletionStream(streamCtx, req)
	if err != nil {
		if idleTimedOut.Load() { // 请求阶段就看门狗超时（连响应头都没等到）
			slog.Error("LLM 响应超时（请求阶段）", "model", model, "idleTimeout", streamIdleTimeout.String())
			return nil, fmt.Errorf("%w：%v 内未建立流式连接", ErrStreamIdleTimeout, streamIdleTimeout)
		}
		slog.Error("LLM 请求发起失败", "model", model, "baseURL", c.baseURL, "err", err, "stack", string(debug.Stack()))
		return nil, fmt.Errorf("chat completion: %w", err)
	}
	defer stream.Close() // 无论如何关闭流（≈ 关闭 HTTP 响应体）

	var content strings.Builder      // 累积最终文本（strings.Builder ≈ StringBuilder）
	toolCalls := map[int]*ToolCall{} // 按分片 index 累积工具调用
	// 为什么用 map？因为流式协议下，一次工具调用的 name/arguments 会被
	// 切成很多小片陆续到达，每片带一个 index 表明属于第几个调用。

	// --- 3. 逐片消费 ---
	for {
		chunk, err := stream.Recv() // 阻塞读取下一个片段（≈ 迭代器 next()）
		if errors.Is(err, io.EOF) { // EOF = 流正常结束（errors.Is ≈ instanceof 判断哨兵错误）
			break
		}
		if err != nil {
			if idleTimedOut.Load() { // 看门狗触发：翻译成可识别的"响应超时"
				slog.Error("LLM 响应超时（流空闲）", "model", model, "idleTimeout", streamIdleTimeout.String())
				return nil, fmt.Errorf("%w：%v 内没有收到任何新内容", ErrStreamIdleTimeout, streamIdleTimeout)
			}
			slog.Error("LLM 流读取失败", "model", model, "err", err, "stack", string(debug.Stack()))
			return nil, fmt.Errorf("stream recv: %w", err)
		}
		idleTimer.Reset(streamIdleTimeout) // 收到片段 = 连接活着，重置空闲计时
		if len(chunk.Choices) == 0 { // 有些片段只有 usage 统计没有正文
			continue
		}
		delta := chunk.Choices[0].Delta // 本片段的增量内容
		if delta.Content != "" {
			content.WriteString(delta.Content) // 累积到最终文本
			if onDelta != nil {
				// 实时推给上层 → 最终变成前端事件，实现打字机效果
				if err := onDelta(delta.Content); err != nil {
					return nil, err // 上层要求中止（如窗口关闭）时中断流
				}
			}
		}
		for _, tc := range delta.ToolCalls {
			// 工具调用分片拼装：按 index 找到（或创建）对应的累积器
			idx := 0
			if tc.Index != nil { // SDK 新版本里 Index 是指针（可能缺省）
				idx = *tc.Index // *p 解引用取指针指向的值
			}
			cur, ok := toolCalls[idx]
			if !ok {
				cur = &ToolCall{}
				toolCalls[idx] = cur
			}
			if tc.ID != "" { // id 通常只在第一片出现
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" { // 函数名通常也只在第一片
				cur.Name = tc.Function.Name
			}
			cur.Arguments += tc.Function.Arguments // 参数 JSON 逐片拼接
		}
	}

	// --- 4. 汇总返回 ---
	resp := &Response{Message: Message{Role: RoleAssistant, Content: content.String()}}
	if len(toolCalls) > 0 {
		// map 遍历是无序的，按 index 排序保证工具调用顺序稳定（可复现、可测试）
		idx := make([]int, 0, len(toolCalls))
		for i := range toolCalls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			resp.Message.ToolCalls = append(resp.Message.ToolCalls, *toolCalls[i])
		}
	}
	return resp, nil
}

// ValidJSON 判断字符串是否为合法 JSON。
// Agent 循环在执行工具前用它防御模型偶尔吐出的坏参数。
func ValidJSON(s string) bool {
	return json.Valid([]byte(s))
}
