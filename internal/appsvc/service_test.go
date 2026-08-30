package appsvc

import (
	"context"
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
	st, err := store.Open(filepath.Join(dir, "test.db"))
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
	svc := New(st, agents, skills, dockerx.NewManager(), box, func(event string, payload map[string]any) {
		if event != EventDone {
			return
		}
		mu.Lock()
		if e, _ := payload["error"].(string); e != "" {
			doneErrs = append(doneErrs, e)
		}
		mu.Unlock()
	})
	svc.newLLM = func(string, string) llm.Client { return blockingLLM{} }

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
	return svc
}

// doneErrsHook 由 newTestService 设置，用例用它读取收到的 done 错误文案。
// （包级变量仅测试使用；同一包内测试串行执行，无需并发保护。）
var doneErrsHook func() []string

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
