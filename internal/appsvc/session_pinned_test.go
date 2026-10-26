package appsvc

import (
	"strings"
	"testing"
)

// TestSetSessionPinned 置顶 RPC 面：bool→0/1 的转换在这一层完成，store 只见整数；
// 成功路径落库；会话不存在与空 id 都给中文错误（用户显式动作必须报错，不静默降级）。
func TestSetSessionPinned(t *testing.T) {
	svc := newTestService(t)
	sess := newBareSession(t, svc)

	// 置顶：true → 1
	if err := svc.SetSessionPinned(sess.ID, true); err != nil {
		t.Fatalf("SetSessionPinned(true): %v", err)
	}
	got, err := svc.store.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Pinned != 1 {
		t.Fatalf("置顶后 pinned = %d, want 1", got.Pinned)
	}
	// 置顶时间由 store 从 pinned 派生（本层不传时间戳），落库必须是真时间
	if got.PinnedAt <= 0 {
		t.Fatalf("置顶后 pinnedAt = %d, want > 0", got.PinnedAt)
	}
	// 定向写入：标题与 Agent 绑定不受影响
	if got.AgentID != "bare" || got.Title != "新会话" {
		t.Fatalf("置顶不该动其他列: %+v", got)
	}

	// 取消置顶：false → 0（pinnedAt 同步归零）
	if err := svc.SetSessionPinned(sess.ID, false); err != nil {
		t.Fatalf("SetSessionPinned(false): %v", err)
	}
	if got, _ := svc.store.GetSession(sess.ID); got.Pinned != 0 || got.PinnedAt != 0 {
		t.Fatalf("取消置顶后 pinned/pinnedAt = %d/%d, want 0/0", got.Pinned, got.PinnedAt)
	}

	// 排序由后端给出（前端从不本地排序）：置顶项必须在列表最前
	other := newBareSession(t, svc)
	if err := svc.SetSessionPinned(other.ID, true); err != nil {
		t.Fatalf("SetSessionPinned(other): %v", err)
	}
	list, err := svc.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != other.ID || list[0].Pinned != 1 {
		t.Fatalf("置顶后会不在最前: %+v", list)
	}

	// 不存在的会话：中文错误
	if err := svc.SetSessionPinned("no-such", true); err == nil ||
		!strings.Contains(err.Error(), "会话不存在或已被删除") {
		t.Fatalf("SetSessionPinned(missing) err = %v, want 会话不存在或已被删除", err)
	}

	// 空/纯空白 id：中文错误
	for _, bad := range []string{"", "   ", "\t"} {
		if err := svc.SetSessionPinned(bad, true); err == nil ||
			!strings.Contains(err.Error(), "会话 id 不能为空") {
			t.Fatalf("SetSessionPinned(%q) err = %v, want 会话 id 不能为空", bad, err)
		}
	}
}
