package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// forceUpdatedAt 把会话的 updated_at 钉成指定值。改名、切 Agent 这些定向写入会刷新
// updated_at，置顶则刻意不刷（置顶不是"活跃"，见 SetSessionPinned）——但同毫秒的
// 创建/写入仍会让排序断言不可预测，所以测试里显式造出一条确定的活跃时间阶梯，
// 只验证 ORDER BY 本身的语义；阶梯同时是"置顶写入不脏活跃时间"的断言面。
func forceUpdatedAt(t *testing.T, st *Store, id string, ts int64) {
	t.Helper()
	if _, err := st.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, ts, id); err != nil {
		t.Fatalf("设置 %s 的 updated_at: %v", id, err)
	}
}

// forcePinnedAt 把会话的 pinned_at 钉成指定值（pinned 标记本身保持由 SetSessionPinned 写）。
// 同毫秒的置顶写入同样会让 ORDER BY 断言不可预测（理由与上面一致），所以置顶组的排序
// 用例显式造一条"置顶时间阶梯"，只验证 ORDER BY 本身的语义；pinned_at 由谁算出来
// （由 pinned 派生）另有专门用例覆盖。
func forcePinnedAt(t *testing.T, st *Store, id string, ts int64) {
	t.Helper()
	if _, err := st.db.Exec(`UPDATE sessions SET pinned_at = ? WHERE id = ?`, ts, id); err != nil {
		t.Fatalf("设置 %s 的 pinned_at: %v", id, err)
	}
}

// orderOf 取会话列表的 id 顺序（逗号连接，排序断言用）。
func orderOf(list []Session) string {
	ids := make([]string, 0, len(list))
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ",")
}

// TestMigrateAddsPinnedColumnToLegacyDB 存量库（sessions 还没有 pinned / pinned_at 两列）：
// 启动必须把两列都 ALTER 出来，旧行都取 DEFAULT 0（未置顶、无置顶时间），其他数据零变化；
// 重复启动探测命中 → 干净跳过，绝不重复加列。
func TestMigrateAddsPinnedColumnToLegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`
CREATE TABLE sessions (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL DEFAULT '',
	agent_id   TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
INSERT INTO sessions (id, title, agent_id, created_at, updated_at) VALUES ('old', '旧会话', 'ops', 1, 1);
`); err != nil {
		t.Fatalf("建老表: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open 缺 pinned 列的存量库: %v", err)
	}
	cols, err := st.sessionColumns()
	if err != nil {
		t.Fatalf("列清单: %v", err)
	}
	if n := countColumn(cols, "pinned"); n != 1 {
		t.Fatalf("pinned 列出现 %d 次，want 1（cols=%v）", n, cols)
	}
	if n := countColumn(cols, "pinned_at"); n != 1 {
		t.Fatalf("pinned_at 列出现 %d 次，want 1（cols=%v）", n, cols)
	}
	got, err := st.GetSession("old")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Pinned != 0 {
		t.Fatalf("旧行 pinned = %d，want 0（DEFAULT 落位）", got.Pinned)
	}
	if got.PinnedAt != 0 {
		t.Fatalf("旧行 pinned_at = %d，want 0（DEFAULT 落位）", got.PinnedAt)
	}
	if got.Title != "旧会话" || got.AgentID != "ops" {
		t.Fatalf("加列迁移改了别的数据: %+v", got)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(path) // 第二次启动：列已存在 → 探测命中，不报错也不重复 ALTER
	if err != nil {
		t.Fatalf("第二次 Open: %v", err)
	}
	defer st2.Close()
	cols, err = st2.sessionColumns()
	if err != nil {
		t.Fatalf("第二次列清单: %v", err)
	}
	if n := countColumn(cols, "pinned"); n != 1 {
		t.Fatalf("重复启动后 pinned 列出现 %d 次，want 1（cols=%v）", n, cols)
	}
	if n := countColumn(cols, "pinned_at"); n != 1 {
		t.Fatalf("重复启动后 pinned_at 列出现 %d 次，want 1（cols=%v）", n, cols)
	}
}

// TestListSessionsPinnedFirst 排序：置顶组整体在前（pinned 权重压过一切时间），
// 未置顶组按 updated_at 倒序。取消置顶后该会话回到它活跃时间本该在的位置
// （pinned 只是前置权重，置顶写入也不刷活跃时间）。
// 置顶组内怎么排（按置顶时间倒序）另有 TestListSessionsPinnedGroupByPinnedAt 专门钉住。
func TestListSessionsPinnedFirst(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	for _, id := range []string{"s1", "s2", "s3"} {
		if err := st.CreateSession(Session{ID: id, Title: "会话 " + id, AgentID: "ops"}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	base := time.Now().UnixMilli()
	for i, id := range []string{"s1", "s2", "s3"} {
		forceUpdatedAt(t, st, id, base+int64((3-i)*1000)) // s1 > s2 > s3
	}

	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := orderOf(list); got != "s1,s2,s3" {
		t.Fatalf("无置顶时排序 = %q, want s1,s2,s3", got)
	}

	// 置顶活跃时间最靠后的 s3：pinned 权重必须压过 updated_at
	if wrote, err := st.SetSessionPinned("s3", 1); err != nil || !wrote {
		t.Fatalf("SetSessionPinned(s3,1) = (%v, %v), want (true, nil)", wrote, err)
	}
	list, err = st.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := orderOf(list); got != "s3,s1,s2" {
		t.Fatalf("置顶 s3 后排序 = %q, want s3,s1,s2", got)
	}
	if list[0].Pinned != 1 || list[1].Pinned != 0 {
		t.Fatalf("ListSessions 的 pinned 字段 = %d/%d, want 1/0（前端分组靠它）", list[0].Pinned, list[1].Pinned)
	}
	if list[0].PinnedAt <= 0 {
		t.Fatalf("置顶项的 pinnedAt = %d, want > 0（置顶时间随标记一起落库）", list[0].PinnedAt)
	}

	// 取消置顶 → 回到 updated_at 决定的历史位置（置顶写入不刷活跃时间）
	if wrote, err := st.SetSessionPinned("s3", 0); err != nil || !wrote {
		t.Fatalf("SetSessionPinned(s3,0) = (%v, %v), want (true, nil)", wrote, err)
	}
	list, err = st.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := orderOf(list); got != "s1,s2,s3" {
		t.Fatalf("取消置顶后排序 = %q, want s1,s2,s3", got)
	}
	wantLadder := map[string]int64{"s1": base + 3000, "s2": base + 2000, "s3": base + 1000}
	for _, s := range list { // 钉住「置顶不是活跃」：置顶/取消两次写入后 updated_at 阶梯原样不动
		if s.UpdatedAt != wantLadder[s.ID] {
			t.Fatalf("%s 的 updated_at = %d, want %d（置顶写入不该刷新活跃时间）", s.ID, s.UpdatedAt, wantLadder[s.ID])
		}
		if s.PinnedAt != 0 { // 全部取消置顶后没有一行该留着置顶时间（否则它会压过别人的活跃排序）
			t.Fatalf("%s 的 pinned_at = %d, want 0（取消置顶必须清零）", s.ID, s.PinnedAt)
		}
	}
}

// TestListSessionsPinnedGroupByPinnedAt 置顶组内按「置顶时间」倒序：
// 先置顶最活跃的 s1，再置顶活跃时间最靠后的 s3 —— 刚钉的 s3 必须排在 s1 前面。
// 若置顶组内改成按活跃时间排（ORDER BY 里没有 pinned_at 这一级），这里必然给出
// s1,s3,s2，所以这条断言只钉住「置顶组内看置顶时间」这一语义，别的设计给不出这个结果。
// 两个 pinned_at 用 forcePinnedAt 钉成确定阶梯（同毫秒写入会让 ORDER BY 不可预测）。
func TestListSessionsPinnedGroupByPinnedAt(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	for _, id := range []string{"s1", "s2", "s3"} {
		if err := st.CreateSession(Session{ID: id, Title: "会话 " + id, AgentID: "ops"}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	base := time.Now().UnixMilli()
	for i, id := range []string{"s1", "s2", "s3"} {
		forceUpdatedAt(t, st, id, base+int64((3-i)*1000)) // 活跃阶梯 s1 > s2 > s3
	}

	if _, err := st.SetSessionPinned("s1", 1); err != nil {
		t.Fatalf("SetSessionPinned(s1,1): %v", err)
	}
	forcePinnedAt(t, st, "s1", base+10000) // 先被置顶
	if _, err := st.SetSessionPinned("s3", 1); err != nil {
		t.Fatalf("SetSessionPinned(s3,1): %v", err)
	}
	forcePinnedAt(t, st, "s3", base+20000) // 后被置顶（活跃时间却最靠后）

	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := orderOf(list); got != "s3,s1,s2" {
		t.Fatalf("置顶时间更晚的 s3 没压过更活跃的 s1：排序 = %q, want s3,s1,s2", got)
	}
	if list[0].Pinned != 1 || list[1].Pinned != 1 || list[2].Pinned != 0 {
		t.Fatalf("置顶分组字段 = %d/%d/%d, want 1/1/0", list[0].Pinned, list[1].Pinned, list[2].Pinned)
	}
	if list[0].PinnedAt <= list[1].PinnedAt {
		t.Fatalf("置顶组内 pinned_at 不是倒序：%d <= %d", list[0].PinnedAt, list[1].PinnedAt)
	}

	// 取消较晚置顶的那个 → 剩下的置顶项不受影响，被取消者回到活跃时间决定的位置
	// （同时证明取消写入把 pinned_at 清零了：留着 base+20000 的话 s3 会仍排在 s1 前）
	if _, err := st.SetSessionPinned("s3", 0); err != nil {
		t.Fatalf("SetSessionPinned(s3,0): %v", err)
	}
	list, err = st.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := orderOf(list); got != "s1,s2,s3" {
		t.Fatalf("取消 s3 置顶后排序 = %q, want s1,s2,s3", got)
	}
}

// TestSetSessionPinned 定向写入：0/1 都生效、可幂等重写、不存在的 id 是 (false, nil)
// （不是 error，也不 panic）；同时确认它只碰 pinned 与 pinned_at 两列，
// 以及 pinned=1 ⇔ pinned_at>0 这个不变量（pinned_at 由 store 从 pinned 派生）。
func TestSetSessionPinned(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.CreateSession(Session{ID: "s1", Title: "新会话", AgentID: "ops"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 新建会话必然是不置顶态（pinned / pinned_at 都不在 INSERT 列里，由列 DEFAULT 落位）
	got, err := st.GetSession("s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Pinned != 0 {
		t.Fatalf("新建会话 pinned = %d, want 0", got.Pinned)
	}
	if got.PinnedAt != 0 {
		t.Fatalf("新建会话 pinned_at = %d, want 0", got.PinnedAt)
	}

	beforePin := time.Now().UnixMilli()
	if wrote, err := st.SetSessionPinned("s1", 1); err != nil || !wrote {
		t.Fatalf("置为置顶 = (%v, %v), want (true, nil)", wrote, err)
	}
	pinned, _ := st.GetSession("s1")
	if pinned.Pinned != 1 {
		t.Fatalf("置顶后 pinned = %d, want 1", pinned.Pinned)
	}
	// 不变量 pinned=1 ⇒ pinned_at>0，且时间戳就是刚那次写入落的（不是残留、也不是 0）
	if pinned.PinnedAt < beforePin {
		t.Fatalf("置顶后 pinned_at = %d, want >= %d（置顶时间由 store 从 pinned 派生）", pinned.PinnedAt, beforePin)
	}
	// 幂等：再写一次 1 仍是置顶态
	if wrote, err := st.SetSessionPinned("s1", 1); err != nil || !wrote {
		t.Fatalf("重复置顶 = (%v, %v), want (true, nil)", wrote, err)
	}
	if again, _ := st.GetSession("s1"); again.Pinned != 1 {
		t.Fatalf("重复置顶后 pinned = %d, want 1", again.Pinned)
	}
	// 重复置顶要刷新成"更晚"的置顶时间：先把列钉成一个远古值（等价于"这行很久以前置顶过"），
	// 直接比较两次相邻写入拿同毫秒的运气没法断言严格变大。
	forcePinnedAt(t, st, "s1", 1)
	rePin := time.Now().UnixMilli()
	if _, err := st.SetSessionPinned("s1", 1); err != nil {
		t.Fatalf("重复置顶: %v", err)
	}
	refreshed, _ := st.GetSession("s1")
	if refreshed.PinnedAt <= 1 {
		t.Fatalf("重复置顶后 pinned_at = %d, want 被刷新（远古置顶时间 1 该被盖掉）", refreshed.PinnedAt)
	}
	if refreshed.PinnedAt < rePin {
		t.Fatalf("重复置顶后 pinned_at = %d, want >= %d（刷新成当前时间）", refreshed.PinnedAt, rePin)
	}
	// 取消置顶
	if wrote, err := st.SetSessionPinned("s1", 0); err != nil || !wrote {
		t.Fatalf("取消置顶 = (%v, %v), want (true, nil)", wrote, err)
	}
	unpinned, _ := st.GetSession("s1")
	if unpinned.Pinned != 0 {
		t.Fatalf("取消置顶后 pinned = %d, want 0", unpinned.Pinned)
	}
	// 不变量另一半：pinned=0 ⇒ pinned_at==0（留着旧置顶时间会让它下次置顶前排错位置）
	if unpinned.PinnedAt != 0 {
		t.Fatalf("取消置顶后 pinned_at = %d, want 0", unpinned.PinnedAt)
	}

	// 定向写入的列所有权：title/title_source/agent_id/updated_at 都不该被置顶写入改动，
	// 被改动的只有它自己拥有的 pinned 与 pinned_at 两列。
	before, _ := st.GetSession("s1")
	if _, err := st.UpdateSessionTitle("s1", "AI 标题"); err != nil {
		t.Fatalf("ai 标题: %v", err)
	}
	withTitle, _ := st.GetSession("s1")
	if _, err := st.SetSessionPinned("s1", 1); err != nil {
		t.Fatalf("set pinned: %v", err)
	}
	after, _ := st.GetSession("s1")
	if after.Title != withTitle.Title || after.TitleSource != withTitle.TitleSource ||
		after.AgentID != withTitle.AgentID || after.UpdatedAt != withTitle.UpdatedAt {
		t.Fatalf("置顶写入污染了其他列: before=%+v after=%+v", before, after)
	}
	if after.Pinned != 1 || after.PinnedAt <= 0 {
		t.Fatalf("置顶写入没写成自己那两列: %+v", after)
	}

	// 不存在的会话：写 0 行，不是错误
	if wrote, err := st.SetSessionPinned("no-such", 1); err != nil || wrote {
		t.Fatalf("missing id = (%v, %v), want (false, nil)", wrote, err)
	}
}

// TestPinnedSurvivesOtherTargetedWrites 反向不污染：其余定向写入（切 Agent、占位标题、
// AI 标题、手动改名）都不得改写 pinned 与 pinned_at —— 它们各自只拥有自己的列。
func TestPinnedSurvivesOtherTargetedWrites(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.CreateSession(Session{ID: "s1", Title: "新会话", AgentID: "ops"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.SetSessionPinned("s1", 1); err != nil {
		t.Fatalf("set pinned: %v", err)
	}
	pinned, err := st.GetSession("s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pinned.PinnedAt <= 0 {
		t.Fatalf("置顶后 pinned_at = %d, want > 0", pinned.PinnedAt)
	}

	// 四个定向写入依次跑过（各自的守卫语义在 session_title_test.go 里覆盖，
	// 这里只关心它们都不许改写 pinned / pinned_at）
	if _, err := st.SetSessionAgentID("s1", "bare"); err != nil {
		t.Fatalf("SetSessionAgentID: %v", err)
	}
	if _, err := st.SetSessionFallbackTitle("s1", "首条问题截断"); err != nil {
		t.Fatalf("SetSessionFallbackTitle: %v", err)
	}
	if _, err := st.UpdateSessionTitle("s1", "AI 标题"); err != nil {
		t.Fatalf("UpdateSessionTitle: %v", err)
	}
	if _, err := st.SetSessionTitleManual("s1", "手动名字"); err != nil {
		t.Fatalf("SetSessionTitleManual: %v", err)
	}

	got, err := st.GetSession("s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Pinned != 1 {
		t.Fatalf("其他定向写入把 pinned 改成了 %d, want 1", got.Pinned)
	}
	if got.PinnedAt != pinned.PinnedAt {
		t.Fatalf("其他定向写入把 pinned_at 从 %d 改成了 %d", pinned.PinnedAt, got.PinnedAt)
	}
	if got.AgentID != "bare" || got.Title != "手动名字" || got.TitleSource != TitleSourceManual {
		t.Fatalf("四个定向写入本身失效: %+v", got)
	}
}
