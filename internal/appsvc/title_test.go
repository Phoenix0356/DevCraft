package appsvc

import (
	"strings"
	"testing"
)

// TestRenameSession 手动改名：非空校验（中文错误）、超长截断（truncate 同款语义：
// 30 字 + 省略号）、写回（只动 title，agent_id 保持不变）。
func TestRenameSession(t *testing.T) {
	svc := newTestService(t)
	sess := newBareSession(t, svc)

	// 空/纯空白标题：中文错误
	for _, bad := range []string{"", "   ", "\t\n"} {
		if err := svc.RenameSession(sess.ID, bad); err == nil || !strings.Contains(err.Error(), "标题不能为空") {
			t.Fatalf("RenameSession(%q) err = %v, want 标题不能为空", bad, err)
		}
	}

	// 不存在的会话：透传 store 的 not-found 错误
	if err := svc.RenameSession("no-such", "x"); err == nil {
		t.Fatal("RenameSession(missing session) should fail")
	}

	// 正常改名：TrimSpace 后写回，agent_id 不受影响
	if err := svc.RenameSession(sess.ID, "  我的运维会话  "); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	got, err := svc.store.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Title != "我的运维会话" {
		t.Fatalf("after rename: title=%q, want 我的运维会话", got.Title)
	}
	if got.AgentID != "bare" {
		t.Fatalf("rename 不该动 agent_id: %q", got.AgentID)
	}

	// 超长标题：复用 truncate——按 rune 截断到 30 字并加省略号（与占位标题同款语义）
	long := strings.Repeat("长", 40)
	if err := svc.RenameSession(sess.ID, long); err != nil {
		t.Fatalf("RenameSession(long): %v", err)
	}
	got, _ = svc.store.GetSession(sess.ID)
	if want := strings.Repeat("长", 30) + "…"; got.Title != want {
		t.Fatalf("truncated title = %q, want %q", got.Title, want)
	}
}
