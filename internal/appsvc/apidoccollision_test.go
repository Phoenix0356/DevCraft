package appsvc

// 10-21 修复「命名结构撞名导致的假破坏性差异」的编排层测试（三条各钉一端）：
//   ① TestApiDocImportPagingCollisionFixture —— 真实片段 fixture（internal/apidoc/testdata/
//      paperpilot-paging-collision.md，摘自用户 PaperPilot-接口文档-v1.md）：修复前该片段
//      基线导入后库里只剩一行 FavoriteVO（分页字段把 item 字段覆盖掉），按原样重导整批拒收
//      E-BREAKING-DENIED + E-DIFF-MISMATCH-UNCHANGED + E-UPDATED-NO-DIFF 与 2×W-STRUCT-DIVERGED；
//      修复后两套结构各自入库、预览零 E 零歧义警告、事件 diff 只含真实字段变化；
//   ② TestApiDocImportStructAmbiguousNotAuthorViolation —— 人为「真同名不同构」（AC2/D2）：
//      仍报 W-STRUCT-DIVERGED、仍需人工确认，但两条违约闸（1.1 修改 + breaking:false / 1.2 未变更）
//      都不许因歧义噪声判作者撒谎；
//   ③ TestApiDocImportRealBreakingStillDenied —— AC5 防退化：无歧义的真破坏（删响应字段）
//      声明 breaking: false 必须照旧被 E-BREAKING-DENIED 拒收，闸门不许修哑。
//
// 复用 apidoc_test.go 的文档构造助手与 readApiFixture（同一批字节，解析层与编排层共享 fixture）。

import (
	"strings"
	"testing"

	"DevCraft/internal/apidoc"
	"DevCraft/internal/store"
)

// apiSchemaNamesIn 命名结构名列表（断言失败时把实际名字打出来）。
func apiSchemaNamesIn(schemas []store.ApiSchema) []string {
	out := make([]string, 0, len(schemas))
	for _, sc := range schemas {
		out = append(out, sc.Name)
	}
	return out
}

func TestApiDocImportPagingCollisionFixture(t *testing.T) {
	svc := newTestService(t)
	fx := readApiFixture(t, "paperpilot-paging-collision.md")
	const project = "collision-fixture"

	// —— 1) 库里先铺基线：同一份真实片段，把三条变更声明改成「新增」入库 ——
	// 用户真实流程是先导旧版基线文档再导 v1 增量；fixture 自带的是「修改/未变更」声明，
	// 首次入库时库里没有对照，只能先铺基线（除声明行外一字未改）。
	baseline := strings.ReplaceAll(fx, "- **变更**：修改\n", "- **变更**：新增\n")
	baseline = strings.ReplaceAll(baseline, "- **变更**：未变更\n", "- **变更**：新增\n")
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: baseline, ProjectName: project})
	projectID := res.ProjectID
	if res.Stats.Added != 3 {
		t.Fatalf("基线导入新增条目 = %d, want 3: %+v", res.Stats.Added, res.Stats)
	}

	// 分页表与 item 表各自作为独立命名结构挂在接口下（AC1 后半）：
	// 修复前两条都叫 FavoriteVO → api_schemas 唯一键 (project_id, name, owner_scope) 上互相覆盖，
	// 库里只剩分页那一套字段，item 字段凭空消失。
	fav := mustEntryByIdentity(t, svc, projectID, "GET|/api/v1.0/user/favorite")
	schemas, err := svc.store.ListApiSchemasByOwner(projectID, fav.ID)
	if err != nil {
		t.Fatalf("ListApiSchemasByOwner: %v", err)
	}
	if len(schemas) != 2 {
		t.Fatalf("收藏列表的命名结构数 = %d %v, want 2（FavoriteVO + FavoriteVO 分页）", len(schemas), apiSchemaNamesIn(schemas))
	}
	byName := map[string]store.ApiSchema{}
	for _, sc := range schemas {
		byName[sc.Name] = sc
	}
	item, page := byName["FavoriteVO"], byName["FavoriteVO 分页"]
	if item.ID == "" || page.ID == "" {
		t.Fatalf("两套结构名不对: %v", apiSchemaNamesIn(schemas))
	}
	if item.Ambiguous || page.Ambiguous {
		t.Fatalf("分页表与 item 表不撞名后不得标 ambiguous: %+v %+v", item, page)
	}
	if item.BodyHash == page.BodyHash {
		t.Fatal("两套字段的 body_hash 必须不同")
	}
	itemFields, err := svc.store.ListApiFields(store.ApiOwnerSchema, item.ID)
	if err != nil {
		t.Fatalf("ListApiFields(item): %v", err)
	}
	pageFields, err := svc.store.ListApiFields(store.ApiOwnerSchema, page.ID)
	if err != nil {
		t.Fatalf("ListApiFields(page): %v", err)
	}
	if len(itemFields) != 7 || len(pageFields) != 5 {
		t.Fatalf("字段数 = %d / %d, want 7 / 5（item 与分页各自独立，不合并）", len(itemFields), len(pageFields))
	}
	for _, f := range itemFields {
		if f.Name == "records" || f.Name == "total" || f.Name == "pages" {
			t.Fatalf("item 结构被分页字段污染: %+v", f)
		}
	}
	for _, f := range pageFields {
		if f.Name == "title" || f.Name == "arxivId" || f.Name == "doi" {
			t.Fatalf("分页结构被 item 字段污染: %+v", f)
		}
	}

	// —— 2) 按文档原样再导一次（9.1/9.2 声明 修改 + breaking:false，13.2 声明 未变更）——
	// AC1 逐字钉住：这份 fixture 的预览里 0 个 E-BREAKING-DENIED、0 个 E-DIFF-MISMATCH-UNCHANGED、
	// 0 个 W-STRUCT-DIVERGED，且没有任何「records/total/size/current/pages 被删除」的假差异。
	//
	// 实测对照（本次变异跑出来的原始输出，恢复旧剥名 + 关闭 D2 降级 = 修复前完整形态）：
	//   E-BREAKING-DENIED(第19行 GET /api/v1.0/user/favorite) breaking=1
	//   E-DIFF-MISMATCH-UNCHANGED(第164行 GET /api/v1.0/user/feedback)
	//     「response.body FeedbackVO.id added、FeedbackVO.paperId added、… records removed、
	//       total removed、size removed、current removed、pages removed」
	//   E-UPDATED-NO-DIFF(第86行 POST /api/v1.0/user/favorite)
	//   2 × W-STRUCT-DIVERGED（FavoriteVO / FeedbackVO）
	//   且基线导入后库里只剩 1 行 FavoriteVO（分页那套字段把 item 那套覆盖掉了）
	//
	// commitApiDoc 任何 error 都直接 t.Fatalf，所以「整批通过」本身就是断言。
	// 字段级差异为零 → 不发事件；只有「声明行从新增变回修改」这类原文措辞差异需要同步（AC4 同源）。
	pv := previewApiDocOrFail(t, svc, ApiDocImportRequest{DocText: fx, ProjectName: project})
	for _, code := range []string{apidoc.ErrBreakingDenied, apidoc.ErrDiffMismatchUnc} {
		if hasIssueCode(pv.Issues, code) {
			t.Fatalf("AC1：真实片段不得再产出 %s: %+v", code, pv.Issues)
		}
	}
	if hasIssueCode(pv.Issues, apidoc.WarnStructDiverged) {
		t.Fatalf("AC1：分页表与 item 表不撞名后不得有 W-STRUCT-DIVERGED: %+v", pv.Issues)
	}
	if !pv.OK {
		t.Fatalf("预览必须通过（用户那份文档重新导入的前提）: %+v", pv.Issues)
	}
	if pv.Breaking != store.ApiBreakingNo {
		t.Fatalf("AC1：聚合 breaking = %d, want 0（假破坏归零）", pv.Breaking)
	}
	for _, c := range pv.Changes {
		if n := fakePagingNoise(c.Diff); n != 0 {
			t.Fatalf("%s %s 预览里仍有 %d 条分页字段假差异: %+v", c.Method, c.Path, n, c.Diff)
		}
	}
	eventsBefore := apiEventCount(t, svc, projectID)
	again := commitApiDoc(t, svc, ApiDocImportRequest{DocText: fx, ProjectName: project})
	if again.EventID != 0 {
		t.Fatalf("与库中定义逐字对齐的文档不应生成事件: %+v", again)
	}
	if again.Stats.Unchanged != 3 || again.Stats.Updated != 0 || again.Stats.Added != 0 {
		t.Fatalf("stats = %+v, want 3 条全部未变更", again.Stats)
	}
	if again.Breaking != store.ApiBreakingNo {
		t.Fatalf("breaking = %d, want 0（假破坏已随撞名一起消失）", again.Breaking)
	}
	if !strings.Contains(again.Message, "未生成事件") {
		t.Fatalf("结果应说明未生成事件（仅同步条目原文）: %q", again.Message)
	}
	if got := apiEventCount(t, svc, projectID); got != eventsBefore {
		t.Fatalf("事件数变了: %d → %d", eventsBefore, got)
	}
	if n := len(apiAllEntries(t, svc, projectID)); n != 3 {
		t.Fatalf("条目数 = %d, want 3（零新增零删除）", n)
	}
	var stale int
	for _, note := range again.Notes {
		if strings.Contains(note, "未算出变化") {
			stale++
		}
		if strings.Contains(note, "同名不同构") {
			t.Fatalf("撞名修复后不应再出现歧义降级提示: %q", note)
		}
	}
	if stale != 2 {
		t.Fatalf("陈旧声明提示 = %d, want 2（两条「修改」算不出差异）: %v", stale, again.Notes)
	}

	// —— 3) AC4：真实新增一个响应字段（两处内联 FavoriteVO 同时加）→ 事件 diff 只含真实变化 ——
	real := strings.ReplaceAll(fx,
		"| createTime | String | 收藏时间（ISO 8601） |",
		"| createTime | String | 收藏时间（ISO 8601） |\n| abstract | String | 摘要（本轮真实新增字段） |")
	res3 := commitApiDoc(t, svc, ApiDocImportRequest{DocText: real, ProjectName: project})
	if res3.EventID == 0 {
		t.Fatalf("真实字段新增应生成事件: %+v", res3)
	}
	if res3.Breaking != store.ApiBreakingNo {
		t.Fatalf("响应新增字段是非破坏，聚合 breaking = %d, want 0", res3.Breaking)
	}
	if res3.Stats.Updated != 2 || res3.Stats.Unchanged != 1 {
		t.Fatalf("stats = %+v, want updated 2 / unchanged 1", res3.Stats)
	}
	detail, err := svc.GetApiChangeEvent(res3.EventID)
	if err != nil {
		t.Fatalf("GetApiChangeEvent: %v", err)
	}
	if len(detail.Items) != 2 {
		t.Fatalf("事件明细数 = %d, want 2", len(detail.Items))
	}
	for _, it := range detail.Items {
		if len(it.Diff) != 1 {
			t.Fatalf("%s %s 的 diff 应恰好 1 条真实变化，实际 %+v", it.Method, it.Path, it.Diff)
		}
		d := it.Diff[0]
		if d.Key != "FavoriteVO.abstract" || d.Op != apidoc.OpAdded || d.Breaking ||
			d.Rule != "response-property-added" || d.Section != apidoc.SectionResponseBody {
			t.Fatalf("diff 载荷不是那条真实新增字段: %+v", d)
		}
		// 假增删（分页字段被当成删除、item 字段被当成新增）一条都不许有
		if n := fakePagingNoise(it.Diff); n != 0 {
			t.Fatalf("AC4：事件里仍有 %d 条分页字段假差异（10-21 事故形态）: %+v", n, it.Diff)
		}
	}
}

// fakePagingNoise 统计「records/total/size/current/pages 出现在响应结构差异里」这类假差异
// —— 撞名事故的载荷指纹：分页表字段被当成 item 表的删除项。
func fakePagingNoise(diff []apidoc.DiffItem) int {
	fake := map[string]bool{"records": true, "total": true, "size": true, "current": true, "pages": true}
	n := 0
	for _, d := range diff {
		name := d.Key
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		if fake[name] && d.Section == apidoc.SectionResponseBody {
			n++
		}
	}
	return n
}

// previewApiDocOrFail 预览（失败即 t.Fatalf）。
func previewApiDocOrFail(t *testing.T, svc *Service, req ApiDocImportRequest) ApiDocImportPreview {
	t.Helper()
	pv, err := svc.PreviewApiDocImport(req)
	if err != nil {
		t.Fatalf("PreviewApiDocImport: %v", err)
	}
	return pv
}

// ambiguousStructDoc 人为构造的「真同名不同构」文档（AC2 的对抗样例走内联，不污染真实 fixture）：
// 1.1 与 1.2 各自在同一接口里放两张**同名**粗体结构表、字段集合不同 → 解析标 ambiguous。
// 库里同名两条会撞 api_schemas 唯一键、只剩后写的那一套，于是重导时算出「前一套字段全新增 +
// 后一套字段全删除」的假差异。1.1 声明「修改 + breaking:false」（验 E-BREAKING-DENIED 一侧），
// 1.2 声明「未变更」（验 E-DIFF-MISMATCH-UNCHANGED 一侧）。
func ambiguousStructDoc(itemDecl, feedbackDecl string) string {
	head := "# 演示服务接口变更文档\n\n" + apiMetaBlock("amb-svc", "fix", "同名结构冲突批次", "验证 ambiguous 降级不参与违约裁决", "REQ-A") +
		"## 一、用户管理\n\n"
	one := "### 1.1 收藏列表\n\n分页收藏，同名结构冲突。\n\n" +
		"- **URL**：`GET /api/v1/demo/favorite`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：" + itemDecl + "\n"
	if itemDecl == "修改" {
		one += "- **变更点**：只是同名结构冲突，未改任何契约\n- **breaking**：false\n"
	}
	one += "\n" + apiParamSection("| page | Int | 否 | Query | 1 | 页码 |") +
		apiRespSection("| message | String | 响应消息 |", "| data | IPage\\<FavoriteVO\\> | 分页结果 |") +
		"**FavoriteVO 结构**：\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| id | String | 收藏ID |\n| paperId | String | 论文ID |\n\n" +
		"**FavoriteVO 结构**：\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| records | List\\<FavoriteVO\\> | 当前页数据 |\n| total | Long | 总条数 |\n"
	two := "### 1.2 反馈列表\n\n分页反馈，同名结构冲突但声明未变更。\n\n" +
		"- **URL**：`GET /api/v1/demo/feedback`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：" + feedbackDecl + "\n"
	if feedbackDecl == "修改" {
		two += "- **变更点**：只是同名结构冲突，未改任何契约\n- **breaking**：false\n"
	}
	two += "\n" + apiParamSection("| page | Int | 否 | Query | 1 | 页码 |") +
		apiRespSection("| message | String | 响应消息 |", "| data | IPage\\<FeedbackVO\\> | 分页结果 |") +
		"**FeedbackVO 结构**：\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| id | String | 反馈ID |\n| userNote | String | 反馈说明 |\n\n" +
		"**FeedbackVO 结构**：\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| records | List\\<FeedbackVO\\> | 当前页数据 |\n| total | Long | 总条数 |\n"
	return head + one + two
}

func TestApiDocImportStructAmbiguousNotAuthorViolation(t *testing.T) {
	svc := newTestService(t)
	const project = "amb-svc"
	commitApiDoc(t, svc, ApiDocImportRequest{DocText: ambiguousStructDoc("新增", "新增"), ProjectName: project})

	pv := previewApiDocOrFail(t, svc, ApiDocImportRequest{DocText: ambiguousStructDoc("修改", "未变更"), ProjectName: project})
	// AC2：真同名不同构仍要看得见（W-STRUCT-DIVERGED 不升级、不阻断）
	sawDiverged := false
	for _, is := range pv.Issues {
		if is.Code == apidoc.WarnStructDiverged {
			sawDiverged = true
		}
	}
	if !sawDiverged {
		t.Fatalf("缺 W-STRUCT-DIVERGED: %+v", pv.Issues)
	}
	// 但两条违约闸都不得因歧义噪声触发（1.1 验 E-BREAKING-DENIED，1.2 验 E-DIFF-MISMATCH-UNCHANGED）
	for _, code := range []string{apidoc.ErrBreakingDenied, apidoc.ErrDiffMismatchUnc} {
		if hasIssueCode(pv.Issues, code) {
			t.Fatalf("歧义噪声不得判作者违约（%s）: %+v", code, pv.Issues)
		}
	}
	if !pv.OK {
		t.Fatalf("歧义不得阻断导入: %+v", pv.Issues)
	}
	// 仍需人工确认（降级 ≠ 静默放行）
	if pv.Breaking != store.ApiBreakingConfirm || !pv.NeedConfirm {
		t.Fatalf("歧义项应降为需人工确认: breaking=%d needConfirm=%v", pv.Breaking, pv.NeedConfirm)
	}
	if !strings.Contains(strings.Join(pv.Notes, "｜"), "同名不同构") {
		t.Fatalf("预览 notes 必须说明降级原因: %v", pv.Notes)
	}
	for _, c := range pv.Changes {
		if c.ChangeType == "unchanged" && len(apidoc.AuthoritativeItems(c.Diff)) != 0 {
			t.Fatalf("声明未变更的接口不得有权威差异: %+v", c.Diff)
		}
	}

	// 未确认时服务层照常拒写（拒的是「未确认」，不是「作者违约」）
	if _, err := svc.CommitApiDocImport(ApiDocImportRequest{
		DocText: ambiguousStructDoc("修改", "未变更"), ProjectName: project}); err == nil ||
		strings.Contains(err.Error(), apidoc.ErrBreakingDenied) ||
		strings.Contains(err.Error(), apidoc.ErrDiffMismatchUnc) ||
		!strings.Contains(err.Error(), "确认") {
		t.Fatalf("歧义导入应只被确认闸拦下: %v", err)
	}

	res := commitApiDoc(t, svc, ApiDocImportRequest{
		DocText: ambiguousStructDoc("修改", "未变更"), ProjectName: project, Confirmed: true})
	if res.Breaking != store.ApiBreakingConfirm {
		t.Fatalf("事件头 breaking = %d, want 2（需人工确认）", res.Breaking)
	}
	if res.Stats.Updated != 1 || res.Stats.Unchanged != 1 {
		t.Fatalf("stats = %+v, want updated 1 / unchanged 1（1.2 声明未变更且只有噪声差异 → 不算违约）", res.Stats)
	}
	detail, err := svc.GetApiChangeEvent(res.EventID)
	if err != nil {
		t.Fatalf("GetApiChangeEvent: %v", err)
	}
	var seen int
	for _, it := range detail.Items {
		for _, d := range it.Diff {
			seen++
			if d.Rule != apidoc.RuleStructAmbiguous || d.Breaking || !d.NeedsConfirm {
				t.Fatalf("涉及歧义结构的差异必须降级为 struct-ambiguous + 需确认: %+v", d)
			}
		}
	}
	if seen == 0 {
		t.Fatal("歧义差异必须留在事件载荷里（不许静默吞掉）")
	}
	if !strings.Contains(strings.Join(res.Notes, "｜"), "同名不同构") {
		t.Fatalf("提交结果也要带回降级提示（与预览同源）: %v", res.Notes)
	}
}

func hasIssueCode(issues []apidoc.Issue, code string) bool {
	for _, is := range issues {
		if is.Code == code {
			return true
		}
	}
	return false
}

// TestApiDocImportRealBreakingStillDenied AC5 防退化：真实的破坏性交叉校验必须照常生效。
// 一份无歧义、真的删了响应字段、却声明 breaking: false 的文档 → 仍整批拒收 + 零残留。
// 变异依据：若把 D2 的降级条件写成「所有响应结构差异都降级」（把闸门修哑），本用例红。
func TestApiDocImportRealBreakingStillDenied(t *testing.T) {
	svc := newTestService(t)
	const project = "gate-svc"
	base := "# 演示服务接口变更文档\n\n" + apiMetaBlock(project, "feature", "铺底", "为破坏性闸用例铺底", "REQ-G0") +
		"## 一、报表\n\n" +
		"### 1.1 报表查询\n\n原始说明。\n\n" +
		"- **URL**：`GET /api/v1/demo/report`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiRespSection("| message | String | 响应消息 |", "| total | Int | 总条数 |")
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: base, ProjectName: project})
	projectID := res.ProjectID

	// 响应真删 total，却声明 breaking: false —— 且带着确认标记提交，证明拒收原因只能是 E-BREAKING-DENIED
	cutting := "# 演示服务接口变更文档\n\n" + apiMetaBlock(project, "refactor", "下线 total 字段", "报表瘦身", "REQ-G1") +
		"## 一、报表\n\n" +
		"### 1.1 报表查询\n\n原始说明。\n\n" +
		"- **URL**：`GET /api/v1/demo/report`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：修改\n" +
		"- **变更点**：响应删除 total 字段\n" +
		"- **breaking**：false\n\n" +
		apiRespSection("| message | String | 响应消息 |")
	before := apiRawTableCounts(t)
	_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: cutting, ProjectName: project, Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), apidoc.ErrBreakingDenied) {
		t.Fatalf("无歧义的真破坏 + breaking:false 必须仍被 %s 拒收: %v", apidoc.ErrBreakingDenied, err)
	}
	assertCountsUnchanged(t, svc, projectID, 1, 1)
	assertApiTablesUnchanged(t, before)
}
