package store

import (
	"path/filepath"
	"testing"
)

func TestSessionAndMessageRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	sess := Session{ID: "s1", Title: "新会话", AgentID: "ops"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.AppendMessage(Message{SessionID: "s1", Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := st.AppendMessage(Message{SessionID: "s1", Role: "assistant", Content: "hi"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	msgs, err := st.Messages("s1", 0)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Content != "hi" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}

	got, err := st.GetSession("s1")
	if err != nil || got.AgentID != "ops" {
		t.Fatalf("get session: %+v err=%v", got, err)
	}
	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetSession("s1"); err == nil {
		t.Fatal("expected not-found after delete")
	}
}

func TestAgentUpsertAndSettings(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	a := AgentRow{ID: "ops", Name: "运维 Agent", SystemPrompt: "p", Builtin: true, Skills: []string{"ops.a", "ops.b"}}
	if err := st.UpsertAgent(a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// upsert replaces skill list
	a.Skills = []string{"ops.a"}
	if err := st.UpsertAgent(a); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	got, err := st.GetAgent("ops")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(got.Skills) != 1 || got.Skills[0] != "ops.a" || !got.Builtin {
		t.Fatalf("agent mismatch: %+v", got)
	}

	if err := st.SetSetting("k", "v1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.SetSetting("k", "v2"); err != nil {
		t.Fatalf("set2: %v", err)
	}
	v, ok, err := st.GetSetting("k")
	if err != nil || !ok || v != "v2" {
		t.Fatalf("setting = %q ok=%v err=%v", v, ok, err)
	}
	if _, ok, _ := st.GetSetting("missing"); ok {
		t.Fatal("expected missing key to be absent")
	}
}

// TestDeployFlowCRUD 部署流程的新建/查重/更新/按名查找/删除。
func TestDeployFlowCRUD(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	f := DeployFlow{
		Name:        "web-deploy",
		Description: "部署 web 服务",
		Target:      TargetSSH,
		Params:      []FlowParam{{Name: "version", Desc: "版本号", Pattern: `^\d+\.\d+\.\d+$`}},
		Steps:       []string{"docker pull web:{{version}}", "docker restart web"},
	}
	if err := st.SaveDeployFlow(&f); err != nil {
		t.Fatalf("save: %v", err)
	}
	if f.ID == 0 {
		t.Fatal("expected auto-increment id filled back")
	}

	// 名称唯一：同名新建必须拒绝
	dup := DeployFlow{Name: "web-deploy", Target: TargetLocal, Steps: []string{"echo hi"}}
	if err := st.SaveDeployFlow(&dup); err == nil {
		t.Fatal("duplicate flow name should be rejected")
	}

	// 更新（同名覆盖自己不报冲突）
	f.Description = "部署 web 服务（灰度）"
	f.Steps = append(f.Steps, "echo done {{version}}")
	if err := st.SaveDeployFlow(&f); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := st.GetDeployFlowByName("web-deploy")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if got.Description != "部署 web 服务（灰度）" || len(got.Steps) != 3 {
		t.Fatalf("flow mismatch after update: %+v", got)
	}
	if len(got.Params) != 1 || got.Params[0].Pattern == "" {
		t.Fatalf("params round-trip failed: %+v", got.Params)
	}

	// 列表
	list, err := st.ListDeployFlows()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	if err := st.DeleteDeployFlow(f.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetDeployFlow(f.ID); err == nil {
		t.Fatal("expected not-found after delete")
	}
}

// TestDeployHistoryRoundTrip 执行历史：开始时插入（只有 started_at），结束时回填终态。
func TestDeployHistoryRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	h := DeployHistory{
		FlowID:    1,
		FlowName:  "web-deploy",
		Params:    map[string]string{"version": "1.2.3"},
		SessionID: "sess-1",
		Target:    "ssh://root@10.0.0.5:22",
	}
	if err := st.InsertDeployHistory(&h); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if h.ID == 0 || h.StartedAt == 0 {
		t.Fatalf("insert should fill id and started_at: %+v", h)
	}

	h.Status = DeployFailed
	h.Detail = "步骤 1/3 成功；步骤 2/3 失败: 远端命令失败"
	if err := st.UpdateDeployHistory(h); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := st.GetDeployHistory(h.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != DeployFailed || got.FinishedAt == 0 || got.Params["version"] != "1.2.3" {
		t.Fatalf("history mismatch: %+v", got)
	}
	if got.FlowName != "web-deploy" || got.SessionID != "sess-1" {
		t.Fatalf("history snapshot fields mismatch: %+v", got)
	}
}
