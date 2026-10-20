package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"DevCraft/internal/apidoc"
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
	if len(f.Params) != 1 || f.Params[0].Pattern == "" {
		t.Fatalf("params round-trip failed: %+v", f.Params)
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

// newApiTestStore 打开一个 t.TempDir() 下的真 SQLite（仓库测试约定：无断言库、手写 t.Fatalf）。
func newApiTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestApiProjectDomainEntryRoundTrip 项目/领域/接口条目的 CRUD 往返。
func TestApiProjectDomainEntryRoundTrip(t *testing.T) {
	st := newApiTestStore(t)

	proj := ApiProject{Name: "paper-manager", Display: "论文管家", BasePath: "/api/v1.0/user"}
	if err := st.CreateApiProject(&proj); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if proj.ID == "" || proj.CreatedAt == 0 {
		t.Fatalf("project id/createdAt should be filled back: %+v", proj)
	}
	// 名称唯一：同名新建必须拒绝（中文错误，不是裸 UNIQUE constraint failed）
	dup := ApiProject{Name: "paper-manager"}
	if err := st.CreateApiProject(&dup); err == nil {
		t.Fatal("duplicate project name should be rejected")
	}
	byName, err := st.GetApiProjectByName("paper-manager")
	if err != nil || byName.ID != proj.ID || byName.Display != "论文管家" {
		t.Fatalf("get by name: %+v err=%v", byName, err)
	}
	if _, err := st.GetApiProjectByName("不存在"); err == nil {
		t.Fatal("expected not-found error for missing project")
	}

	dom := ApiDomain{ProjectID: proj.ID, Name: "订阅源管理", Sort: 1, NotesRaw: "认证方式：需要登录"}
	if err := st.SaveApiDomain(&dom); err != nil {
		t.Fatalf("save domain: %v", err)
	}
	if dom.ID == "" {
		t.Fatalf("domain id should be filled back: %+v", dom)
	}
	// (project_id, name) upsert：再次保存同名领域 → 更新而不是新增，且保留原 id
	dom.NotesRaw = "认证方式：X-Internal-Token"
	dom.Internal = true
	firstID := dom.ID
	if err := st.SaveApiDomain(&ApiDomain{ProjectID: proj.ID, Name: "订阅源管理", Sort: 2, NotesRaw: "x"}); err != nil {
		t.Fatalf("save domain again: %v", err)
	}
	doms, err := st.ListApiDomains(proj.ID)
	if err != nil || len(doms) != 1 {
		t.Fatalf("domains = %+v err=%v", doms, err)
	}
	if doms[0].ID != firstID || doms[0].NotesRaw != "x" || doms[0].Sort != 2 {
		t.Fatalf("domain upsert should keep id and update fields: %+v", doms[0])
	}

	entry := ApiEntry{
		ProjectID: proj.ID, DomainID: doms[0].ID, Name: "新增订阅",
		Method: "POST", PathRaw: "/api/v1.0/user/subscription", IdentityKey: "POST|/api/v1.0/user/subscription",
		ContentType: "application/json", Auth: "需要登录", Visibility: ApiVisibilityPublic,
		SemanticHash: "sem-1", DocHash: "doc-1", Description: "为当前用户新增一条订阅",
		ParseState: ApiParseStateOK, RawMarkdown: "### 8.2 新增订阅",
	}
	if err := st.SaveApiEntry(&entry); err != nil {
		t.Fatalf("save entry: %v", err)
	}
	if entry.ID == "" {
		t.Fatalf("entry id should be filled back: %+v", entry)
	}
	got, err := st.GetApiEntryByIdentity(proj.ID, "POST|/api/v1.0/user/subscription")
	if err != nil {
		t.Fatalf("get by identity: %v", err)
	}
	if got.Name != "新增订阅" || got.RawMarkdown != "### 8.2 新增订阅" || got.Auth != "需要登录" {
		t.Fatalf("entry round-trip mismatch: %+v", got)
	}
	// 更新定义（同 key 再存 → 覆盖当前态，id 不变）
	got.SemanticHash = "sem-2"
	got.Description = "改过的说明"
	if err := st.SaveApiEntry(&got); err != nil {
		t.Fatalf("update entry: %v", err)
	}
	got2, err := st.GetApiEntry(got.ID)
	if err != nil || got2.SemanticHash != "sem-2" || got2.Description != "改过的说明" || got2.ID != entry.ID {
		t.Fatalf("entry after update: %+v err=%v", got2, err)
	}

	// 字段与示例整组替换
	fields := []ApiField{
		{Kind: ApiFieldKindRequest, Name: "topCategory", TypeRaw: "String", TypeKey: "String", Required: 0, Position: "Body", Description: "顶级分类"},
		{Kind: ApiFieldKindResponse, Name: "message", TypeRaw: "String", TypeKey: "String", Required: -1, Description: "响应消息"},
	}
	if err := st.ReplaceApiFields(ApiOwnerEndpoint, got.ID, fields); err != nil {
		t.Fatalf("replace fields: %v", err)
	}
	if fields[0].ID == 0 || fields[1].ID == 0 {
		t.Fatalf("field ids should be filled back: %+v", fields)
	}
	gotFields, err := st.ListApiFields(ApiOwnerEndpoint, got.ID)
	if err != nil || len(gotFields) != 2 {
		t.Fatalf("fields = %+v err=%v", gotFields, err)
	}
	if gotFields[0].Required != 0 || gotFields[1].Required != -1 {
		t.Fatalf("required sentinel (-1 未知 | 0 否) round-trip failed: %+v", gotFields)
	}
	// 再替换一次 = 整组覆盖，不残留旧行
	if err := st.ReplaceApiFields(ApiOwnerEndpoint, got.ID, fields[:1]); err != nil {
		t.Fatalf("replace fields 2: %v", err)
	}
	gotFields, _ = st.ListApiFields(ApiOwnerEndpoint, got.ID)
	if len(gotFields) != 1 || gotFields[0].Name != "topCategory" {
		t.Fatalf("fields after replace: %+v", gotFields)
	}
	examples := []ApiExample{{Kind: ApiExampleCURL, Language: "bash", Content: "curl -X POST ..."}}
	if err := st.ReplaceApiExamples(ApiOwnerEndpoint, got.ID, examples); err != nil {
		t.Fatalf("replace examples: %v", err)
	}
	gotEx, err := st.ListApiExamples(ApiOwnerEndpoint, got.ID)
	if err != nil || len(gotEx) != 1 || gotEx[0].Content != "curl -X POST ..." {
		t.Fatalf("examples = %+v err=%v", gotEx, err)
	}

	// 命名结构 upsert + ambiguous 标记
	sc := ApiSchema{ProjectID: proj.ID, Name: "IPage", OwnerScope: got.ID, BodyHash: "h1", Ambiguous: true, RawMarkdown: "**IPage 分页结构**："}
	if err := st.SaveApiSchema(&sc); err != nil {
		t.Fatalf("save schema: %v", err)
	}
	if sc.ID == "" {
		t.Fatalf("schema id should be filled back: %+v", sc)
	}
	sc2 := ApiSchema{ProjectID: proj.ID, Name: "IPage", OwnerScope: got.ID, BodyHash: "h2"}
	if err := st.SaveApiSchema(&sc2); err != nil {
		t.Fatalf("save schema again: %v", err)
	}
	if sc2.ID != sc.ID {
		t.Fatalf("schema upsert should keep id: %s vs %s", sc2.ID, sc.ID)
	}
	schemas, err := st.ListApiSchemas(proj.ID)
	if err != nil || len(schemas) != 1 || schemas[0].Ambiguous || schemas[0].BodyHash != "h2" {
		t.Fatalf("schemas = %+v err=%v", schemas, err)
	}

	// 删除条目 → 字段/示例/命名结构级联清理
	if err := st.DeleteApiEntry(got.ID); err != nil {
		t.Fatalf("delete entry: %v", err)
	}
	if _, err := st.GetApiEntry(got.ID); err == nil {
		t.Fatal("expected not-found after delete")
	}
	if n, _ := st.ListApiFields(ApiOwnerEndpoint, got.ID); len(n) != 0 {
		t.Fatalf("fields should be cascaded: %+v", n)
	}
	if n, _ := st.ListApiSchemas(proj.ID); len(n) != 0 {
		t.Fatalf("schemas owned by entry should be cascaded: %+v", n)
	}

	// 删除项目 → 全部级联
	dom2 := ApiDomain{ProjectID: proj.ID, Name: "下载记录"}
	if err := st.SaveApiDomain(&dom2); err != nil {
		t.Fatalf("save domain2: %v", err)
	}
	if err := st.DeleteApiProject(proj.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if _, err := st.GetApiProject(proj.ID); err == nil {
		t.Fatal("expected not-found after project delete")
	}
	if n, _ := st.ListApiDomains(proj.ID); len(n) != 0 {
		t.Fatalf("domains should be cascaded: %+v", n)
	}
}

// TestListApiDomainsOrder 钉住 ListApiDomains 的排序行为（ORDER BY sort, name）：
// 该方法的函数体在 PR3 被抽取为 Store/Tx 共用的 executor 助手，签名与行为必须不变
// （Tx.ListApiDomains 走同一份 SQL，导入大事务里读领域名 → id 映射依赖此顺序稳定）。
func TestListApiDomainsOrder(t *testing.T) {
	st := newApiTestStore(t)
	proj := ApiProject{Name: "svc-order"}
	if err := st.CreateApiProject(&proj); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// 乱序写入：sort 优先，同 sort 按 name
	for _, d := range []ApiDomain{
		{ProjectID: proj.ID, Name: "丙领域", Sort: 2},
		{ProjectID: proj.ID, Name: "乙领域", Sort: 1},
		{ProjectID: proj.ID, Name: "b领域", Sort: 1},
		{ProjectID: proj.ID, Name: "a领域", Sort: 0},
	} {
		row := d
		if err := st.SaveApiDomain(&row); err != nil {
			t.Fatalf("save domain %s: %v", d.Name, err)
		}
	}
	doms, err := st.ListApiDomains(proj.ID)
	if err != nil || len(doms) != 4 {
		t.Fatalf("domains = %+v err=%v", doms, err)
	}
	want := []string{"a领域", "b领域", "乙领域", "丙领域"}
	for i, w := range want {
		if doms[i].Name != w {
			t.Fatalf("排序不对: [%d] = %s, want %s（ORDER BY sort, name）", i, doms[i].Name, w)
		}
	}
	// Tx 面同一份 SQL：事务内读到的顺序一致
	if err := st.WithTx(func(tx *Tx) error {
		txDoms, err := tx.ListApiDomains(proj.ID)
		if err != nil || len(txDoms) != 4 {
			t.Fatalf("tx domains = %+v err=%v", txDoms, err)
		}
		for i, w := range want {
			if txDoms[i].Name != w {
				t.Fatalf("Tx 排序不一致: [%d] = %s, want %s", i, txDoms[i].Name, w)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("with tx: %v", err)
	}
}

// TestApiEntryIdentityUnique 同项目内 identity_key 唯一（UNIQUE(project_id, identity_key)）：
// 不同 id 撞同一 key → upsert 收敛为一行；不同项目同 key → 互不干扰。
func TestApiEntryIdentityUnique(t *testing.T) {
	st := newApiTestStore(t)
	p1 := ApiProject{Name: "svc-a"}
	p2 := ApiProject{Name: "svc-b"}
	if err := st.CreateApiProject(&p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}
	if err := st.CreateApiProject(&p2); err != nil {
		t.Fatalf("create p2: %v", err)
	}
	key := "PUT|/api/v1.0/user/subscription/{}"
	e1 := ApiEntry{ID: "id-1", ProjectID: p1.ID, Name: "修改订阅", Method: "PUT", PathRaw: "/api/v1.0/user/subscription/{id}", IdentityKey: key}
	if err := st.SaveApiEntry(&e1); err != nil {
		t.Fatalf("save e1: %v", err)
	}
	// 换一个预置 id 撞同一 (project, identity_key) → 必须收敛更新，不产生第二行
	e2 := ApiEntry{ID: "id-2", ProjectID: p1.ID, Name: "修改订阅（改名）", Method: "PUT", PathRaw: "/api/v1.0/user/subscription/{subId}", IdentityKey: key}
	if err := st.SaveApiEntry(&e2); err != nil {
		t.Fatalf("save e2: %v", err)
	}
	if e2.ID != e1.ID {
		t.Fatalf("upsert should keep existing row id %s, got %s", e1.ID, e2.ID)
	}
	list, err := st.ListApiEntries(p1.ID, "", true, true)
	if err != nil || len(list) != 1 {
		t.Fatalf("entries = %+v err=%v", list, err)
	}
	if list[0].Name != "修改订阅（改名）" || list[0].PathRaw != "/api/v1.0/user/subscription/{subId}" {
		t.Fatalf("entry should be updated in place: %+v", list[0])
	}
	// 另一个项目的同 key 是独立行
	e3 := ApiEntry{ProjectID: p2.ID, Name: "别的服务", Method: "PUT", PathRaw: "/api/v1.0/user/subscription/{id}", IdentityKey: key}
	if err := st.SaveApiEntry(&e3); err != nil {
		t.Fatalf("save e3: %v", err)
	}
	if e3.ID == e1.ID {
		t.Fatal("same identity_key in another project must be a separate row")
	}
}

// TestApiEntrySoftDelete 软删：status=removed + deleted_at；列表默认过滤，可显式包含；重新保存即恢复。
func TestApiEntrySoftDelete(t *testing.T) {
	st := newApiTestStore(t)
	proj := ApiProject{Name: "svc"}
	if err := st.CreateApiProject(&proj); err != nil {
		t.Fatalf("create project: %v", err)
	}
	e := ApiEntry{ProjectID: proj.ID, Name: "删除订阅", Method: "DELETE",
		PathRaw: "/api/v1.0/user/subscription/{id}", IdentityKey: "DELETE|/api/v1.0/user/subscription/{}"}
	if err := st.SaveApiEntry(&e); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.MarkApiEntryRemoved(e.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := st.MarkApiEntryRemoved("不存在"); err == nil {
		t.Fatal("expected not-found error")
	}
	// 默认列表过滤 removed
	if list, _ := st.ListApiEntries(proj.ID, "", false, true); len(list) != 0 {
		t.Fatalf("removed entry should be filtered by default: %+v", list)
	}
	all, err := st.ListApiEntries(proj.ID, "", true, true)
	if err != nil || len(all) != 1 || all[0].Status != ApiEntryStatusRemoved || all[0].DeletedAt == 0 {
		t.Fatalf("includeRemoved should show removed entry: %+v err=%v", all, err)
	}
	// 软删后仍可按 id / identity 取到（事件要能长期引用「当时那个接口」）
	if _, err := st.GetApiEntry(e.ID); err != nil {
		t.Fatalf("removed entry still readable: %v", err)
	}
	// 重新出现 → 恢复（status 回 active，deleted_at 清零）
	e.Status = ApiEntryStatusActive
	e.DeletedAt = 0
	if err := st.SaveApiEntry(&e); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if list, _ := st.ListApiEntries(proj.ID, "", false, true); len(list) != 1 || list[0].Status != ApiEntryStatusActive {
		t.Fatalf("entry should be restored: %+v", list)
	}
	// internal 可见性过滤
	e2 := ApiEntry{ProjectID: proj.ID, Name: "RPC 保存", Method: "POST",
		PathRaw: "/api/v1.0/paper/rpc/arxiv/save", IdentityKey: "POST|/api/v1.0/paper/rpc/arxiv/save",
		Visibility: ApiVisibilityInternal}
	if err := st.SaveApiEntry(&e2); err != nil {
		t.Fatalf("save internal: %v", err)
	}
	if list, _ := st.ListApiEntries(proj.ID, "", false, false); len(list) != 1 {
		t.Fatalf("internal should be filtered by default: %+v", list)
	}
	if list, _ := st.ListApiEntries(proj.ID, "", false, true); len(list) != 2 {
		t.Fatalf("includeInternal should show both: %+v", list)
	}
}

// TestApiChangeEventSeqAndItems seq 项目内自增（同一事务 MAX(seq)+1）；
// 列表不含 doc_text；明细 diff JSON 列坏数据容错（照 _ = json.Unmarshal 先例）。
func TestApiChangeEventSeqAndItems(t *testing.T) {
	st := newApiTestStore(t)
	pa := ApiProject{Name: "svc-a"}
	pb := ApiProject{Name: "svc-b"}
	if err := st.CreateApiProject(&pa); err != nil {
		t.Fatalf("create pa: %v", err)
	}
	if err := st.CreateApiProject(&pb); err != nil {
		t.Fatalf("create pb: %v", err)
	}

	ev1 := ApiChangeEvent{ProjectID: pa.ID, Title: "第一次导入", Reason: "前端要下载历史",
		Kind: ApiEventKindFeature, Label: "REQ-1024", ScopeMode: ApiScopeIncremental,
		Stats: ApiEventStats{Added: 2, Updated: 1}, Meta: map[string]any{"doc_sha256": "abc"},
		DocText: "# 用户服务接口变更文档\n...全文..."}
	if err := st.InsertApiChangeEvent(&ev1); err != nil {
		t.Fatalf("insert ev1: %v", err)
	}
	if ev1.ID == 0 || ev1.Seq != 1 {
		t.Fatalf("ev1 should get id and seq=1: %+v", ev1)
	}
	ev2 := ApiChangeEvent{ProjectID: pa.ID, Title: "第二次导入"}
	if err := st.InsertApiChangeEvent(&ev2); err != nil {
		t.Fatalf("insert ev2: %v", err)
	}
	ev3 := ApiChangeEvent{ProjectID: pa.ID, Title: "第三次导入"}
	if err := st.InsertApiChangeEvent(&ev3); err != nil {
		t.Fatalf("insert ev3: %v", err)
	}
	if ev2.Seq != 2 || ev3.Seq != 3 {
		t.Fatalf("seq should auto-increment per project: %d %d", ev2.Seq, ev3.Seq)
	}
	evb := ApiChangeEvent{ProjectID: pb.ID, Title: "别的项目第一次"}
	if err := st.InsertApiChangeEvent(&evb); err != nil {
		t.Fatalf("insert evb: %v", err)
	}
	if evb.Seq != 1 {
		t.Fatalf("seq is per-project, want 1 got %d", evb.Seq)
	}

	// 列表按 seq DESC，且明确列名不带 doc_text（大文本单独取）
	list, err := st.ListApiChangeEvents(pa.ID, 0)
	if err != nil || len(list) != 3 {
		t.Fatalf("events = %+v err=%v", list, err)
	}
	if list[0].Seq != 3 || list[2].Seq != 1 {
		t.Fatalf("events should be ordered by seq DESC: %+v", list)
	}
	if list[0].DocText != "" {
		t.Fatal("list must not carry doc_text")
	}
	if list[2].Stats.Added != 2 || list[2].Stats.Updated != 1 || list[2].Meta["doc_sha256"] != "abc" {
		t.Fatalf("stats/meta JSON round-trip failed: %+v", list[2])
	}
	docText, err := st.GetApiChangeEventDocText(ev1.ID)
	if err != nil || docText != ev1.DocText {
		t.Fatalf("doc text = %q err=%v", docText, err)
	}
	got, err := st.GetApiChangeEvent(ev1.ID)
	if err != nil || got.Title != "第一次导入" || got.DocText != "" {
		t.Fatalf("event detail = %+v err=%v", got, err)
	}
	if _, err := st.GetApiChangeEvent(99999); err == nil {
		t.Fatal("expected not-found event error")
	}

	// 明细：字段级 diff 载荷往返
	item := ApiChangeItem{
		EventID: ev1.ID, ProjectID: pa.ID, EntryID: "entry-1", DomainName: "订阅源管理", EntryName: "新增订阅",
		Method: "POST", Path: "/api/v1.0/user/subscription", ChangeType: ApiChangeTypeUpdated,
		Breaking: ApiBreakingYes, DeclaredNote: "请求体新增可选字段 source",
		Diff: []apidoc.DiffItem{{
			Section: "request.body", Key: "subCategories", Op: "updated",
			Before:   apidoc.FieldFragment{Name: "subCategories", Type: "String", Required: 0},
			After:    apidoc.FieldFragment{Name: "subCategories", Type: "String", Required: 1},
			Breaking: true, Rule: "request-property-became-required",
		}},
	}
	if err := st.InsertApiChangeItems([]*ApiChangeItem{&item}); err != nil {
		t.Fatalf("insert items: %v", err)
	}
	if item.ID == 0 {
		t.Fatalf("item id should be filled back: %+v", item)
	}
	items, err := st.ListApiChangeItems(ev1.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v err=%v", items, err)
	}
	before, _ := json.Marshal(items[0].Diff[0].Before)
	if len(items[0].Diff) != 1 || items[0].Diff[0].Rule != "request-property-became-required" ||
		!strings.Contains(string(before), "subCategories") {
		t.Fatalf("diff payload round-trip failed: %+v", items[0].Diff)
	}

	// diff 列坏数据容错：不崩、按空值继续（照 store.go 的 _ = json.Unmarshal 先例）
	if _, err := st.db.Exec(`UPDATE api_change_items SET diff = ? WHERE id = ?`, `{不是合法JSON`, item.ID); err != nil {
		t.Fatalf("corrupt diff: %v", err)
	}
	items, err = st.ListApiChangeItems(ev1.ID)
	if err != nil {
		t.Fatalf("list with corrupt diff should not fail: %v", err)
	}
	if len(items) != 1 || len(items[0].Diff) != 0 || items[0].DeclaredNote != "请求体新增可选字段 source" {
		t.Fatalf("corrupt diff should degrade to empty: %+v", items[0])
	}

	// 删除事件 → 明细级联
	if err := st.DeleteApiChangeEvent(ev1.ID); err != nil {
		t.Fatalf("delete event: %v", err)
	}
	if items, _ := st.ListApiChangeItems(ev1.ID); len(items) != 0 {
		t.Fatalf("items should be cascaded: %+v", items)
	}
}

// TestApiImportBatchRoundTrip 导入批次（表 MVP 就建好、列留全；删除候选逻辑二期）。
func TestApiImportBatchRoundTrip(t *testing.T) {
	st := newApiTestStore(t)
	proj := ApiProject{Name: "svc"}
	if err := st.CreateApiProject(&proj); err != nil {
		t.Fatalf("create project: %v", err)
	}
	b := ApiImportBatch{
		ProjectID: proj.ID, ScopeIdentity: "svc|/api/v1.0/user|用户服务接口文档",
		DeclaredMode: ApiScopeInit, DocFingerprint: "sha256:xxx",
		Keys:         []string{"GET|/api/v1.0/user", "POST|/api/v1.0/user/register"},
		SummaryCheck: apidoc.SummaryCheck{Present: true, Closed: true},
	}
	if err := st.InsertApiImportBatch(&b); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	if b.ID == 0 || b.CreatedAt == 0 {
		t.Fatalf("batch id/createdAt should be filled back: %+v", b)
	}
	list, err := st.ListApiImportBatches(b.ScopeIdentity, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("batches = %+v err=%v", list, err)
	}
	if len(list[0].Keys) != 2 || list[0].Keys[1] != "POST|/api/v1.0/user/register" {
		t.Fatalf("keys JSON round-trip failed: %+v", list[0].Keys)
	}
	if !list[0].SummaryCheck.Closed || !list[0].SummaryCheck.Present {
		t.Fatalf("summary check round-trip failed: %+v", list[0].SummaryCheck)
	}
	// keys_json 坏数据容错
	if _, err := st.db.Exec(`UPDATE api_import_batches SET keys_json = ? WHERE id = ?`, `[坏的`, b.ID); err != nil {
		t.Fatalf("corrupt keys: %v", err)
	}
	if list, err = st.ListApiImportBatches(b.ScopeIdentity, 0); err != nil || len(list) != 1 || len(list[0].Keys) != 0 {
		t.Fatalf("corrupt keys should degrade to empty: %+v err=%v", list, err)
	}
}

// TestStoreApiWithTx 一次导入的多表写单事务（PRD R4 / AC3）：
// Tx 内写 项目+领域+条目+字段+事件+明细，成功一起提交；任一步失败整体回滚、数据库零变更；
// 事务内能读到自己未提交的写（导入编排要对照当前态算 diff）。
func TestStoreApiWithTx(t *testing.T) {
	st := newApiTestStore(t)

	var projID, entryID string
	var eventID int64
	err := st.WithTx(func(tx *Tx) error {
		proj := ApiProject{Name: "svc-tx", Display: "事务服务"}
		if err := tx.CreateApiProject(&proj); err != nil {
			return err
		}
		projID = proj.ID
		dom := ApiDomain{ProjectID: proj.ID, Name: "订阅源管理"}
		if err := tx.SaveApiDomain(&dom); err != nil {
			return err
		}
		e := ApiEntry{ProjectID: proj.ID, DomainID: dom.ID, Name: "新增订阅",
			Method: "POST", PathRaw: "/api/v1/tx/sub", IdentityKey: "POST|/api/v1/tx/sub"}
		if err := tx.SaveApiEntry(&e); err != nil {
			return err
		}
		entryID = e.ID
		// 事务内读到自己未提交的写
		got, err := tx.GetApiEntryByIdentity(proj.ID, "POST|/api/v1/tx/sub")
		if err != nil {
			return err
		}
		if got.ID != e.ID || got.DomainID != dom.ID {
			return errors.New("事务内读取应与写入一致")
		}
		if err := tx.ReplaceApiFields(ApiOwnerEndpoint, e.ID, []ApiField{
			{Kind: ApiFieldKindRequest, Name: "source", TypeRaw: "String", TypeKey: "String", Required: 0, Position: "Body"},
		}); err != nil {
			return err
		}
		ev := ApiChangeEvent{ProjectID: proj.ID, Title: "第一次导入", Reason: "测试单事务多表写", Stats: ApiEventStats{Added: 1}}
		if err := tx.InsertApiChangeEvent(&ev); err != nil {
			return err
		}
		eventID = ev.ID
		if ev.Seq != 1 {
			return errors.New("事务内 MAX(seq)+1 应为 1")
		}
		return tx.InsertApiChangeItems([]*ApiChangeItem{{
			EventID: ev.ID, ProjectID: proj.ID, EntryID: e.ID, DomainName: dom.Name, EntryName: e.Name,
			Method: e.Method, Path: e.PathRaw, ChangeType: ApiChangeTypeAdded,
		}})
	})
	if err != nil {
		t.Fatalf("with tx: %v", err)
	}
	if projID == "" || entryID == "" || eventID == 0 {
		t.Fatalf("主键应在事务内回填: %q %q %d", projID, entryID, eventID)
	}
	if fields, _ := st.ListApiFields(ApiOwnerEndpoint, entryID); len(fields) != 1 || fields[0].Name != "source" {
		t.Fatalf("提交后字段应可见: %+v", fields)
	}
	if items, _ := st.ListApiChangeItems(eventID); len(items) != 1 || items[0].ChangeType != ApiChangeTypeAdded {
		t.Fatalf("提交后明细应可见: %+v", items)
	}

	// 回滚路径：多表写完后最后一步出错 → 数据库零变更（AC3「整批拒收且零写入」的存储层保证）
	boom := errors.New("模拟失败")
	err = st.WithTx(func(tx *Tx) error {
		proj := ApiProject{Name: "svc-rollback"}
		if err := tx.CreateApiProject(&proj); err != nil {
			return err
		}
		e := ApiEntry{ProjectID: proj.ID, Name: "不该存在的接口", Method: "GET",
			PathRaw: "/rollback/x", IdentityKey: "GET|/rollback/x"}
		if err := tx.SaveApiEntry(&e); err != nil {
			return err
		}
		if err := tx.ReplaceApiFields(ApiOwnerEndpoint, e.ID, []ApiField{
			{Kind: ApiFieldKindRequest, Name: "ghost", Position: "Query"},
		}); err != nil {
			return err
		}
		if err := tx.MarkApiEntryRemoved(e.ID); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx 应原样返回错误: %v", err)
	}
	if _, err := st.GetApiProjectByName("svc-rollback"); err == nil {
		t.Fatal("回滚后项目不应存在")
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM api_entries WHERE identity_key = 'GET|/rollback/x'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("回滚后条目应零变更: n=%d err=%v", n, err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM api_fields WHERE name = 'ghost'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("回滚后字段应零变更: n=%d err=%v", n, err)
	}
	// 先前的成功事务不受回滚影响
	if fields, _ := st.ListApiFields(ApiOwnerEndpoint, entryID); len(fields) != 1 {
		t.Fatalf("回滚不应影响已提交事务: %+v", fields)
	}
}
