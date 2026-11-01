package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// apiEntryColumnNames 读 api_entries 的列名清单（PRAGMA table_info 第 2 列），
// 供加列迁移断言用（与 sessions 的 sessionColumns 同形，表名写死、不接外部输入）。
func apiEntryColumnNames(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.db.Query(`PRAGMA table_info(api_entries)`)
	if err != nil {
		t.Fatalf("pragma table_info(api_entries): %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma rows: %v", err)
	}
	return names
}

// TestApiEntryUnresolvedRoundTrip 钉住 11-01-req-section-absent R5：unresolved 槽位数组
// 随条目落库（JSON 列）并原样读回；详情/按身份/列表三条读取路径都带该列；
// 空槽位读回 [] 而非 nil（JS 侧拿到数组不是 null）；重导治愈时槽位可被清空。
func TestApiEntryUnresolvedRoundTrip(t *testing.T) {
	st := newApiTestStore(t)
	proj := ApiProject{Name: "unresolved-svc", BasePath: "/api/v1/demo"}
	if err := st.CreateApiProject(&proj); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// partial 条目：两个真缺失槽位按序落库、按序读回（顺序敏感，详情页横幅按此列出）
	partial := ApiEntry{
		ProjectID: proj.ID, Name: "缺返回表", Method: "DELETE",
		PathRaw: "/api/v1/demo/user/{id}", IdentityKey: "DELETE|/api/v1/demo/user/{}",
		ParseState: ApiParseStatePartial, Unresolved: []string{"response_params", "path_params"},
		RawMarkdown: "### 1.1 缺返回表",
	}
	if err := st.SaveApiEntry(&partial); err != nil {
		t.Fatalf("save partial: %v", err)
	}
	got, err := st.GetApiEntry(partial.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if len(got.Unresolved) != 2 || got.Unresolved[0] != "response_params" || got.Unresolved[1] != "path_params" {
		t.Fatalf("unresolved 槽位往返失败（GetApiEntry）: %v", got.Unresolved)
	}
	byKey, err := st.GetApiEntryByIdentity(proj.ID, partial.IdentityKey)
	if err != nil || len(byKey.Unresolved) != 2 || byKey.Unresolved[1] != "path_params" {
		t.Fatalf("unresolved 往返失败（GetApiEntryByIdentity）: %v err=%v", byKey.Unresolved, err)
	}
	// 列表行也带 unresolved（小 JSON，列表页横幅 fallback 要用），但 raw_markdown 仍有意留空
	list, err := st.ListApiEntries(proj.ID, "", false, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v err=%v", list, err)
	}
	if len(list[0].Unresolved) != 2 || list[0].Unresolved[0] != "response_params" {
		t.Fatalf("列表行 unresolved 缺失: %v", list[0].Unresolved)
	}
	if list[0].RawMarkdown != "" {
		t.Fatalf("列表行不应带 raw_markdown（有意留空）: %q", list[0].RawMarkdown)
	}

	// ok 条目、Unresolved 传 nil → 落库 '[]'、读回空且非 nil（JSON 序列化成 [] 不是 null）
	okEntry := ApiEntry{
		ProjectID: proj.ID, Name: "无请求参数", Method: "GET",
		PathRaw: "/api/v1/demo/login-log/export", IdentityKey: "GET|/api/v1/demo/login-log/export",
		ParseState: ApiParseStateOK, Unresolved: nil,
	}
	if err := st.SaveApiEntry(&okEntry); err != nil {
		t.Fatalf("save ok: %v", err)
	}
	gotOK, err := st.GetApiEntry(okEntry.ID)
	if err != nil {
		t.Fatalf("get ok: %v", err)
	}
	if gotOK.Unresolved == nil || len(gotOK.Unresolved) != 0 {
		t.Fatalf("空槽位应读回非 nil 的空数组: %#v", gotOK.Unresolved)
	}
	var rawCol string
	if err := st.db.QueryRow(`SELECT unresolved FROM api_entries WHERE id = ?`, okEntry.ID).Scan(&rawCol); err != nil {
		t.Fatalf("读原始列: %v", err)
	}
	if rawCol != "[]" {
		t.Fatalf("空槽位原始列应为 '[]'，实际 %q", rawCol)
	}

	// 治愈：partial 条目重导后改 ok + 清空槽位 → 读回空（store 层覆盖写，不残留旧槽位）
	got.ParseState = ApiParseStateOK
	got.Unresolved = nil
	if err := st.SaveApiEntry(&got); err != nil {
		t.Fatalf("cure save: %v", err)
	}
	healed, err := st.GetApiEntry(partial.ID)
	if err != nil {
		t.Fatalf("get healed: %v", err)
	}
	if healed.ParseState != ApiParseStateOK || len(healed.Unresolved) != 0 {
		t.Fatalf("治愈后应 ok 且零槽位: state=%s unresolved=%v", healed.ParseState, healed.Unresolved)
	}
}

// TestMigrateAddsUnresolvedColumnToLegacyApiEntries 复刻「存量库还没有 unresolved 列」的形态：
// 老 21 列 api_entries 表 + 一条旧 partial 数据行，启动后必须 ALTER 出该列（DEFAULT '[]'），
// 旧行读回 Unresolved=[]（非 nil）、parse_state 仍 partial、别的数据逐字不变；且加列幂等
// （列只出现一次，重开库不重复 ALTER）。这是 R5/R6 存量治愈在存储层的前提：旧行先有 '[]'
// 兜底，前端横幅走 fallback 文案，重导同文档后由 entryNeedsWrite 的槽位对照回填/治愈。
func TestMigrateAddsUnresolvedColumnToLegacyApiEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-api.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	// 老表：当前 22 列 schema 去掉 unresolved 那一列（规则放宽前的存量形态）
	if _, err := raw.Exec(`
CREATE TABLE api_entries (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
    domain_id        TEXT NOT NULL,
    name             TEXT NOT NULL,
    method           TEXT NOT NULL,
    path_raw         TEXT NOT NULL,
    identity_key     TEXT NOT NULL,
    base_path_assert TEXT NOT NULL DEFAULT '',
    content_type     TEXT NOT NULL DEFAULT '',
    auth             TEXT NOT NULL DEFAULT '',
    visibility       TEXT NOT NULL DEFAULT 'public',
    semantic_hash    TEXT NOT NULL DEFAULT '',
    doc_hash         TEXT NOT NULL DEFAULT '',
    description      TEXT NOT NULL DEFAULT '',
    parse_state      TEXT NOT NULL DEFAULT 'ok',
    raw_markdown     TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'active',
    deleted_at       INTEGER NOT NULL DEFAULT 0,
    sort             INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    UNIQUE (project_id, identity_key)
);
INSERT INTO api_entries
    (id, project_id, domain_id, name, method, path_raw, identity_key, parse_state, raw_markdown, created_at, updated_at)
VALUES
    ('old', 'p1', 'd1', '删除订阅', 'DELETE', '/api/v1.0/user/subscription/{id}',
     'DELETE|/api/v1.0/user/subscription/{}', 'partial', '### 8.4 删除订阅', 1, 1);
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

	cols := apiEntryColumnNames(t, st)
	if n := countColumn(cols, "unresolved"); n != 1 {
		t.Fatalf("unresolved 列出现 %d 次，want 1（cols=%v）", n, cols)
	}
	got, err := st.GetApiEntry("old")
	if err != nil {
		t.Fatalf("get old: %v", err)
	}
	if got.Unresolved == nil || len(got.Unresolved) != 0 {
		t.Fatalf("旧行 unresolved 应读回非 nil 空数组（DEFAULT '[]' 落位）: %#v", got.Unresolved)
	}
	if got.ParseState != ApiParseStatePartial {
		t.Fatalf("加列迁移不得改 parse_state: %s", got.ParseState)
	}
	if got.Name != "删除订阅" || got.RawMarkdown != "### 8.4 删除订阅" || got.IdentityKey != "DELETE|/api/v1.0/user/subscription/{}" {
		t.Fatalf("加列迁移改了别的数据: %+v", got)
	}

	// 幂等：再开一次同一库，列仍只一份、旧行不变（ensureTableColumn 先 PRAGMA 探测再 ALTER）
	st.Close()
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("二次 Open 存量库: %v", err)
	}
	defer st2.Close()
	if n := countColumn(apiEntryColumnNames(t, st2), "unresolved"); n != 1 {
		t.Fatalf("二次启动后 unresolved 列出现 %d 次，want 1（重复 ALTER）", n)
	}
	again, err := st2.GetApiEntry("old")
	if err != nil || again.ParseState != ApiParseStatePartial || len(again.Unresolved) != 0 {
		t.Fatalf("二次启动旧行变了: %+v err=%v", again, err)
	}
}