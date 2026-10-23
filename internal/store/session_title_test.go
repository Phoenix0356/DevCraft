package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// countColumn 数某列在列名清单里出现几次（重复 ALTER 会把同一列加两次的形态排除掉）。
func countColumn(cols []string, name string) int {
	n := 0
	for _, c := range cols {
		if c == name {
			n++
		}
	}
	return n
}

// TestMigrateIdempotentOnFreshDB 全新库：建表语句已带 title_source，
// 反复 Open（每次启动都会跑一遍 migrate）必须不报错，且列只有一条。
func TestMigrateIdempotentOnFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("第一次 Open: %v", err)
	}
	if err := st.CreateSession(Session{ID: "s1", Title: "新会话", AgentID: "ops"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(path) // 第二次启动：CREATE TABLE IF NOT EXISTS 跳过 + 加列探测命中
	if err != nil {
		t.Fatalf("第二次 Open: %v", err)
	}
	defer st2.Close()
	st3, err := Open(path) // 第三次也无所谓（幂等 = 跑几次都一样）
	if err != nil {
		t.Fatalf("第三次 Open: %v", err)
	}
	defer st3.Close()

	cols, err := st2.sessionColumns()
	if err != nil {
		t.Fatalf("列清单: %v", err)
	}
	if n := countColumn(cols, "title_source"); n != 1 {
		t.Fatalf("title_source 列出现 %d 次，want 1（cols=%v）", n, cols)
	}
	got, err := st2.GetSession("s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TitleSource != "" {
		t.Fatalf("新建会话的 title_source = %q，want 空串（占位态）", got.TitleSource)
	}
}

// TestMigrateSkipsExistingColumn 复刻"用户这台机器"的存量库：sessions 表里
// title_source 列已经存在（历史上跑过旧版迁移，SQLite 加列不会随代码回滚消失）。
// 迁移必须探测到列已存在 → 干净跳过，不报错、不重复加列。
func TestMigrateSkipsExistingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 手工建一张"已经带该列"的 sessions 表（等价于旧版迁移跑完后的形态）
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	_, err = raw.Exec(`
CREATE TABLE sessions (
	id           TEXT PRIMARY KEY,
	title        TEXT NOT NULL DEFAULT '',
	agent_id     TEXT NOT NULL DEFAULT '',
	title_source TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
INSERT INTO sessions (id, title, agent_id, title_source, created_at, updated_at)
VALUES ('old', '旧会话', 'ops', 'manual', 1, 1);
`)
	if err != nil {
		t.Fatalf("建存量表: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	st, err := Open(path) // 迁移必须不报 "duplicate column name"
	if err != nil {
		t.Fatalf("Open 已有该列的库: %v", err)
	}
	defer st.Close()

	cols, err := st.sessionColumns()
	if err != nil {
		t.Fatalf("列清单: %v", err)
	}
	if n := countColumn(cols, "title_source"); n != 1 {
		t.Fatalf("title_source 列出现 %d 次，want 1（cols=%v）", n, cols)
	}
	got, err := st.GetSession("old")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TitleSource != TitleSourceManual {
		t.Fatalf("旧行的 title_source = %q，want manual（迁移不该动数据）", got.TitleSource)
	}
}

// TestMigrateAddsColumnToLegacyDB 复刻"存量库还没有该列"的形态：老 5 列表 + 旧数据行，
// 启动后必须 ALTER 出该列，旧行取 DEFAULT（空串 = 占位态，下轮 done 后会获得 AI 标题）。
func TestMigrateAddsColumnToLegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

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
		t.Fatalf("Open 缺列的存量库: %v", err)
	}
	defer st.Close()

	cols, err := st.sessionColumns()
	if err != nil {
		t.Fatalf("列清单: %v", err)
	}
	if n := countColumn(cols, "title_source"); n != 1 {
		t.Fatalf("title_source 列出现 %d 次，want 1（cols=%v）", n, cols)
	}
	got, err := st.GetSession("old")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TitleSource != "" {
		t.Fatalf("旧行 title_source = %q，want 空串", got.TitleSource)
	}
	if got.Title != "旧会话" || got.AgentID != "ops" {
		t.Fatalf("加列迁移改了别的数据: %+v", got)
	}
}

// TestUpdateSessionTitleStateMachine 标题来源状态机的存储层守卫：
// 空串态可被 AI 写入 → 'ai'；'manual' 被 SQL 条件永久挡下（不靠调用方自律）；
// 未知 id 写不进去（RowsAffected=0 且不是错误）。
func TestUpdateSessionTitleStateMachine(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.CreateSession(Session{ID: "s1", Title: "新会话", AgentID: "ops"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 空串态：AI 总结写入成功，来源变 'ai'，agent_id 不受影响
	wrote, err := st.UpdateSessionTitle("s1", "容器日志排查")
	if err != nil {
		t.Fatalf("UpdateSessionTitle: %v", err)
	}
	if !wrote {
		t.Fatal("占位态下 AI 标题应写入成功")
	}
	got, _ := st.GetSession("s1")
	if got.Title != "容器日志排查" || got.TitleSource != TitleSourceAI {
		t.Fatalf("AI 写入后 = %+v", got)
	}
	if got.AgentID != "ops" {
		t.Fatalf("AI 写入不该动 agent_id: %q", got.AgentID)
	}

	// 'ai' 态：存储层允许再次写入（"不做每轮重总结"是 appsvc 的触发闸门，不是列约束）
	if wrote, err := st.UpdateSessionTitle("s1", "第二个AI标题"); err != nil || !wrote {
		t.Fatalf("ai 态再写 = (%v, %v)，want (true, nil)", wrote, err)
	}

	// 手动改名：来源锁定 manual
	if wrote, err := st.SetSessionTitleManual("s1", "我自己起的名字"); err != nil || !wrote {
		t.Fatalf("SetSessionTitleManual = (%v, %v)，want (true, nil)", wrote, err)
	}
	got, _ = st.GetSession("s1")
	if got.TitleSource != TitleSourceManual {
		t.Fatalf("手动改名后来源 = %q，want manual", got.TitleSource)
	}

	// manual 之后 AI 再总结：SQL 条件挡下，标题不变
	wrote, err = st.UpdateSessionTitle("s1", "AI 想覆盖")
	if err != nil {
		t.Fatalf("manual 态 UpdateSessionTitle: %v", err)
	}
	if wrote {
		t.Fatal("manual 态 AI 写入必须返回 false")
	}
	got, _ = st.GetSession("s1")
	if got.Title != "我自己起的名字" || got.TitleSource != TitleSourceManual {
		t.Fatalf("manual 保护失效，当前 = %+v", got)
	}

	// 不存在的会话：写 0 行，不是错误
	if wrote, err := st.UpdateSessionTitle("no-such", "x"); err != nil || wrote {
		t.Fatalf("missing id = (%v, %v)，want (false, nil)", wrote, err)
	}
	if wrote, err := st.SetSessionTitleManual("no-such", "x"); err != nil || wrote {
		t.Fatalf("missing id manual = (%v, %v)，want (false, nil)", wrote, err)
	}
}

// TestSessionTargetedWrites 定向写入各自只碰自己的列：
// SetSessionAgentID 只改 agent_id（标题与来源不动）、SetSessionFallbackTitle
// 只在标题仍是占位时写入（已有标题/AI 标题/manual 都写不进去）。
func TestSessionTargetedWrites(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.CreateSession(Session{ID: "s1", Title: "新会话", AgentID: "ops"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 切 Agent：标题/来源零变化
	if wrote, err := st.SetSessionAgentID("s1", "bare"); err != nil || !wrote {
		t.Fatalf("SetSessionAgentID = (%v, %v)", wrote, err)
	}
	got, _ := st.GetSession("s1")
	if got.AgentID != "bare" || got.Title != "新会话" || got.TitleSource != "" {
		t.Fatalf("切 Agent 后 = %+v", got)
	}

	// 占位标题：只在仍是"新会话"/空串时写入
	if wrote, err := st.SetSessionFallbackTitle("s1", "首条问题截断"); err != nil || !wrote {
		t.Fatalf("SetSessionFallbackTitle = (%v, %v)", wrote, err)
	}
	// 已有标题后再来一次占位写入（并发回合的场景）：条件不成立，写不进去
	if wrote, err := st.SetSessionFallbackTitle("s1", "陈旧的截断值"); err != nil || wrote {
		t.Fatalf("占位标题二次写入 = (%v, %v)，want (false, nil)", wrote, err)
	}
	got, _ = st.GetSession("s1")
	if got.Title != "首条问题截断" {
		t.Fatalf("占位标题被陈旧值覆盖: %+v", got)
	}

	// AI 写入后再来的占位写入同样落空；manual 态也挡住"用户改名为新会话"的边界
	if _, err := st.UpdateSessionTitle("s1", "AI 标题"); err != nil {
		t.Fatalf("ai: %v", err)
	}
	if wrote, _ := st.SetSessionFallbackTitle("s1", "截断值"); wrote {
		t.Fatal("AI 标题之后占位写入必须落空")
	}
	if _, err := st.SetSessionTitleManual("s1", "新会话"); err != nil { // 用户真的把名字改成"新会话"
		t.Fatalf("manual: %v", err)
	}
	if wrote, _ := st.SetSessionFallbackTitle("s1", "截断值"); wrote {
		t.Fatal("manual 态占位写入必须落空（即便标题字面上还是新会话）")
	}

	// 不存在的会话：三个定向写入都是 false + nil
	for _, fn := range []func() (bool, error){
		func() (bool, error) { return st.SetSessionAgentID("no-such", "x") },
		func() (bool, error) { return st.SetSessionFallbackTitle("no-such", "x") },
	} {
		if wrote, err := fn(); err != nil || wrote {
			t.Fatalf("missing id = (%v, %v)，want (false, nil)", wrote, err)
		}
	}

	// ListSessions 带出来源（前端侧栏与自动总结闸门都靠它）
	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].TitleSource != TitleSourceManual {
		t.Fatalf("ListSessions = %+v", list)
	}
}
