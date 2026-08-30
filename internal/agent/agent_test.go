package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"DevCraft/internal/llm"
	"DevCraft/internal/skill"
	"DevCraft/internal/store"
)

type fakeSkill struct {
	name    string
	lastArg string
}

func (f *fakeSkill) Name() string        { return f.name }
func (f *fakeSkill) Description() string { return "fake" }
func (f *fakeSkill) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (f *fakeSkill) Execute(_ context.Context, args json.RawMessage) (string, error) {
	f.lastArg = string(args)
	return "tool-result", nil
}

// fakeLLM scripts successive completion rounds.
type fakeLLM struct {
	rounds []llm.Message
	calls  int
}

func (f *fakeLLM) ChatStream(_ context.Context, _ string, _ []llm.Message, _ []llm.ToolSpec, onDelta func(string) error) (*llm.Response, error) {
	if f.calls >= len(f.rounds) {
		return nil, errors.New("unexpected extra round")
	}
	msg := f.rounds[f.calls]
	f.calls++
	if msg.Content != "" && onDelta != nil {
		_ = onDelta(msg.Content)
	}
	return &llm.Response{Message: msg}, nil
}

func runCase(t *testing.T, rounds []llm.Message, skills ...skill.Skill) (string, *Runner, error) {
	t.Helper()
	reg := skill.NewRegistry()
	var names []string
	for _, s := range skills {
		if err := reg.Register(s); err != nil {
			t.Fatalf("register: %v", err)
		}
		names = append(names, s.Name())
	}
	fake := &fakeLLM{rounds: rounds}
	r := NewRunner(fake, reg, "test-model")
	a := store.AgentRow{ID: "ops", Name: "test", SystemPrompt: "sys", Skills: names}
	history := []llm.Message{{Role: llm.RoleUser, Content: "hi"}}
	var deltas string
	answer, err := r.Run(context.Background(), a, history, Events{
		OnDelta: func(d string) { deltas += d },
	})
	return answer, r, err
}

func TestRunnerToolCallThenAnswer(t *testing.T) {
	fs := &fakeSkill{name: "ops_echo"}
	answer, _, err := runCase(t, []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "ops_echo", Arguments: `{"q":1}`}}},
		{Role: llm.RoleAssistant, Content: "final answer"},
	}, fs)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if answer != "final answer" {
		t.Fatalf("answer = %q", answer)
	}
	if fs.lastArg != `{"q":1}` {
		t.Fatalf("skill args = %q", fs.lastArg)
	}
}

func TestRunnerUnknownToolReportsFailure(t *testing.T) {
	answer, _, err := runCase(t, []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "ghost_tool", Arguments: `{}`}}},
		{Role: llm.RoleAssistant, Content: "handled"},
	}, &fakeSkill{name: "ops_echo"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if answer != "handled" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestRunnerMissingModel(t *testing.T) {
	reg := skill.NewRegistry()
	r := NewRunner(&fakeLLM{}, reg, "")
	a := store.AgentRow{ID: "ops", Skills: []string{}}
	_, err := r.Run(context.Background(), a, []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, Events{})
	if err == nil {
		t.Fatal("expected model-missing error")
	}
}

// blockingLLM 模拟卡住的流：阻塞直到 ctx 终止，然后返回 ctx 错误
// （与真实适配器在 ctx 取消后的行为一致——错误原样透传，不被吞掉）。
type blockingLLM struct{}

func (blockingLLM) ChatStream(ctx context.Context, _ string, _ []llm.Message, _ []llm.ToolSpec, _ func(string) error) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRunnerContextCancel Runner 必须对 ctx 取消敏感：
// 取消后回合立即以取消错误结束，而不是继续阻塞。
func TestRunnerContextCancel(t *testing.T) {
	reg := skill.NewRegistry()
	r := NewRunner(blockingLLM{}, reg, "test-model")
	a := store.AgentRow{ID: "ops", Skills: []string{}}
	ctx, cancel := context.WithCancel(context.Background())

	type result struct {
		answer string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		answer, err := r.Run(ctx, a, []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, Events{})
		done <- result{answer, err}
	}()

	time.Sleep(20 * time.Millisecond) // 让 Run 进入阻塞的 ChatStream
	cancel()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", res.err)
		}
		if res.answer != "" {
			t.Fatalf("answer = %q, want empty on cancel", res.answer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
