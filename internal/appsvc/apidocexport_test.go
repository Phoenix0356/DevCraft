package appsvc

import (
	"strings"
	"testing"

	"DevCraft/internal/apidoc"
	"DevCraft/internal/store"
)

// ===================== 导出全量文档（库内当前态 → Markdown）=====================
//
// 这条链路的价值全在「round-trip」：导出文档发给前端后，前端可以拿它当 init 基线重导，
// 所以单测必须真的把导出文本喂回 Parse / CommitApiDocImport，而不是只看字符串长什么样。

func partialExportDoc() string {
	return "# 演示服务接口变更文档\n\n" +
		apiMetaBlock("demo-partial", "feature", "体检报告接口", "先占位，参数表待补", "REQ-9") +
		"## 一、体检管理\n\n" +
		"### 1.1 体检报告\n\n按 id 查询体检报告。\n\n" +
		"- **URL**：`GET /api/v1/demo/report/{id}`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		"#### 请求参数\n\n" +
		"#### 路径参数\n\n"
}

// mustExport 导出失败即 Fatal。
func mustExport(t *testing.T, svc *Service, projectID string) string {
	t.Helper()
	out, err := svc.ExportApiProjectDoc(projectID)
	if err != nil {
		t.Fatalf("ExportApiProjectDoc: %v", err)
	}
	return out
}

// assertRoundTrip 导出文本必须能被解析器以「零阻断」收回；wantEndpoints 为期望接口数。
func assertRoundTrip(t *testing.T, out string, wantEndpoints int) *apidoc.Document {
	t.Helper()
	doc := apidoc.Parse(out, apidoc.Options{})
	if doc.HasErrors() {
		t.Fatalf("导出文档重解析出现阻断项: %+v\n-----\n%s", doc.Errors(), out)
	}
	if len(doc.Endpoints) != wantEndpoints {
		t.Fatalf("导出文档接口数 = %d, want %d\n-----\n%s", len(doc.Endpoints), wantEndpoints, out)
	}
	return doc
}

func TestExportApiProjectDocSnapshot(t *testing.T) {
	svc := newTestService(t)
	pid := seedDemoBaseline(t, svc)
	// 第二次导入：2 新增 + 1 修改 + 1 显式删除（1.2 软删，快照里不该再出现）
	commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true})

	out := mustExport(t, svc, pid)

	active, err := svc.store.ListApiEntries(pid, "", false, true)
	if err != nil {
		t.Fatalf("ListApiEntries: %v", err)
	}
	doc := assertRoundTrip(t, out, len(active))
	if len(active) != 3 {
		t.Fatalf("用例前提失效：active 条目 = %d", len(active))
	}
	if warns := doc.Warnings(); len(warns) != 0 {
		t.Fatalf("导出文档重解析应零警告: %+v\n-----\n%s", warns, out)
	}

	// meta：快照一律 init 档，version_tag 取最新事件 label
	for _, want := range []string{
		"spec: 1\n", "project: demo-svc\n", "service: 演示服务\n", "base_path: /api/v1/demo\n",
		"scope: init\n", "version_tag: REQ-1\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("导出 meta 缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "change_type:") {
		t.Fatalf("快照 meta 不应携带变更声明:\n%s", out)
	}

	// 头部「本次更新」= seq 最大的事件（v2 密码策略升级），而不是 v1
	if !strings.Contains(out, "## 本次更新\n") ||
		!strings.Contains(out, "密码策略升级") ||
		!strings.Contains(out, "安全要求旧密码必填并新增登录日志接口") {
		t.Fatalf("本次更新章节不对:\n%s", out)
	}
	if !strings.Contains(out, "（v2 · ") {
		t.Fatalf("本次更新应标注最新事件版本号:\n%s", out)
	}
	if !strings.Contains(out, "- DELETE /api/v1/demo/user/{id} —— 删除") {
		t.Fatalf("涉及接口清单应含删除项:\n%s", out)
	}

	// 软删条目不进快照正文，但作为变更明细留在「本次更新」清单里
	if doc.FindEndpoint("DELETE|/api/v1/demo/user/{}") != nil {
		t.Fatal("已删除接口不应出现在快照正文")
	}
	if doc.FindEndpoint("POST|/api/v1/demo/password") == nil {
		t.Fatal("修改密码接口未往返")
	}
	// 修改后的字段状态来自结构化行，而不是陈旧的 raw_markdown
	pw := doc.FindEndpoint("POST|/api/v1/demo/password")
	if len(pw.RequestFields) != 2 || pw.RequestFields[0].Required != apidoc.RequiredYes {
		t.Fatalf("oldPwd 应已改为必填（结构化行为准）: %+v", pw.RequestFields)
	}
}

// TestExportApiProjectDocReimportsAsBaseline 端到端：真实 golden 文档 init 入库 → 导出 →
// 再 init 导入到新项目 → 身份键集合必须与源项目逐条相等。
// 这一条同时钉住「导出文档可以当基线发给前端」这个核心承诺。
func TestExportApiProjectDocReimportsAsBaseline(t *testing.T) {
	svc := newTestService(t)
	src := commitApiDoc(t, svc, ApiDocImportRequest{
		DocText: readApiFixture(t, "user-service.md"), ProjectName: "export-src", InitMode: true})
	out := mustExport(t, svc, src.ProjectID)

	srcEntries, err := svc.store.ListApiEntries(src.ProjectID, "", false, true)
	if err != nil {
		t.Fatalf("ListApiEntries: %v", err)
	}
	doc := assertRoundTrip(t, out, len(srcEntries))
	if len(srcEntries) != 20 {
		t.Fatalf("用例前提失效：golden 条目 = %d, want 20", len(srcEntries))
	}

	dst := commitApiDoc(t, svc, ApiDocImportRequest{DocText: out, ProjectName: "export-dst", InitMode: true})
	dstEntries, err := svc.store.ListApiEntries(dst.ProjectID, "", false, true)
	if err != nil {
		t.Fatalf("ListApiEntries: %v", err)
	}
	if len(dstEntries) != len(srcEntries) {
		t.Fatalf("重导基线条目数 = %d, want %d（%s）", len(dstEntries), len(srcEntries), doc.Mode)
	}
	got := map[string]bool{}
	for _, e := range dstEntries {
		got[e.IdentityKey] = true
	}
	for _, e := range srcEntries {
		if !got[e.IdentityKey] {
			t.Fatalf("重导后身份键丢失: %s", e.IdentityKey)
		}
		delete(got, e.IdentityKey)
	}
	for k := range got {
		t.Fatalf("重导多出了条目: %s", k)
	}
	// 领域/命名结构/示例也得跟着过去：至少一个领域、一个命名结构、一个示例
	if len(doc.Domains) == 0 {
		t.Fatal("导出文档没有领域章节")
	}
	if len(doc.Schemas) == 0 {
		t.Fatal("导出文档没有命名结构")
	}
	nExamples := 0
	for _, e := range doc.Endpoints {
		nExamples += len(e.Examples)
	}
	if nExamples == 0 {
		t.Fatal("导出文档没有示例/curl")
	}
}

func TestExportApiProjectDocNoEventsOmitSection(t *testing.T) {
	svc := newTestService(t)
	res := commitApiDoc(t, svc, ApiDocImportRequest{
		DocText: readApiFixture(t, "user-service.md"), ProjectName: "export-noevent", InitMode: true})
	if apiEventCount(t, svc, res.ProjectID) != 0 {
		t.Fatal("用例前提失效：init 导入不应产生事件")
	}
	out := mustExport(t, svc, res.ProjectID)
	if strings.Contains(out, "## 本次更新") || strings.Contains(out, "version_tag:") {
		t.Fatalf("无变更事件时「本次更新」与 version_tag 应整节省略:\n%s", out[:min(len(out), 400)])
	}
}

// TestExportApiProjectDocPartialFidelity 库里缺的信息导出时不粉饰：
// partial 条目重导后依旧 partial，槽位逐一对应。
func TestExportApiProjectDocPartialFidelity(t *testing.T) {
	svc := newTestService(t)
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: partialExportDoc()})
	e := mustEntryByIdentity(t, svc, res.ProjectID, "GET|/api/v1/demo/report/{}")
	if e.ParseState != store.ApiParseStatePartial || len(e.Unresolved) != 3 {
		t.Fatalf("用例前提失效：条目应 partial 且缺 3 个槽位: %s %v", e.ParseState, e.Unresolved)
	}

	out := mustExport(t, svc, res.ProjectID)
	doc := assertRoundTrip(t, out, 1)
	got := doc.Endpoints[0]
	if got.ParseState != apidoc.ParsePartial {
		t.Fatalf("partial 条目重解析后应仍为 partial，实际 %s:\n%s", got.ParseState, out)
	}
	for _, slot := range []string{"request_params", "path_params", "response_params"} {
		found := false
		for _, s := range got.Unresolved {
			if s == slot {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺失槽位 %s 未忠实往返: %v\n%s", slot, got.Unresolved, out)
		}
	}
	if strings.Contains(out, "无。") {
		t.Fatalf("槽位缺失不能写成「无。」（那是明确无参数）:\n%s", out)
	}
}

func TestExportApiProjectDocErrors(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.ExportApiProjectDoc("  "); err == nil || !strings.Contains(err.Error(), "请先选择接口项目") {
		t.Fatalf("空项目 id 应提示选择项目，实际: %v", err)
	}
	if _, err := svc.ExportApiProjectDoc("no-such-project"); err == nil ||
		!strings.Contains(err.Error(), "接口项目不存在") {
		t.Fatalf("不存在的项目应报中文错误，实际: %v", err)
	}
	if err := svc.store.CreateApiProject(&store.ApiProject{Name: "empty-svc", Display: "空项目"}); err != nil {
		t.Fatalf("CreateApiProject: %v", err)
	}
	projects, err := svc.store.ListApiProjects()
	if err != nil {
		t.Fatalf("ListApiProjects: %v", err)
	}
	var pid string
	for _, pr := range projects {
		if pr.Name == "empty-svc" {
			pid = pr.ID
		}
	}
	if pid == "" {
		t.Fatal("空项目未建出来")
	}
	if _, err := svc.ExportApiProjectDoc(pid); err == nil || !strings.Contains(err.Error(), "还没有可导出的接口") {
		t.Fatalf("零接口项目应拒导出，实际: %v", err)
	}
}
