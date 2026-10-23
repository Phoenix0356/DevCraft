package appsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"DevCraft/internal/agent"
	"DevCraft/internal/dockerx"
	"DevCraft/internal/llm"
	"DevCraft/internal/secrets"
	"DevCraft/internal/skill"
	"DevCraft/internal/store"
)

// blockingLLM 模拟半开连接/极端慢吐流：阻塞直到 ctx 终止再返回 ctx 错误
// （与真实 llm 适配器被 ctx 取消后的行为一致）。
type blockingLLM struct{}

func (blockingLLM) ChatStream(ctx context.Context, _ string, _ []llm.Message, _ []llm.ToolSpec, _ func(string) error) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// newTestService 组装一个可测的 Service：临时 SQLite + 预置的 LLM 设置
// （跳过"未配置 Key"分支）+ 可替换的假 LLM 工厂 + 可观测的事件记录。
func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	lastTestDBPath = filepath.Join(dir, "test.db") // 供 apidoc 零残留断言开第二个只读连接做原始 SQL 计数
	st, err := store.Open(lastTestDBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	box, err := secrets.NewBox(dir)
	if err != nil {
		t.Fatalf("secrets box: %v", err)
	}
	skills := skill.NewRegistry()
	agents := agent.NewRegistry(st, skills)
	if err := agents.SeedDefaults(); err != nil {
		t.Fatalf("seed agents: %v", err)
	}
	// 测试专用"裸 Agent"：不装配任何技能（内置运维 Agent 的技能在本测试注册表里不存在）
	if err := agents.Upsert(store.AgentRow{ID: "bare", Name: "测试裸 Agent", SystemPrompt: "sys"}); err != nil {
		t.Fatalf("upsert bare agent: %v", err)
	}

	var mu sync.Mutex
	var doneErrs []string
	var titleFrames []map[string]any
	svc := New(st, agents, skills, dockerx.NewManager(), box, func(event string, payload map[string]any) {
		switch event {
		case EventDone:
			mu.Lock()
			if e, _ := payload["error"].(string); e != "" {
				doneErrs = append(doneErrs, e)
			}
			mu.Unlock()
		case EventSessionTitle:
			mu.Lock()
			titleFrames = append(titleFrames, payload)
			mu.Unlock()
		}
	})
	svc.newLLM = func(string, string) llm.Client { return blockingLLM{} }
	// 默认关掉"回合成功后异步总结标题"：见 Service.disableAutoTitle 注释
	// （否则多用例会多出一次并发 LLM 请求，打断按请求序列断言的回合用例）。
	// 标题自己的用例需要真链路时显式置回 false。
	svc.disableAutoTitle = true

	// 预置 LLM 设置（密文落库，走真实加密路径）
	enc, err := box.Encrypt("sk-test")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.SetSetting(settingLLMAPIKey, enc); err != nil {
		t.Fatalf("set api key: %v", err)
	}
	if err := st.SetSetting(settingLLMModel, "test-model"); err != nil {
		t.Fatalf("set model: %v", err)
	}

	// 把 done 错误记录的读取闭包挂到包级钩子，供各用例断言
	doneErrsHook = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), doneErrs...)
	}
	// 同理：捕获收到的 session:title 帧载荷（自动标题用例断言帧内容用）
	titleFramesHook = func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), titleFrames...)
	}
	return svc
}

// doneErrsHook 由 newTestService 设置，用例用它读取收到的 done 错误文案。
// （包级变量仅测试使用；同一包内测试串行执行，无需并发保护。）
var doneErrsHook func() []string

// titleFramesHook 由 newTestService 设置，用例用它读取收到的 session:title 帧载荷。
var titleFramesHook func() []map[string]any

// lastTestDBPath 由 newTestService 设置：最近一个测试库的文件路径
// （apidoc 拒收零残留断言用它开独立的只读连接做全表计数）。
var lastTestDBPath string

// withTurnTimeout 临时调小回合总上限，测试结束自动还原（生产值 3 分钟）。
func withTurnTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := turnTimeout
	turnTimeout = d
	t.Cleanup(func() { turnTimeout = old })
}

// activeTurns 读取会话级取消注册表当前条目数（直接访问包内字段做白盒断言）。
func (s *Service) activeTurns() int {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	return len(s.turns)
}

// newBareSession 建一个绑定"裸 Agent"的会话（回合直达 LLM 层，不碰技能）。
func newBareSession(t *testing.T, svc *Service) store.Session {
	t.Helper()
	sess, err := svc.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := svc.SetSessionAgent(sess.ID, "bare"); err != nil {
		t.Fatalf("set bare agent: %v", err)
	}
	sess.AgentID = "bare"
	return sess
}

// TestSendMessageTurnTimeout 回合总超时路径：假 LLM 持续阻塞（等价于慢吐超过上限），
// 回合必须在总上限附近中止，返回超时错误，发"回合超时"done 帧，注册表清理干净。
func TestSendMessageTurnTimeout(t *testing.T) {
	withTurnTimeout(t, 100*time.Millisecond)
	svc := newTestService(t)
	sess := newBareSession(t, svc)

	start := time.Now()
	err := svc.SendMessage(context.Background(), sess.ID, "hello")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected turn-timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errors.Is context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("turn timeout took too long: %v", elapsed)
	}
	errs := doneErrsHook()
	if len(errs) != 1 || !strings.Contains(errs[0], "回合超时") {
		t.Fatalf("done errors = %v, want one containing 回合超时", errs)
	}
	if n := svc.activeTurns(); n != 0 {
		t.Fatalf("turns registry not cleaned: %d entries", n)
	}
}

// TestSendMessageCancelTurn 取消路径：回合进行中调用 CancelTurn，
// 回合必须以取消错误结束、发"已停止"done 帧，注册表条目被清理。
func TestSendMessageCancelTurn(t *testing.T) {
	withTurnTimeout(t, time.Minute) // 上限放大，确保结束原因是取消而非超时
	svc := newTestService(t)
	sess := newBareSession(t, svc)

	errCh := make(chan error, 1)
	go func() { errCh <- svc.SendMessage(context.Background(), sess.ID, "hello") }()

	// 轮询等待回合登记进注册表（最多 2 秒）
	deadline := time.Now().Add(2 * time.Second)
	for svc.activeTurns() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("turn never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := svc.CancelTurn(sess.ID); err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancel error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want errors.Is context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not end after CancelTurn")
	}

	errs := doneErrsHook()
	if len(errs) != 1 || errs[0] != "已停止" {
		t.Fatalf("done errors = %v, want exactly [已停止]", errs)
	}
	if n := svc.activeTurns(); n != 0 {
		t.Fatalf("turns registry not cleaned: %d entries", n)
	}
}

// TestCancelTurnWithoutActiveTurn 幂等性：没有进行中回合时静默成功。
func TestCancelTurnWithoutActiveTurn(t *testing.T) {
	svc := newTestService(t)
	if err := svc.CancelTurn("no-such-session"); err != nil {
		t.Fatalf("CancelTurn without active turn = %v, want nil", err)
	}
	if err := svc.CancelTurn(""); err != nil {
		t.Fatalf("CancelTurn(empty) = %v, want nil", err)
	}
}

// TestSendMessageNormalTurnUnaffected 回归保护：快速应答的回合零影响，
// done 帧不带错误，注册表干净。
func TestSendMessageNormalTurnUnaffected(t *testing.T) {
	withTurnTimeout(t, 5*time.Second)
	svc := newTestService(t)
	svc.newLLM = func(string, string) llm.Client { return quickLLM{answer: "ok"} }
	sess := newBareSession(t, svc)
	if err := svc.SendMessage(context.Background(), sess.ID, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if errs := doneErrsHook(); len(errs) != 0 {
		t.Fatalf("unexpected done errors: %v", errs)
	}
	if n := svc.activeTurns(); n != 0 {
		t.Fatalf("turns registry not cleaned: %d entries", n)
	}
}

// quickLLM 立即返回固定回答（模拟正常快速回合）。
type quickLLM struct{ answer string }

func (q quickLLM) ChatStream(_ context.Context, _ string, _ []llm.Message, _ []llm.ToolSpec, onDelta func(string) error) (*llm.Response, error) {
	if onDelta != nil {
		_ = onDelta(q.answer)
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: q.answer}}, nil
}

// idleTimeoutLLM 模拟 llm 包空闲看门狗触发后的行为：返回包装哨兵的错误。
type idleTimeoutLLM struct{}

func (idleTimeoutLLM) ChatStream(context.Context, string, []llm.Message, []llm.ToolSpec, func(string) error) (*llm.Response, error) {
	return nil, fmt.Errorf("%w：90s 内没有收到任何新内容", llm.ErrStreamIdleTimeout)
}

// TestSendMessageIdleTimeoutClassified 流空闲超时的端到端分类：
// RPC 错误可 errors.Is 识别哨兵；done 帧文案是"响应超时"（而非回合超时/已停止）；注册表清理。
func TestSendMessageIdleTimeoutClassified(t *testing.T) {
	svc := newTestService(t)
	svc.newLLM = func(string, string) llm.Client { return idleTimeoutLLM{} }
	sess := newBareSession(t, svc)

	err := svc.SendMessage(context.Background(), sess.ID, "hello")
	if !errors.Is(err, llm.ErrStreamIdleTimeout) {
		t.Fatalf("err = %v, want errors.Is ErrStreamIdleTimeout", err)
	}
	errs := doneErrsHook()
	if len(errs) != 1 || !strings.Contains(errs[0], "响应超时") || strings.Contains(errs[0], "回合超时") {
		t.Fatalf("done errors = %v, want single 响应超时 wording", errs)
	}
	if n := svc.activeTurns(); n != 0 {
		t.Fatalf("turns registry not cleaned: %d entries", n)
	}
}

// TestTurnErrTextClassification 错误文案四分类：响应超时 / 回合超时 / 已停止 / 一般错误。
// 关键：哨兵优先于 context 错误判定（空闲取消底层也是 ctx 取消）；取消不得误判为超时。
func TestTurnErrTextClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"idle sentinel wrapped", fmt.Errorf("outer: %w", llm.ErrStreamIdleTimeout), "响应超时：LLM 长时间未返回新内容，已中止，请重试"},
		{"deadline", context.DeadlineExceeded, "回合超时：本次回答超过最长处理时间，已中止，请重试"},
		{"canceled", context.Canceled, "已停止"},
		{"generic", errors.New("connection reset"), "connection reset"},
	}
	for _, c := range cases {
		if got := turnErrText(c.err); got != c.want {
			t.Fatalf("%s: turnErrText = %q, want %q", c.name, got, c.want)
		}
	}
}

// ==================== /技能名 软路由（parseSkillPrefix + 注入） ====================

// stubSkill 测试用最小技能实现（只需要"存在于注册表"，不会被真正执行——
// 软路由只注入指令，工具调用由 LLM 决定，测试里的假 LLM 直接返回文本）。
type stubSkill struct{ name string }

func (s stubSkill) Name() string                                             { return s.name }
func (s stubSkill) Description() string                                      { return "测试技能" }
func (s stubSkill) Parameters() json.RawMessage                              { return json.RawMessage(`{}`) }
func (s stubSkill) Execute(context.Context, json.RawMessage) (string, error) { return "ok", nil }

// recordingLLM 记录每次收到的消息序列（断言注入形态），立即返回固定回答。
type recordingLLM struct {
	mu       sync.Mutex
	requests [][]llm.Message
	answer   string
}

func (r *recordingLLM) ChatStream(_ context.Context, _ string, msgs []llm.Message, _ []llm.ToolSpec, onDelta func(string) error) (*llm.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, append([]llm.Message(nil), msgs...))
	r.mu.Unlock()
	if onDelta != nil {
		_ = onDelta(r.answer)
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: r.answer}}, nil
}

// lastUserContent 最近一次请求里最后一条 user 消息的内容（= 本轮发给 LLM 的用户消息）。
func (r *recordingLLM) lastUserContent(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("recordingLLM received no requests")
	}
	msgs := r.requests[len(r.requests)-1]
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			return msgs[i].Content
		}
	}
	t.Fatal("no user message in recorded request")
	return ""
}

// TestParseSkillPrefix 解析规则与 parseAgentPrefix 镜像：
// 行首 / 触发、必须有空白分隔的正文、名字取第一段、正文 TrimSpace。
func TestParseSkillPrefix(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantName string
		wantRest string
		wantOK   bool
	}{
		{"正常", "/pg_query 查 papers 前 10 条", "pg_query", "查 papers 前 10 条", true},
		{"无正文", "/pg_query", "", "", false},
		{"仅空白正文", "/pg_query \t\n", "pg_query", "", true}, // 调用方 SendMessage 先 TrimSpace，实际到不了这个形态
		{"名字前无正文", "/ 查日志", "", "", false},
		{"多空格分隔", "/pg_query    查   日志", "pg_query", "查   日志", true},
		{"制表符分隔", "/pg_query\t查日志", "pg_query", "查日志", true},
		{"换行分隔", "/pg_query\n查日志", "pg_query", "查日志", true},
		{"中文正文", "/ops_demo 把服务部署到生产环境并验证", "ops_demo", "把服务部署到生产环境并验证", true},
		{"非行首触发", "请 /pg_query 查日志", "", "", false},
		{"@ 前缀不命中", "@运维 查日志", "", "", false},
		{"空串", "", "", "", false},
	}
	for _, c := range cases {
		name, rest, ok := parseSkillPrefix(c.in)
		if ok != c.wantOK || name != c.wantName || rest != c.wantRest {
			t.Errorf("%s: parseSkillPrefix(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.name, c.in, name, rest, ok, c.wantName, c.wantRest, c.wantOK)
		}
	}
}

// TestSendMessageSkillInjection 软路由命中路径：技能存在于注册表 →
// 入库/展示为去前缀的正文（与 @ 前缀存储语义对齐），发给 LLM 的用户消息
// 带"【用户指定】"注入指令（仅本轮，不入库）。当前 Agent（bare）未装配
// 该技能也照常注入不拦截（PRD 决策：LLM 无工具时自然降级）。
func TestSendMessageSkillInjection(t *testing.T) {
	withTurnTimeout(t, 5*time.Second)
	svc := newTestService(t)
	if err := svc.skills.Register(stubSkill{name: "demo_query"}); err != nil {
		t.Fatalf("register stub skill: %v", err)
	}
	rec := &recordingLLM{answer: "ok"}
	svc.newLLM = func(string, string) llm.Client { return rec }
	sess := newBareSession(t, svc)

	if err := svc.SendMessage(context.Background(), sess.ID, "/demo_query  查 papers 前 10 条"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// 发给 LLM 的用户消息 = 注入指令 + 正文
	wantLLM := "【用户指定】请优先使用技能 demo_query 完成以下请求：\n查 papers 前 10 条"
	if got := rec.lastUserContent(t); got != wantLLM {
		t.Fatalf("LLM user message = %q, want %q", got, wantLLM)
	}
	// 入库的是去前缀正文（注入文本不落库）
	msgs, err := svc.Messages(sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) < 1 || msgs[0].Content != "查 papers 前 10 条" {
		t.Fatalf("stored user message = %+v, want content 查 papers 前 10 条", msgs[0])
	}
	// 会话标题也用去前缀正文
	if s, err := svc.store.GetSession(sess.ID); err != nil || s.Title != "查 papers 前 10 条" {
		t.Fatalf("session title = %q (err %v), want 查 papers 前 10 条", s.Title, err)
	}
}

// TestSendMessageSkillFailOpen fail-open 路径：技能不存在 / 无正文 →
// 按普通文本原样入库、原样发给 LLM，不报错。
func TestSendMessageSkillFailOpen(t *testing.T) {
	withTurnTimeout(t, 5*time.Second)
	svc := newTestService(t)
	if err := svc.skills.Register(stubSkill{name: "demo_query"}); err != nil {
		t.Fatalf("register stub skill: %v", err)
	}
	rec := &recordingLLM{answer: "ok"}
	svc.newLLM = func(string, string) llm.Client { return rec }

	cases := []struct {
		name string
		in   string
	}{
		{"技能不存在", "/no_such_skill 查日志"},
		{"无正文", "/demo_query"},
		{"非行首触发", "请 /demo_query 查日志"},
	}
	for _, c := range cases {
		sess := newBareSession(t, svc)
		if err := svc.SendMessage(context.Background(), sess.ID, c.in); err != nil {
			t.Fatalf("%s: SendMessage: %v", c.name, err)
		}
		if got := rec.lastUserContent(t); got != c.in {
			t.Errorf("%s: LLM user message = %q, want %q（原样发送）", c.name, got, c.in)
		}
		msgs, err := svc.Messages(sess.ID)
		if err != nil {
			t.Fatalf("%s: Messages: %v", c.name, err)
		}
		if msgs[0].Content != c.in {
			t.Errorf("%s: stored user message = %q, want %q（原样入库）", c.name, msgs[0].Content, c.in)
		}
	}
	if errs := doneErrsHook(); len(errs) != 0 {
		t.Fatalf("unexpected done errors: %v", errs)
	}
}

// TestSendMessageAgentThenSkillPrefix 组合路径："@Agent /技能 正文" →
// ① @ 路由先切 Agent，①' 软路由再解析 / 前缀；两级前缀都被剥离，
// 入库只剩正文，LLM 收到注入指令。
func TestSendMessageAgentThenSkillPrefix(t *testing.T) {
	withTurnTimeout(t, 5*time.Second)
	svc := newTestService(t)
	if err := svc.skills.Register(stubSkill{name: "demo_query"}); err != nil {
		t.Fatalf("register stub skill: %v", err)
	}
	rec := &recordingLLM{answer: "ok"}
	svc.newLLM = func(string, string) llm.Client { return rec }
	sess := newBareSession(t, svc)

	if err := svc.SendMessage(context.Background(), sess.ID, "@bare /demo_query 查日志"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	wantLLM := "【用户指定】请优先使用技能 demo_query 完成以下请求：\n查日志"
	if got := rec.lastUserContent(t); got != wantLLM {
		t.Fatalf("LLM user message = %q, want %q", got, wantLLM)
	}
	msgs, err := svc.Messages(sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if msgs[0].Content != "查日志" {
		t.Fatalf("stored user message = %q, want 查日志", msgs[0].Content)
	}
}
