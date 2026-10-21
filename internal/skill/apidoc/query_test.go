package apidoc

// query_test.go 只读查询技能 apidoc_query 的单测（假只读缝，无库）：
// 钉住 PRD 的六条契约——
//   ① D2 分层预算：六个 action 各自的返回形状与条数上限；
//   ② R2：entries/events 超限必须 truncated=true + total + 缩小指引（不能让模型误以为看到全部）；
//   ③ D4/R4：出口统一 MaskSecrets + CapChars(12000)，截断显式说明；
//   ④ R3：raw_markdown / doc_text 不返回（EntryView/EventView 类型里没有该字段——
//      类型闸门在编译期，这里断言出口文案讲明了这一事实）；
//   ⑤ 参数闸门：未知 action / 缺 project / 项目不存在 三类反例全部中文短句并指路；
//   ⑥ 只读：QuerySource 接口没有任何写方法（接口形状即保证，实现想写也没有通道）。
// 真 SQLite 的「9 张 api_* 表计数不变」断言在 internal/appsvc/apidocquery_test.go。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	docspec "DevCraft/internal/apidoc"
	"DevCraft/internal/store"
)

// fakeQuery 只读缝的假实现（自带 project/domain/keyword/limit 过滤，行为对齐
// appsvc.ListApiEntries / ListApiChangeEvents，保证技能层测的是技能层的事）。
type fakeQuery struct {
	projects []store.ApiProject
	domains  []store.ApiDomain
	entries  []store.ApiEntry
	entry    EntryView
	events   []store.ApiChangeEvent
	event    EventView
}

func newFakeQuery() *fakeQuery {
	return &fakeQuery{
		projects: []store.ApiProject{
			{ID: "p1", Name: "demo-svc", Display: "演示服务"},
			{ID: "p2", Name: "paper-pilot"},
		},
		domains: []store.ApiDomain{
			{ID: "d1", ProjectID: "p1", Name: "用户管理"},
			{ID: "d2", ProjectID: "p1", Name: "内部运维", Internal: true},
		},
		entries: []store.ApiEntry{
			{ID: "e1", ProjectID: "p1", DomainID: "d1", Name: "查询用户", Method: "GET", PathRaw: "/api/v1/demo/user", ParseState: "ok", Visibility: store.ApiVisibilityPublic, Status: store.ApiEntryStatusActive},
			{ID: "e2", ProjectID: "p1", DomainID: "d1", Name: "修改密码", Method: "POST", PathRaw: "/api/v1/demo/password", ParseState: "partial", Visibility: store.ApiVisibilityPublic, Status: store.ApiEntryStatusActive},
			{ID: "e3", ProjectID: "p1", DomainID: "d2", Name: "内部重启", Method: "POST", PathRaw: "/api/v1/demo/restart", ParseState: "ok", Visibility: store.ApiVisibilityInternal, Status: store.ApiEntryStatusActive},
			{ID: "e4", ProjectID: "p1", DomainID: "d1", Name: "删除用户", Method: "DELETE", PathRaw: "/api/v1/demo/user/{id}", ParseState: "ok", Visibility: store.ApiVisibilityPublic, Status: store.ApiEntryStatusRemoved},
			{ID: "e5", ProjectID: "p2", Name: "收藏列表", Method: "GET", PathRaw: "/api/v1.0/user/favorite", ParseState: "ok", Visibility: store.ApiVisibilityPublic, Status: store.ApiEntryStatusActive},
		},
		entry: EntryView{
			ID: "e1", ProjectID: "p1", DomainID: "d1", Name: "查询用户",
			Method: "GET", Path: "/api/v1/demo/user", Description: "按 id 查询用户。",
			Auth: "需要登录", ContentType: "application/json", Visibility: store.ApiVisibilityPublic,
			Status: store.ApiEntryStatusActive, ParseState: "ok", BasePathAssert: "/api/v1/demo",
			PathFields: []store.ApiField{{Name: "id", TypeRaw: "Long", Required: 1, Position: "Path", Description: "用户ID"}},
			RequestFields: []store.ApiField{
				{Name: "withOrders", TypeRaw: "Boolean", Required: 0, Position: "Query", DefaultValue: "false", Description: "是否带订单"},
			},
			ResponseFields: []store.ApiField{
				{Name: "data", TypeRaw: "UserVO", Required: 1, Description: "用户信息"},
				{Name: "code", TypeRaw: "Int", Required: 1, Description: "状态码"},
			},
			Schemas: []EntrySchemaView{
				{Name: "UserVO", Fields: []store.ApiField{
					{Name: "id", TypeRaw: "Long", Required: 1, Description: "用户ID"},
					{Name: "name", TypeRaw: "String", Required: 1, Description: "昵称"},
				}},
				{Name: "分页结构", Ambiguous: true, Fields: []store.ApiField{
					{Name: "total", TypeRaw: "Long", Required: 1},
				}},
			},
			Examples: []store.ApiExample{
				{Kind: "curl", Language: "bash", Content: "curl -H \"Authorization: Bearer abcdefGHIJklmnopQRSTUVwx0123456789\" http://10.0.0.8:8080/api/v1/demo/user?id=1"},
			},
			Events: []store.ApiEntryEventRef{
				{EventID: 17, Seq: 2, Label: "REQ-1", Title: "密码策略升级", ChangeType: "updated", Breaking: 1, CreatedAt: 1760000000000},
			},
		},
		events: []store.ApiChangeEvent{
			{ID: 17, ProjectID: "p1", Seq: 2, Label: "REQ-1", Title: "密码策略升级", Kind: "feature", Breaking: 1,
				Source: store.ApiEventSourceImport, ScopeMode: "incremental",
				Stats: store.ApiEventStats{Added: 2, Updated: 1, Removed: 1}, CreatedAt: 1760000000000},
			{ID: 16, ProjectID: "p1", Seq: 1, Label: "REQ-0", Title: "初始化演示接口", Kind: "feature",
				Source: store.ApiEventSourceImport, ScopeMode: "incremental",
				Stats: store.ApiEventStats{Added: 2}, CreatedAt: 1759000000000},
		},
		event: EventView{
			ID: 17, ProjectID: "p1", Seq: 2, Label: "REQ-1", Title: "密码策略升级",
			Reason:    "安全要求旧密码必填 sk-abcdefghijklmnopqrstuvwxyz123456",
			Kind:      "feature", Breaking: 1, Source: store.ApiEventSourceImport,
			DocRef: "docs/api/change-01.md", ScopeMode: "incremental",
			Stats:     store.ApiEventStats{Added: 2, Updated: 1, Removed: 1}, CreatedAt: 1760000000000,
			Items: []EventItemView{
				{EntryID: "e2", DomainName: "用户管理", EntryName: "修改密码", Method: "POST", Path: "/api/v1/demo/password",
					ChangeType: "updated", Breaking: 1, DeclaredNote: "oldPwd 由可选改为必填",
					Diff: []docspec.DiffItem{
						{Section: docspec.SectionRequestBody, Key: "oldPwd", Op: docspec.OpUpdated,
							Before: docspec.FieldFragment{Name: "oldPwd", Type: "String", Required: 0},
							After:  docspec.FieldFragment{Name: "oldPwd", Type: "String", Required: 1},
							Breaking: true, Rule: "required-changed"},
					}},
			},
		},
	}
}

func (f *fakeQuery) ApiDocSkillProjects(context.Context) ([]store.ApiProject, error) {
	return f.projects, nil
}

func (f *fakeQuery) ApiDocSkillDomains(_ context.Context, projectID string) ([]store.ApiDomain, error) {
	var out []store.ApiDomain
	for _, d := range f.domains {
		if d.ProjectID == projectID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeQuery) ApiDocSkillEntries(_ context.Context, projectID, domainID, keyword string, includeInternal, includeRemoved bool) ([]store.ApiEntry, error) {
	var out []store.ApiEntry
	kw := strings.ToLower(keyword)
	for _, e := range f.entries {
		if e.ProjectID != projectID {
			continue
		}
		if domainID != "" && e.DomainID != domainID {
			continue
		}
		if !includeInternal && e.Visibility == store.ApiVisibilityInternal {
			continue
		}
		if !includeRemoved && e.Status == store.ApiEntryStatusRemoved {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(e.Name), kw) && !strings.Contains(strings.ToLower(e.PathRaw), kw) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeQuery) ApiDocSkillEntry(_ context.Context, entryID string) (EntryView, error) {
	if f.entry.ID == "" || f.entry.ID != entryID {
		return EntryView{}, fmt.Errorf("接口条目不存在: %s", entryID)
	}
	return f.entry, nil
}

func (f *fakeQuery) ApiDocSkillEvents(_ context.Context, projectID string, limit int) ([]store.ApiChangeEvent, error) {
	var out []store.ApiChangeEvent
	for _, e := range f.events {
		if e.ProjectID != projectID {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeQuery) ApiDocSkillEvent(_ context.Context, eventID int64) (EventView, error) {
	if f.event.ID != eventID {
		return EventView{}, fmt.Errorf("变更事件不存在: %d", eventID)
	}
	return f.event, nil
}

// 编译期核对：假实现满足只读缝（接口没有写方法，技能结构上就写不了库——AC6 的技能侧一半）。
var _ QuerySource = (*fakeQuery)(nil)

// runQuery 走注册表执行 apidoc_query（与 agent 循环同一条 dispatch 路径）。
func runQuery(t *testing.T, q QuerySource, args map[string]any) (string, error) {
	t.Helper()
	reg := registerFakeQuery(t, &fakeDocs{}, q)
	return execSkill(t, reg, SkillQuery, mustArgs(t, args))
}

// ===================== ① 六个 action 各一正例 =====================

func TestQueryProjects(t *testing.T) {
	out, err := runQuery(t, newFakeQuery(), map[string]any{"action": "projects"})
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	for _, want := range []string{
		"- demo-svc（显示名 演示服务） ｜ 接口 3 ｜ 事件 2", // 接口数含 internal、不含已删除
		"- paper-pilot ｜ 接口 1 ｜ 事件 0",
		"共 2 个项目",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("projects 输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestQueryDomains(t *testing.T) {
	out, err := runQuery(t, newFakeQuery(), map[string]any{"action": "domains", "project": "demo-svc"})
	if err != nil {
		t.Fatalf("domains: %v", err)
	}
	for _, want := range []string{"- 用户管理 ｜ 接口 2", "- 内部运维 ｜ 接口 1（内部领域", "共 2 个领域"} {
		if !strings.Contains(out, want) {
			t.Fatalf("domains 输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestQueryEntries(t *testing.T) {
	out, err := runQuery(t, newFakeQuery(), map[string]any{"action": "entries", "project": "demo-svc", "keyword": "用户"})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	// D2 摘要行格式：METHOD path ｜ 名称 ｜ 领域 ｜ parseState（不带字段表）
	if !strings.Contains(out, "GET /api/v1/demo/user ｜ 查询用户 ｜ 用户管理 ｜ ok") {
		t.Fatalf("摘要行格式不符 D2:\n%s", out)
	}
	if strings.Contains(out, "oldPwd") || strings.Contains(out, "返回字段:") {
		t.Fatalf("摘要层绝不允许出现字段表（R2）:\n%s", out)
	}
	if !strings.Contains(out, "摘要行不含字段表") {
		t.Fatalf("entries 输出应指路 action=entry:\n%s", out)
	}
}

func TestQueryEntry(t *testing.T) {
	out, err := runQuery(t, newFakeQuery(), map[string]any{"action": "entry", "id": "e1"})
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	for _, want := range []string{
		"GET /api/v1/demo/user ｜ 查询用户", "领域 用户管理",
		"路径参数:", "- id ｜ Long ｜ 必填 ｜ 位置 Path",
		"返回字段:", "命名结构:", "分页结构", "歧义",
		"示例与 curl:", "该接口历史事件", "v2 [id=17]",
		"raw_markdown", // R3：出口文案要讲清「原文不返回」
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("entry 详情缺少 %q:\n%s", want, out)
		}
	}
	// detail 里必须没有 raw_markdown 的内容物（EntryView 类型无该字段 = 类型闸门）；
	// 双 hash 机器列也不进模型
	if strings.Contains(out, "semanticHash") || strings.Contains(out, "docHash") {
		t.Fatalf("机器列不该进模型:\n%s", out)
	}
}

func TestQueryEvents(t *testing.T) {
	out, err := runQuery(t, newFakeQuery(), map[string]any{"action": "events", "project": "demo-svc"})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, want := range []string{"v2 [id=17] 密码策略升级", "label=REQ-1", "breaking=1", "新增2 修改1 删除1 恢复0", "v1 [id=16]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("events 输出缺少 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "为什么改") {
		t.Fatalf("events 应指路 action=event 看 reason/明细:\n%s", out)
	}
}

func TestQueryEvent(t *testing.T) {
	out, err := runQuery(t, newFakeQuery(), map[string]any{"action": "event", "id": "17"})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	for _, want := range []string{
		"变更事件 v2（id 17）: 密码策略升级",
		"为什么改(reason): 安全要求旧密码必填 sk-***", // 掩码后仍保留语义（R4）
		"POST /api/v1/demo/password ｜ 修改密码 ｜ 领域 用户管理 ｜ updated ｜ breaking=1",
		"文档声明变更点: oldPwd 由可选改为必填",
		"[request.body] oldPwd updated ｜", "rule=required-changed", "（破坏）",
		"来源文档: docs/api/change-01.md",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("event 详情缺少 %q:\n%s", want, out)
		}
	}
}

// ===================== ② 反例（未知 action / 缺 project / 项目不存在）=====================

func TestQueryUnknownAction(t *testing.T) {
	_, err := runQuery(t, newFakeQuery(), map[string]any{"action": "endpoints"})
	if err == nil || !strings.Contains(err.Error(), "未知 action") || !strings.Contains(err.Error(), "projects") {
		t.Fatalf("未知 action 应中文报错并列出可用值: %v", err)
	}
}

func TestQueryMissingProject(t *testing.T) {
	for _, action := range []string{"domains", "entries", "events"} {
		_, err := runQuery(t, newFakeQuery(), map[string]any{"action": action})
		if err == nil || !strings.Contains(err.Error(), "需要 project") {
			t.Fatalf("%s 缺 project 应报错: %v", action, err)
		}
	}
	if _, err := runQuery(t, newFakeQuery(), map[string]any{"action": "entry"}); err == nil ||
		!strings.Contains(err.Error(), "需要 id") {
		t.Fatalf("entry 缺 id 应报错: %v", err)
	}
	if _, err := runQuery(t, newFakeQuery(), map[string]any{"action": "event", "id": "不是数字"}); err == nil ||
		!strings.Contains(err.Error(), "必须是数字") {
		t.Fatalf("event 的 id 非数字应报错: %v", err)
	}
}

func TestQueryProjectNotFound(t *testing.T) {
	_, err := runQuery(t, newFakeQuery(), map[string]any{"action": "entries", "project": "ghost-svc"})
	if err == nil || !strings.Contains(err.Error(), "不存在") || !strings.Contains(err.Error(), "demo-svc") {
		t.Fatalf("项目不存在应报错并列出可用项目（帮模型自纠）: %v", err)
	}
	// 领域名同理
	if _, err := runQuery(t, newFakeQuery(), map[string]any{"action": "entries", "project": "demo-svc", "domain": "不存在的领域"}); err == nil ||
		!strings.Contains(err.Error(), "领域") {
		t.Fatalf("领域不存在应报错: %v", err)
	}
}

// ===================== ③ truncated（R2/AC4 的工具侧一半）=====================

func TestQueryEntriesTruncated(t *testing.T) {
	q := newFakeQuery()
	for i := 0; i < 60; i++ {
		q.entries = append(q.entries, store.ApiEntry{
			ID: fmt.Sprintf("big%d", i), ProjectID: "p1", DomainID: "d1",
			Name: fmt.Sprintf("批量接口%d", i), Method: "GET", PathRaw: fmt.Sprintf("/api/v1/demo/bulk%d", i),
			ParseState: "ok", Visibility: store.ApiVisibilityPublic, Status: store.ApiEntryStatusActive,
		})
	}
	out, err := runQuery(t, q, map[string]any{"action": "entries", "project": "demo-svc"})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if !strings.Contains(out, "truncated=true") || !strings.Contains(out, "共 62 条") || !strings.Contains(out, "前 50") {
		t.Fatalf("截断必须显式给 truncated+total:\n%s", out)
	}
	if !strings.Contains(out, "用 domain 或 keyword 缩小") || !strings.Contains(out, "不要把这里看到的当成全部接口") {
		t.Fatalf("截断必须给缩小指引（R2）:\n%s", out)
	}
	if got := strings.Count(out, "- [id="); got != 50 {
		t.Fatalf("默认应只返回 50 条摘要, got %d:\n%.200s", got, out)
	}
}

func TestQueryEventsCap20(t *testing.T) {
	q := newFakeQuery()
	for i := 0; i < 25; i++ {
		q.events = append(q.events, store.ApiChangeEvent{ID: int64(100 + i), ProjectID: "p1", Seq: 100 + i, Title: "批量事件"})
	}
	out, err := runQuery(t, q, map[string]any{"action": "events", "project": "demo-svc"})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if got := strings.Count(out, "- v"); got != 20 {
		t.Fatalf("events 上限 20（D2），got %d", got)
	}
	if !strings.Contains(out, "truncated=true") || !strings.Contains(out, "共 27 个事件") || !strings.Contains(out, "最近 20 个") {
		t.Fatalf("events 超限应显式说明:\n%s", out)
	}
}

func TestQueryEventItemsCap40(t *testing.T) {
	q := newFakeQuery()
	for i := 0; i < 45; i++ {
		q.event.Items = append(q.event.Items, EventItemView{
			Method: "PUT", Path: fmt.Sprintf("/api/v1/demo/x%d", i), EntryName: "批量改", ChangeType: "updated",
		})
	}
	// 第一条明细带 12 条 diff，验证单条 diff 也限 10 行并显式「从略」
	q.event.Items[0].Diff = nil
	for i := 0; i < 12; i++ {
		q.event.Items[0].Diff = append(q.event.Items[0].Diff, docspec.DiffItem{Section: "response.body", Key: fmt.Sprintf("f%d", i), Op: docspec.OpAdded})
	}
	out, err := runQuery(t, q, map[string]any{"action": "event", "id": "17"})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if !strings.Contains(out, "其余 6 条明细未展开（本事件共 46 条") {
		t.Fatalf("明细超 40 必须截断并说明（D2）:\n%.300s", out)
	}
	if !strings.Contains(out, "其余 2 条差异从略") {
		t.Fatalf("单条 diff 超 10 行也要显式从略:\n%.300s", out)
	}
}

// ===================== ④ D4/R4：脱敏与字符上限 =====================

func TestQueryMasksSecrets(t *testing.T) {
	// entry：curl 示例里的 Bearer token（接口示例天然带凭据——D4 的原始动机）
	out, err := runQuery(t, newFakeQuery(), map[string]any{"action": "entry", "id": "e1"})
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	if strings.Contains(out, "abcdefGHIJklmnopQRSTUVwx0123456789") {
		t.Fatalf("Bearer token 原文泄漏给模型:\n%s", out)
	}
	if !strings.Contains(out, "Bearer ***") {
		t.Fatalf("Bearer 应被掩码成 Bearer ***:\n%s", out)
	}
	// event：reason 里的 sk- 密钥
	out2, err := runQuery(t, newFakeQuery(), map[string]any{"action": "event", "id": "17"})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if strings.Contains(out2, "abcdefghijklmnopqrstuvwxyz123456") || !strings.Contains(out2, "sk-***") {
		t.Fatalf("sk- 密钥必须掩码:\n%s", out2)
	}
}

func TestQueryCharCap(t *testing.T) {
	q := newFakeQuery()
	q.entry.Description = strings.Repeat("长", 30000) // 远超 12000 字符预算
	out, err := runQuery(t, q, map[string]any{"action": "entry", "id": "e1"})
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	if !strings.Contains(out, "[内容过长已截断]") {
		t.Fatalf("字符上限截断必须显式说明（R4）:\n%.200s", out)
	}
	if n := utf8.RuneCountInString(out); n > maxReportChars+20 {
		t.Fatalf("单次返回超字符上限: %d > %d", n, maxReportChars)
	}
}

// ===================== ⑤ schema 形状与参数白名单 =====================

func TestQuerySchemaShape(t *testing.T) {
	reg := registerFakeQuery(t, &fakeDocs{}, newFakeQuery())
	sk, ok := reg.Get(SkillQuery)
	if !ok {
		t.Fatal("apidoc_query 未注册")
	}
	var schema map[string]any
	if err := json.Unmarshal(sk.Parameters(), &schema); err != nil {
		t.Fatalf("Parameters 非法 JSON: %v", err)
	}
	if schema["additionalProperties"] != false {
		t.Fatal("apidoc_query 必须 additionalProperties:false")
	}
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "action" {
		t.Fatalf("required = %v, want [action]", req)
	}
	props, _ := schema["properties"].(map[string]any)
	act, _ := props["action"].(map[string]any)
	enum, _ := act["enum"].([]any)
	want := []string{"projects", "domains", "entries", "entry", "events", "event"}
	if len(enum) != len(want) {
		t.Fatalf("action enum = %v, want 六枚举", enum)
	}
	for i, w := range want {
		if enum[i] != w {
			t.Fatalf("action enum[%d] = %v, want %s", i, enum[i], w)
		}
	}
	// 白名单闸门：schema 之外的键（例如塞正文）一律拒绝
	if _, err := execSkill(t, reg, SkillQuery, mustArgs(t, map[string]any{"action": "projects", "docText": " smuggled"})); err == nil ||
		!strings.Contains(err.Error(), "docText") {
		t.Fatalf("未知参数应被点名拒绝: %v", err)
	}
}

// TestQueryLimitClamped 模型乱传大 limit 的防御（照 pg.MaxRowLimit 范式，上限 200）。
func TestQueryLimitClamped(t *testing.T) {
	q := newFakeQuery()
	for i := 0; i < 260; i++ {
		q.entries = append(q.entries, store.ApiEntry{
			ID: fmt.Sprintf("x%d", i), ProjectID: "p1", Name: "接口", Method: "GET",
			PathRaw: fmt.Sprintf("/api/v1/demo/x%d", i), Visibility: store.ApiVisibilityPublic, Status: store.ApiEntryStatusActive,
		})
	}
	out, err := runQuery(t, q, map[string]any{"action": "entries", "project": "demo-svc", "limit": 99999})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if got := strings.Count(out, "- [id="); got > queryMaxEntriesLimit {
		t.Fatalf("limit 应被钳到 %d, got %d 行", queryMaxEntriesLimit, got)
	}
}

// TestQueryEmptyStates 空库/空命中也要给出路（模型不能拿着空转述就下结论）。
func TestQueryEmptyStates(t *testing.T) {
	q := newFakeQuery()
	q.projects = nil
	out, err := runQuery(t, q, map[string]any{"action": "projects"})
	if err != nil || !strings.Contains(out, "还没有任何项目") {
		t.Fatalf("空库应如实说明并指路导入: %q %v", out, err)
	}
	out2, err := runQuery(t, newFakeQuery(), map[string]any{"action": "entries", "project": "demo-svc", "keyword": "查无此路"})
	if err != nil || !strings.Contains(out2, "接口清单为空") || !strings.Contains(out2, "放宽办法") {
		t.Fatalf("空命中应给放宽指引: %q %v", out2, err)
	}
}
