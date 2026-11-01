package apidoc

import (
	"strings"
	"testing"
)

// ===================== 导出渲染单测 =====================
//
// 两条主线：
//   - 形态：输出文本的结构（表头列数、小节三态、示例归组、命名结构落位）逐条钉住；
//   - round-trip：RenderExport 的产物必须能被 Parse 以「零阻断 + 零警告」收回，
//     且接口/字段/示例/命名结构/领域 internal 标记逐项对得上——导出文档因此可以直接
//     当作新项目的 init 基线用，这是「发给前端」之外的第二条用途。

func richEndpoint() *Endpoint {
	return &Endpoint{
		Name:        "新增订阅",
		Method:      "POST",
		PathRaw:     "/api/v1.0/user/subscription",
		ContentType: "application/json",
		Auth:        AuthLogin,
		Description: "为当前用户新增一条 Arxiv 订阅。",
		RequestFields: []Field{
			{Name: "topCategory", TypeRaw: "String", Required: RequiredNo, Position: PosBody, Desc: "Arxiv 顶级分类"},
			{Name: "source", TypeRaw: "String", Required: RequiredUnknown, Position: PosBody, Default: "web", Desc: "来源渠道"},
		},
		ResponseFields: []Field{
			{Name: "message", TypeRaw: "String", Desc: "响应消息"},
			{Name: "data", TypeRaw: "SubscriptionVO", Desc: "订阅信息"},
		},
		Schemas: []SchemaDef{{
			Name:   "SubscriptionVO",
			Fields: []Field{{Name: "id", TypeRaw: "String", Desc: "订阅ID"}},
		}},
		Examples: []Example{
			{Kind: ExampleRequest, Language: "json", Content: "{\"topCategory\":\"cs\"}\n", Sort: 0},
			{Kind: ExampleMultipart, Language: "", Content: "Content-Type: multipart/form-data\nContent-Disposition: form-data; name=\"f\"", Sort: 1},
			{Kind: ExampleResponse, Language: "json", Content: "{\"message\":\"ok\"}", Sort: 2},
			{Kind: ExampleError, Language: "json", Content: "{\"code\":401,\"msg\":\"未登录\"}", Sort: 3},
			{Kind: ExampleCURL, Language: "bash", Content: "curl -X POST 'https://example.com/api/v1.0/user/subscription'", Sort: 4},
		},
	}
}

func pathEndpoint() *Endpoint {
	return &Endpoint{
		Name:       "查询订阅",
		Method:     "GET",
		PathRaw:    "/api/v1.0/user/subscription/{id}",
		PathFields: []Field{{Name: "id", TypeRaw: "Long", Required: RequiredYes, Position: PosPath, Desc: "订阅ID"}},
		ResponseFields: []Field{
			{Name: "message", TypeRaw: "String", Desc: "响应消息"},
		},
	}
}

func richExportInput() ExportInput {
	return ExportInput{
		Title:      "用户服务 接口文档",
		ExportedAt: "2026-11-01 10:00:00",
		Total:      3,
		Meta:       Meta{Project: "paper-manager", Service: "用户服务", BasePath: "/api/v1.0", VersionTag: "REQ-1024"},
		Update: &ExportUpdate{
			Seq: 7, Label: "REQ-1024", Title: "新增论文下载记录同步接口",
			Reason: "前端需要跨端展示下载历史", CreatedAt: "2026-11-01 09:59",
			Items: []ExportUpdateItem{
				{Method: "POST", Path: "/api/v1.0/user/subscription", EntryName: "新增订阅", ChangeType: "added", ChangePoint: "新增接口"},
				{Method: "GET", Path: "/api/v1.0/user/subscription/{id}", ChangeType: "updated"},
			},
		},
		Domains: []ExportDomain{
			{Name: "订阅管理", Entries: []*Endpoint{richEndpoint(), pathEndpoint()}},
			{Name: "RPC 论文写入", Internal: true, NotesRaw: "仅限内部服务调用。", Entries: []*Endpoint{{
				Name: "写入论文", Method: "PUT", PathRaw: "/api/v1.0/rpc/paper",
				RequestFields: []Field{{Name: "paperId", TypeRaw: "Long", Required: RequiredYes, Position: PosBody}},
			}}},
		},
	}
}

// updateChapter 切出「## 本次更新」到下一个 ## 之间的文本，便于单独断言。
func updateChapter(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "## 本次更新\n")
	if i < 0 {
		t.Fatalf("导出文本缺少「## 本次更新」章节:\n%s", out)
	}
	rest := out[i:]
	if j := strings.Index(rest[len("## 本次更新\n"):], "\n## "); j >= 0 {
		return rest[:len("## 本次更新\n")+j]
	}
	return rest
}

func TestRenderExportHeaderAndMeta(t *testing.T) {
	out := RenderExport(richExportInput())
	if !strings.HasPrefix(out, "# 用户服务 接口文档\n\n```apidoc-meta\nspec: 1\nproject: paper-manager\n") {
		t.Fatalf("文档头不符合预期:\n%s", out[:min(len(out), 240)])
	}
	for _, want := range []string{
		"service: 用户服务\n",
		"base_path: /api/v1.0\n",
		"scope: init\n",
		"version_tag: REQ-1024\n",
		"> 由 DevCraft 生成 · 2026-11-01 10:00:00 · 共 3 个接口\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("导出头缺少 %q:\n%s", want, out)
		}
	}
	// 快照不是变更声明：meta 里绝不出现 change_* （否则重导会被要求逐接口声明变更）
	for _, ban := range []string{"change_type:", "change_title:", "change_note:"} {
		if strings.Contains(out, ban) {
			t.Fatalf("导出 meta 不应包含 %q", ban)
		}
	}
}

func TestRenderExportMetaOptionalKeysOmitted(t *testing.T) {
	out := RenderExport(ExportInput{
		Title: "T",
		Meta:  Meta{Project: "p"},
		Domains: []ExportDomain{{Name: "D", Entries: []*Endpoint{
			{Name: "e", Method: "GET", PathRaw: "/d/e"},
		}}},
	})
	for _, ban := range []string{"service:", "base_path:", "version_tag:", "> 由 DevCraft 生成"} {
		if strings.Contains(out, ban) {
			t.Fatalf("空字段不应渲染出 %q:\n%s", ban, out)
		}
	}
	if !strings.Contains(out, "scope: init\n") {
		t.Fatalf("scope: init 必须始终渲染:\n%s", out)
	}
}

func TestRenderExportUpdateChapter(t *testing.T) {
	out := updateChapter(t, RenderExport(richExportInput()))
	for _, want := range []string{
		"## 本次更新\n",
		"**REQ-1024：新增论文下载记录同步接口**（v7 · 2026-11-01 09:59）",
		"前端需要跨端展示下载历史",
		"本次涉及接口：",
		"- POST /api/v1.0/user/subscription —— 新增（新增订阅）：新增接口",
		"- GET /api/v1.0/user/subscription/{id} —— 修改",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("本次更新章节缺少 %q:\n%s", want, out)
		}
	}
	// 关键：本章节内绝不出现 ###。无 URL 的 ### 会落 Unparsed 并发 W-UNPARSED-BLOCK，
	// 而 ## 章节的 preamble 散文/列表不产生任何 Issue（round-trip 零警告的前提）。
	if strings.Contains(out, "### ") {
		t.Fatalf("本次更新章节内不得出现 ###:\n%s", out)
	}
}

func TestRenderExportUpdateOmittedWhenNil(t *testing.T) {
	in := richExportInput()
	in.Update = nil
	in.Meta.VersionTag = "" // 服务层只在有事件时把 label 写进 version_tag
	out := RenderExport(in)
	if strings.Contains(out, "## 本次更新") {
		t.Fatalf("无变更事件时应整节省略:\n%s", out)
	}
	if strings.Contains(out, "version_tag:") {
		t.Fatalf("无事件时不应写 version_tag:\n%s", out)
	}
}

func TestRenderExportEndpointShapes(t *testing.T) {
	out := RenderExport(richExportInput())
	// 请求参数：6 列规范表；Required=Unknown → 必填格留空（不伪造「否」）
	if !strings.Contains(out, "| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n") {
		t.Fatalf("请求参数应为 6 列规范表:\n%s", out)
	}
	if !strings.Contains(out, "| source | String |  | Body | web | 来源渠道 |\n") {
		t.Fatalf("未知必填应渲染成空格而非「否」:\n%s", out)
	}
	// 返回参数：3 列（位置/必填对响应字段无意义）
	if !strings.Contains(out, "| 参数名 | 类型 | 说明 |\n") ||
		!strings.Contains(out, "| data | SubscriptionVO | 订阅信息 |\n") {
		t.Fatalf("返回参数应为 3 列表:\n%s", out)
	}
	// 路径参数小节只在真有路径字段时出现
	if !strings.Contains(out, "#### 路径参数\n\n| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n") {
		t.Fatalf("路径参数小节缺失或表头不对:\n%s", out)
	}
	// 命名结构落在接口块末尾（粗体标题表按块归属，小节外摆放不触发 Issue）
	if !strings.Contains(out, "**SubscriptionVO 结构**：\n\n| 参数名 | 类型 | 说明 |\n") {
		t.Fatalf("命名结构应渲染为块尾粗体标题表:\n%s", out)
	}
	// 请求示例 + multipart 原始报文共用一个小节标题，两个围栏
	reqSec := sectionBody(t, out, "#### 请求示例")
	if got := strings.Count(out, "#### 请求示例"); got != 1 {
		t.Fatalf("请求示例小节只应出现一次（重复同名小节会互相覆盖槽位），实际 %d 次", got)
	}
	if !strings.Contains(reqSec, "```json\n{\"topCategory\":\"cs\"}\n```\n") ||
		!strings.Contains(reqSec, "```\nContent-Type: multipart/form-data") {
		t.Fatalf("请求示例小节应含 json 围栏与无标记 multipart 围栏:\n%s", reqSec)
	}
	// internal 领域复原「（内部）」限定语；章级 preamble 原样输出
	if !strings.Contains(out, "## RPC 论文写入（内部）\n\n仅限内部服务调用。\n") {
		t.Fatalf("internal 领域标题/preamble 不对:\n%s", out)
	}
	// 变更/breaking 声明不进快照
	if strings.Contains(out, "- **变更**") || strings.Contains(out, "- **breaking**") {
		t.Fatalf("快照文档不应携带增量变更声明")
	}
}

// sectionBody 取某个 #### 小节的正文：从标题行往下，遇到下一个标题（## / ####）或
// 粗体命名结构标题即止。
func sectionBody(t *testing.T, out, heading string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("缺少小节 %q:\n%s", heading, out)
	}
	var body []string
	for _, l := range lines[start+1:] {
		s := strings.TrimSpace(l)
		if strings.HasPrefix(s, "##") || strings.HasPrefix(s, "**") {
			break
		}
		body = append(body, l)
	}
	return strings.Join(body, "\n")
}

// TestRenderExportParamSectionTriState 参数小节三态的渲染侧规则：
// 有字段 → 表；槽位在 Unresolved → 小节存在但空（重导依旧 unresolved，partial 忠实往返）；
// 其余 → 「无。」（明确无参数）。
func TestRenderExportParamSectionTriState(t *testing.T) {
	ep := &Endpoint{
		Name:       "无参数接口",
		Method:     "GET",
		PathRaw:    "/api/v1.0/user/ping",
		Unresolved: []string{"response_params"},
	}
	out := RenderExport(ExportInput{Title: "T", Meta: Meta{Project: "p"},
		Domains: []ExportDomain{{Name: "D", Entries: []*Endpoint{ep}}}})
	reqSec := sectionBody(t, out, "#### 请求参数")
	if strings.TrimSpace(reqSec) != "无。" {
		t.Fatalf("请求参数无字段且未缺失应渲染「无。」，实际 %q", reqSec)
	}
	respSec := sectionBody(t, out, "#### 返回参数")
	if strings.TrimSpace(respSec) != "" {
		t.Fatalf("Unresolved 槽位应渲染成空小节（不编造内容），实际 %q", respSec)
	}
	if strings.Contains(out, "#### 路径参数") {
		t.Fatalf("无路径字段不应输出路径参数小节:\n%s", out)
	}
}

func TestRenderExportRoundTripReparse(t *testing.T) {
	out := RenderExport(richExportInput())
	doc := Parse(out, Options{})
	if doc.HasErrors() {
		t.Fatalf("导出文档重导必须零阻断，实际: %+v\n-----\n%s", doc.Errors(), out)
	}
	if doc.Mode != ModeInit {
		t.Fatalf("meta.scope 应生效为 init，实际 %s", doc.Mode)
	}
	if warns := doc.Warnings(); len(warns) != 0 {
		t.Fatalf("导出文档重导应零警告，实际: %+v\n-----\n%s", warns, out)
	}
	if len(doc.Endpoints) != 3 {
		t.Fatalf("接口数 = %d, want 3", len(doc.Endpoints))
	}
	if doc.BasePath != "/api/v1.0" || doc.Meta.VersionTag != "REQ-1024" {
		t.Fatalf("meta 往返不对: %+v", doc.Meta)
	}
	// 「本次更新」是 ## 章节但不含 ###，因此不该被认成领域
	if len(doc.Domains) != 2 {
		var names []string
		for i := range doc.Domains {
			names = append(names, doc.Domains[i].Name)
		}
		t.Fatalf("领域数 = %d %v, want 2（本次更新章节不得成领域）", len(doc.Domains), names)
	}
	if !doc.Domains[1].Internal || doc.Domains[1].Name != "RPC 论文写入" {
		t.Fatalf("internal 标记往返不对: %+v", doc.Domains[1])
	}
	if doc.Domains[0].NotesRaw != "" || !strings.Contains(doc.Domains[1].NotesRaw, "仅限内部服务调用") {
		t.Fatalf("章级 preamble 往返不对: %q / %q", doc.Domains[0].NotesRaw, doc.Domains[1].NotesRaw)
	}

	ep := doc.FindEndpoint("POST|/api/v1.0/user/subscription")
	if ep == nil {
		t.Fatalf("身份键未往返: %v", identityKeys(doc))
	}
	if ep.ParseState != ParseOK || len(ep.Unresolved) != 0 {
		t.Fatalf("齐全接口应 parse_state=ok: %s %v", ep.ParseState, ep.Unresolved)
	}
	if ep.ContentType != "application/json" || ep.Auth != AuthLogin ||
		ep.Description != "为当前用户新增一条 Arxiv 订阅。" {
		t.Fatalf("元信息/bullet 往返不对: %+v", ep)
	}
	if len(ep.RequestFields) != 2 || ep.RequestFields[1].Required != RequiredUnknown ||
		ep.RequestFields[1].Default != "web" {
		t.Fatalf("请求字段往返不对（含必填未知态）: %+v", ep.RequestFields)
	}
	if len(ep.ResponseFields) != 2 || ep.ResponseFields[1].TypeRaw != "SubscriptionVO" {
		t.Fatalf("返回字段往返不对: %+v", ep.ResponseFields)
	}
	if len(ep.Schemas) != 1 || ep.Schemas[0].Name != "SubscriptionVO" ||
		len(ep.Schemas[0].Fields) != 1 {
		t.Fatalf("命名结构往返不对: %+v", ep.Schemas)
	}
	if len(doc.Schemas) != 1 || doc.Schemas[0].OwnerScope != ep.IdentityKey {
		t.Fatalf("命名结构 registry owner_scope 应为宿主接口: %+v", doc.Schemas)
	}
	if got := len(ep.Examples); got != 5 {
		t.Fatalf("示例数 = %d, want 5（multipart 不能被吞）: %+v", got, ep.Examples)
	}
	if kinds := exampleKinds(ep); kinds[ExampleMultipart] != 1 || kinds[ExampleCURL] != 1 ||
		kinds[ExampleRequest] != 1 || kinds[ExampleResponse] != 1 || kinds[ExampleError] != 1 {
		t.Fatalf("示例 kind 往返不对: %+v", kinds)
	}

	// 占位符在身份键里折叠为 {}，但 PathRaw 必须原样往返（显示保真）
	pf := doc.FindEndpoint("GET|/api/v1.0/user/subscription/{}")
	if pf == nil || pf.PathRaw != "/api/v1.0/user/subscription/{id}" {
		t.Fatalf("路径参数往返不对: %+v %v", pf, identityKeys(doc))
	}
	if len(pf.PathFields) != 1 || pf.PathFields[0].Position != PosPath {
		t.Fatalf("路径参数字段往返不对: %+v", pf.PathFields)
	}
	if pf.ReqParamsState != SectionEmpty {
		t.Fatalf("无请求参数应回到 SectionEmpty，实际 %s", pf.ReqParamsState)
	}
}

// TestRenderExportRoundTripPartialFidelity 缺口不粉饰：导出时没有的信息不落「无。」，
// 重导后该槽位依旧 Unresolved、parse_state 依旧 partial。
func TestRenderExportRoundTripPartialFidelity(t *testing.T) {
	ep := &Endpoint{
		Name:       "体检报告",
		Method:     "GET",
		PathRaw:    "/api/v1.0/user/report",
		Unresolved: []string{"request_params", "response_params"},
	}
	out := RenderExport(ExportInput{Title: "T", Meta: Meta{Project: "p", BasePath: "/api/v1.0/user"},
		Domains: []ExportDomain{{Name: "D", Entries: []*Endpoint{ep}}}})
	doc := Parse(out, Options{})
	if doc.HasErrors() {
		t.Fatalf("partial 导出重导必须零阻断: %+v\n%s", doc.Errors(), out)
	}
	got := doc.FindEndpoint("GET|/api/v1.0/user/report")
	if got == nil {
		t.Fatalf("接口未入库: %v", identityKeys(doc))
	}
	if got.ParseState != ParsePartial {
		t.Fatalf("partial 接口重导后应仍为 partial，实际 %s (%v)", got.ParseState, got.Unresolved)
	}
	if !hasSlot(got.Unresolved, "request_params") || !hasSlot(got.Unresolved, "response_params") {
		t.Fatalf("缺失槽位未忠实往返: %v", got.Unresolved)
	}
}

func TestRenderExportDeterministic(t *testing.T) {
	in := richExportInput()
	if a, b := RenderExport(in), RenderExport(in); a != b {
		t.Fatal("同一输入两次渲染字节不一致")
	}
}

func identityKeys(d *Document) []string {
	var out []string
	for _, e := range d.Endpoints {
		out = append(out, e.IdentityKey)
	}
	return out
}

func exampleKinds(ep *Endpoint) map[string]int {
	m := map[string]int{}
	for _, x := range ep.Examples {
		m[x.Kind]++
	}
	return m
}

func hasSlot(slots []string, want string) bool {
	for _, s := range slots {
		if s == want {
			return true
		}
	}
	return false
}
