package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"DevCraft/internal/llm"
	"DevCraft/internal/skill"
	"DevCraft/internal/store"
)

// newTestRegistry 临时 SQLite + 独立技能注册表（返回注册表便于按需注册假技能）。
func newTestRegistry(t *testing.T) (*Registry, *skill.Registry) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	skills := skill.NewRegistry()
	return NewRegistry(st, skills), skills
}

// registerFakes 注册若干假技能并返回其名字列表。
func registerFakes(t *testing.T, reg *skill.Registry, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := reg.Register(&fakeSkill{name: n}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
}

// TestSeedDefaultsIdempotentPreservesUserEdits 播种幂等不覆盖用户编辑：
// 模拟"用户改了名称/模型/提示词/装配 → 应用重启（再次 SeedDefaults）"，
// 重启后所有用户数据原样保留，内置标记不被篡改。
func TestSeedDefaultsIdempotentPreservesUserEdits(t *testing.T) {
	reg, skills := newTestRegistry(t)
	registerFakes(t, skills, "ops_list_containers", "ops_container_stats")

	// 首次启动：出厂预装
	if err := reg.SeedDefaults(); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	seeded, err := reg.Get(BuiltinOpsAgentID)
	if err != nil {
		t.Fatalf("get seeded: %v", err)
	}
	if !seeded.Builtin || seeded.Name != "运维 Agent" {
		t.Fatalf("seeded agent unexpected: %+v", seeded)
	}

	// 用户编辑：改名/换模型/改提示词/减装配
	seeded.Name = "我的运维助手"
	seeded.Model = "qwen-plus"
	seeded.SystemPrompt = "用户自定义的人设提示词"
	if err := reg.Update(seeded); err != nil {
		t.Fatalf("user edit: %v", err)
	}
	if err := reg.SetSkills(BuiltinOpsAgentID, []string{"ops_list_containers"}); err != nil {
		t.Fatalf("user assembly edit: %v", err)
	}

	// 模拟重启：再次播种
	if err := reg.SeedDefaults(); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	got, err := reg.Get(BuiltinOpsAgentID)
	if err != nil {
		t.Fatalf("get after reseed: %v", err)
	}
	if got.Name != "我的运维助手" || got.Model != "qwen-plus" || got.SystemPrompt != "用户自定义的人设提示词" {
		t.Fatalf("reseed overwrote user info: %+v", got)
	}
	if len(got.Skills) != 1 || got.Skills[0] != "ops_list_containers" {
		t.Fatalf("reseed overwrote user assembly: %v", got.Skills)
	}
	if !got.Builtin {
		t.Fatal("builtin flag lost after reseed")
	}
}

// TestUpdateRejectsBuiltinFlagChange 内置标记不可翻转；不存在的 Agent 拒绝更新。
func TestUpdateRejectsBuiltinFlagChange(t *testing.T) {
	reg, _ := newTestRegistry(t)
	if err := reg.SeedDefaults(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tampered, err := reg.Get(BuiltinOpsAgentID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	tampered.Builtin = false // 试图摘掉内置身份
	if err := reg.Update(tampered); err == nil || !strings.Contains(err.Error(), "内置标记") {
		t.Fatalf("builtin flip should be rejected, got: %v", err)
	}
	// 库中记录不受影响
	got, _ := reg.Get(BuiltinOpsAgentID)
	if !got.Builtin {
		t.Fatal("builtin flag changed despite rejection")
	}

	if err := reg.Update(store.AgentRow{ID: "ghost", Name: "x"}); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("update of missing agent should fail with not-found, got: %v", err)
	}
}

// TestSetSkillsWholeGroupReplace 装配整组替换：增、减、清空（纯聊天）三种路径。
func TestSetSkillsWholeGroupReplace(t *testing.T) {
	reg, skills := newTestRegistry(t)
	registerFakes(t, skills, "alpha_one", "beta_two", "gamma_three")
	if err := reg.Upsert(store.AgentRow{ID: "custom", Name: "自定义", SystemPrompt: "p"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	cases := []struct {
		name string
		set  []string
		want []string // 期望装配（按名排序后）
	}{
		{"add", []string{"alpha_one", "beta_two"}, []string{"alpha_one", "beta_two"}},
		{"shrink", []string{"gamma_three"}, []string{"gamma_three"}},
		{"empty", []string{}, nil},
	}
	for _, c := range cases {
		if err := reg.SetSkills("custom", c.set); err != nil {
			t.Fatalf("%s: SetSkills: %v", c.name, err)
		}
		got, err := reg.Get("custom")
		if err != nil {
			t.Fatalf("%s: get: %v", c.name, err)
		}
		if len(got.Skills) != len(c.want) {
			t.Fatalf("%s: skills = %v, want %v", c.name, got.Skills, c.want)
		}
		for i, w := range c.want {
			if got.Skills[i] != w {
				t.Fatalf("%s: skills = %v, want %v", c.name, got.Skills, c.want)
			}
		}
	}
}

// TestSetSkillsRejectsUnknownSkill 未知技能名拒绝装配，且原装配不被破坏。
func TestSetSkillsRejectsUnknownSkill(t *testing.T) {
	reg, skills := newTestRegistry(t)
	registerFakes(t, skills, "alpha_one")
	if err := reg.Upsert(store.AgentRow{ID: "custom", Name: "自定义", Skills: []string{"alpha_one"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	err := reg.SetSkills("custom", []string{"alpha_one", "ghost_tool"})
	if err == nil || !strings.Contains(err.Error(), "ghost_tool") {
		t.Fatalf("unknown skill should be rejected, got: %v", err)
	}
	got, _ := reg.Get("custom")
	if len(got.Skills) != 1 || got.Skills[0] != "alpha_one" {
		t.Fatalf("assembly corrupted by rejected set: %v", got.Skills)
	}

	if err := reg.SetSkills("ghost-agent", []string{"alpha_one"}); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("SetSkills on missing agent should fail, got: %v", err)
	}
}

// TestDeleteBuiltinRejected 内置智能体删除被拒绝（后端保护，前端本期无入口）。
func TestDeleteBuiltinRejected(t *testing.T) {
	reg, _ := newTestRegistry(t)
	if err := reg.SeedDefaults(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := reg.Delete(BuiltinOpsAgentID)
	if err == nil || !strings.Contains(err.Error(), "不可删除") {
		t.Fatalf("builtin delete should be rejected, got: %v", err)
	}
	if _, err := reg.Get(BuiltinOpsAgentID); err != nil {
		t.Fatalf("builtin agent must survive rejected delete: %v", err)
	}
}

// TestDeleteCustomAgent 自定义（非内置）智能体可删除，装配随之清理。
func TestDeleteCustomAgent(t *testing.T) {
	reg, skills := newTestRegistry(t)
	registerFakes(t, skills, "alpha_one")
	if err := reg.Upsert(store.AgentRow{ID: "custom", Name: "自定义", Skills: []string{"alpha_one"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := reg.Delete("custom"); err != nil {
		t.Fatalf("delete custom: %v", err)
	}
	if _, err := reg.Get("custom"); err == nil {
		t.Fatal("expected not-found after delete")
	}
	if err := reg.Delete("ghost"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("delete of missing agent should fail, got: %v", err)
	}
}

// toolsCaptureLLM 记录本回合收到的工具清单后立即给出最终回答。
type toolsCaptureLLM struct {
	tools []llm.ToolSpec
}

func (c *toolsCaptureLLM) ChatStream(_ context.Context, _ string, _ []llm.Message, tools []llm.ToolSpec, _ func(string) error) (*llm.Response, error) {
	c.tools = tools
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}, nil
}

// TestRunnerToolsFollowAssembly 装配变更后下一回合的工具清单即变化：
// SetSkills 减掉一个技能后，Runner 发给 LLM 的 tools 只剩装配的那一个。
func TestRunnerToolsFollowAssembly(t *testing.T) {
	reg, skills := newTestRegistry(t)
	registerFakes(t, skills, "alpha_one", "beta_two")
	if err := reg.Upsert(store.AgentRow{ID: "custom", Name: "自定义", SystemPrompt: "p", Skills: []string{"alpha_one", "beta_two"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// 减装配：只留 beta_two
	if err := reg.SetSkills("custom", []string{"beta_two"}); err != nil {
		t.Fatalf("SetSkills: %v", err)
	}

	a, err := reg.Get("custom") // 模拟 appsvc 每回合实时取 Agent 定义
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	capture := &toolsCaptureLLM{}
	runner := NewRunner(capture, skills, "test-model")
	if _, err := runner.Run(context.Background(), a, []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, Events{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(capture.tools) != 1 || capture.tools[0].Name != "beta_two" {
		t.Fatalf("tools = %+v, want exactly [beta_two]", capture.tools)
	}
}
