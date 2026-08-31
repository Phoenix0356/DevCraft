package appsvc

import (
	"strings"
	"testing"
)

// TestListAgentsDetail 智能体管理弹窗的数据源契约：
// 内置运维 Agent 在列，已装配技能与可选技能全集齐全，
// 分类标记全部为内置（本期无自定义技能），描述实时非空。
func TestListAgentsDetail(t *testing.T) {
	svc := newTestService(t)
	registerProdSkills(t, svc)

	details, err := svc.ListAgentsDetail()
	if err != nil {
		t.Fatalf("ListAgentsDetail: %v", err)
	}
	var ops int = -1
	for i, d := range details {
		if d.ID == "ops" {
			ops = i
		}
	}
	if ops < 0 {
		t.Fatalf("builtin ops agent missing from details: %+v", details)
	}
	d := details[ops]
	if !d.Builtin || d.Name != "运维 Agent" || d.SystemPrompt == "" {
		t.Fatalf("ops detail unexpected: %+v", d)
	}

	// 已装配：播种的 4 个技能（agent_skills 按名有序）
	wantAssembled := []string{"deploy_run_flow", "ops_analyze_logs", "ops_container_stats", "ops_list_containers"}
	if len(d.Skills) != len(wantAssembled) {
		t.Fatalf("assembled = %v, want %v", d.Skills, wantAssembled)
	}
	for i, want := range wantAssembled {
		s := d.Skills[i]
		if s.Name != want || !s.Builtin || strings.TrimSpace(s.Description) == "" {
			t.Fatalf("assembled[%d] = %+v, want name=%s builtin with description", i, s, want)
		}
	}

	// 可选全集：注册表全量（4 个生产技能），全部内置标记
	if len(d.Available) != 4 {
		t.Fatalf("available skills = %d, want 4", len(d.Available))
	}
	for _, s := range d.Available {
		if !s.Builtin {
			t.Fatalf("available skill %s should be marked builtin", s.Name)
		}
	}
}

// TestSaveAgentValidationAndPersistence 信息保存：名称非空校验、
// 不存在的智能体拒绝、正常保存后可读回。
func TestSaveAgentValidationAndPersistence(t *testing.T) {
	svc := newTestService(t)

	if err := svc.SaveAgent("ops", "   ", "m", "p"); err == nil || !strings.Contains(err.Error(), "名称不能为空") {
		t.Fatalf("empty name should be rejected, got: %v", err)
	}
	if err := svc.SaveAgent("ghost", "名字", "", ""); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing agent should be rejected, got: %v", err)
	}

	// 正常保存：名称/模型/提示词；系统提示词允许为空
	if err := svc.SaveAgent("ops", "我的运维助手", " qwen-plus ", "全新人设"); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}
	if err := svc.SaveAgent("bare", "裸智能体", "", ""); err != nil {
		t.Fatalf("SaveAgent with empty prompt/model: %v", err)
	}

	details, err := svc.ListAgentsDetail()
	if err != nil {
		t.Fatalf("ListAgentsDetail: %v", err)
	}
	for _, d := range details {
		switch d.ID {
		case "ops":
			if d.Name != "我的运维助手" || d.Model != "qwen-plus" || d.SystemPrompt != "全新人设" {
				t.Fatalf("ops not persisted: %+v", d)
			}
			if !d.Builtin {
				t.Fatal("builtin flag must survive SaveAgent")
			}
		case "bare":
			if d.Name != "裸智能体" || d.Model != "" || d.SystemPrompt != "" {
				t.Fatalf("bare not persisted: %+v", d)
			}
		}
	}
}

// TestSetAgentSkillsViaService 编排层装配入口：整组替换生效、
// 空装配合法（纯聊天）、未知技能拒绝。
func TestSetAgentSkillsViaService(t *testing.T) {
	svc := newTestService(t)
	registerProdSkills(t, svc)

	if err := svc.SetAgentSkills("ops", []string{"ops_list_containers", "ops_analyze_logs"}); err != nil {
		t.Fatalf("SetAgentSkills: %v", err)
	}
	got, err := svc.agents.Get("ops")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Skills) != 2 {
		t.Fatalf("skills = %v, want 2 entries", got.Skills)
	}

	// 空装配：退化为纯聊天智能体
	if err := svc.SetAgentSkills("ops", nil); err != nil {
		t.Fatalf("empty assembly should be allowed: %v", err)
	}
	got, _ = svc.agents.Get("ops")
	if len(got.Skills) != 0 {
		t.Fatalf("skills = %v, want empty", got.Skills)
	}

	// 未知技能拒绝
	if err := svc.SetAgentSkills("ops", []string{"ghost_tool"}); err == nil || !strings.Contains(err.Error(), "ghost_tool") {
		t.Fatalf("unknown skill should be rejected, got: %v", err)
	}
}
