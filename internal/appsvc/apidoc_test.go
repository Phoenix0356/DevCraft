package appsvc

// 接口文档模块（PR3 编排层）测试：
// 复用 newTestService（临时 SQLite + 真加密盒 + 捕获 emit），仓库约定：真文件 + 手写
// t.Fatalf，无断言库。golden fixture 走 internal/apidoc/testdata（tracked，不可就地改）。
//
// AC 覆盖对照（prd.md）：
//   AC1 → TestApiDocImportAC1SpecFlow        AC2 → TestApiDocImportAC2Idempotent
//   AC3 → TestApiDocImportAC3Rejections      AC4 → TestApiDocImportAC4NoNoise
//   AC5 → TestApiDocImportAC5InitGolden      AC6 → TestApiDocImportAC6ConfirmGate
//   其余：init 闸门 / owner_scope 映射 / doc_text 存档 / 手工编辑无事件 / 项目级联删除 /
//   预览只读 / 规范导出同源。
//
// PR4a 落地后补的四个后端缺口（prd.md D19「手工增删改的实际边界」）：
//   ① TestSaveApiEntryKeepsServerOwnedColumns / TestSaveApiEntryManualGuards
//      —— 手工编辑走服务端读-改-写，保留列逐字节不变（§13.19 从前端自律变成服务端保证）；
//   ② TestPreviewApiDocImportEchoesDocMeta —— 预览回显文档自己声明的 meta；
//   ③ TestSaveApiDomainManual / TestDeleteApiDomainOnlyEmpty —— 领域手工改名/排序/删除；
//   ④ TestCommitApiDocImportResultCards —— 提交结果带回 notes/createdProject/createdDomains。

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"DevCraft/internal/apidoc"
	"DevCraft/internal/store"
)

// ===================== 文档构造助手（小文档内联；规范全角冒号 + backtick 字面约定）=====================

// apiMetaBlock ```apidoc-meta 围栏块（骨架示例同款半角 key: value）。
func apiMetaBlock(project, changeType, title, note, tag string) string {
	return "```apidoc-meta\n" +
		"spec: 1\nproject: " + project + "\nservice: 演示服务\nbase_path: /api/v1/demo\n" +
		"scope: incremental\nchange_type: " + changeType + "\nchange_title: " + title + "\n" +
		"change_note: " + note + "\nversion_tag: " + tag + "\n```\n\n"
}

// apiParamSection 请求参数表（规范模式必填列齐全：参数名|类型|必填|位置|默认值|说明）。
func apiParamSection(rows ...string) string {
	s := "#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n|---|---|---|---|---|---|\n"
	for _, r := range rows {
		s += r + "\n"
	}
	return s + "\n"
}

// apiPathSection 路径参数表。
func apiPathSection(rows ...string) string {
	s := "#### 路径参数\n\n| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n|---|---|---|---|---|---|\n"
	for _, r := range rows {
		s += r + "\n"
	}
	return s + "\n"
}

// apiRespSection 返回参数表（只要求 名称|类型）。
func apiRespSection(rows ...string) string {
	s := "#### 返回参数\n\n| 参数名 | 类型 | 说明 |\n|---|---|---|\n"
	for _, r := range rows {
		s += r + "\n"
	}
	return s + "\n"
}

// demoBaselineDoc 铺底文档：修改密码（oldPwd 可选）+ 删除用户（路径参数 id），均声明新增。
func demoBaselineDoc() string {
	return "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "feature", "初始化演示接口", "为后续变更用例铺底", "REQ-0") +
		"## 一、用户管理\n\n" +
		"### 1.1 修改密码\n\n修改当前用户密码。\n\n" +
		"- **URL**：`POST /api/v1/demo/password`\n" +
		"- **Content-Type**：`application/json`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiParamSection("| oldPwd | String | 否 | Body | | 旧密码 |", "| newPwd | String | 是 | Body | | 新密码 |") +
		apiRespSection("| message | String | 响应消息 |") +
		"### 1.2 删除用户\n\n按 id 删除用户。\n\n" +
		"- **URL**：`DELETE /api/v1/demo/user/{id}`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiPathSection("| id | Long | 是 | Path | | 用户ID |") +
		apiRespSection("| message | String | 响应消息 |")
}

// demoAC1Doc 规范文档：2 新增（1.3/1.4）+ 1 修改（1.1 oldPwd 否→是）+ 1 显式删除（1.2）。
func demoAC1Doc() string {
	return "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "feature", "密码策略升级", "安全要求旧密码必填并新增登录日志接口", "REQ-1") +
		"## 一、用户管理\n\n" +
		"### 1.1 修改密码\n\n修改当前用户密码。\n\n" +
		"- **URL**：`POST /api/v1/demo/password`\n" +
		"- **Content-Type**：`application/json`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：修改\n" +
		"- **变更点**：oldPwd 由可选改为必填\n\n" +
		apiParamSection("| oldPwd | String | 是 | Body | | 旧密码 |", "| newPwd | String | 是 | Body | | 新密码 |") +
		apiRespSection("| message | String | 响应消息 |") +
		"### 1.2 删除用户\n\n按 id 删除用户。\n\n" +
		"- **URL**：`DELETE /api/v1/demo/user/{id}`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：删除\n" +
		"- **变更点**：合规要求下线物理删除能力\n\n" +
		"### 1.3 查询登录日志\n\n分页查询登录日志。\n\n" +
		"- **URL**：`GET /api/v1/demo/login-log`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiParamSection("| page | Int | 否 | Query | 1 | 页码 |") +
		apiRespSection("| message | String | 响应消息 |") +
		"### 1.4 导出登录日志\n\n导出登录日志 CSV。\n\n" +
		"- **URL**：`GET /api/v1/demo/login-log/export`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiRespSection("| message | String | 响应消息 |")
}

// commitApiDoc 提交导入的测试快捷方式（失败即 t.Fatalf）。
func commitApiDoc(t *testing.T, svc *Service, req ApiDocImportRequest) ApiDocImportResult {
	t.Helper()
	res, err := svc.CommitApiDocImport(req)
	if err != nil {
		t.Fatalf("CommitApiDocImport: %v", err)
	}
	return res
}

// seedDemoBaseline 铺底：导入 demoBaselineDoc（事件 v1，2 条 active 条目），返回项目 id。
func seedDemoBaseline(t *testing.T, svc *Service) string {
	t.Helper()
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoBaselineDoc()})
	if res.Seq != 1 || res.EventID == 0 {
		t.Fatalf("铺底导入应生成事件 v1: %+v", res)
	}
	return res.ProjectID
}

// apiEventCount 项目下事件总数。
func apiEventCount(t *testing.T, svc *Service, projectID string) int {
	t.Helper()
	evs, err := svc.ListApiChangeEvents(ApiEventQuery{ProjectID: projectID})
	if err != nil {
		t.Fatalf("ListApiChangeEvents: %v", err)
	}
	return len(evs)
}

// apiAllEntries 项目下全部条目（含 internal 与 removed）。
func apiAllEntries(t *testing.T, svc *Service, projectID string) []store.ApiEntry {
	t.Helper()
	list, err := svc.ListApiEntries(ApiEntryQuery{ProjectID: projectID, IncludeInternal: true, IncludeRemoved: true})
	if err != nil {
		t.Fatalf("ListApiEntries: %v", err)
	}
	return list
}

// mustEntryByIdentity 按身份键取条目（不存在即 Fatal）。
func mustEntryByIdentity(t *testing.T, svc *Service, projectID, key string) store.ApiEntry {
	t.Helper()
	e, found, err := svc.store.FindApiEntryByIdentity(projectID, key)
	if err != nil {
		t.Fatalf("FindApiEntryByIdentity(%s): %v", key, err)
	}
	if !found {
		t.Fatalf("条目不存在: %s", key)
	}
	return e
}

// ===================== AC1 规范导入闭环 =====================

func TestApiDocImportAC1SpecFlow(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)

	// AC1 文档含 removed 与 breaking → 服务层确认闸强制要求 Confirmed（D16）
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true})
	if res.Seq != 2 || res.EventID == 0 {
		t.Fatalf("应生成事件 v2: %+v", res)
	}
	if res.Stats.Added != 2 || res.Stats.Updated != 1 || res.Stats.Removed != 1 {
		t.Fatalf("stats = %+v, want added 2 / updated 1 / removed 1", res.Stats)
	}
	if res.Breaking != store.ApiBreakingYes {
		t.Fatalf("聚合 breaking = %d, want 1（oldPwd 必填翻转是自动破坏）", res.Breaking)
	}
	if apiEventCount(t, svc, projectID) != 2 {
		t.Fatalf("事件总数 = %d, want 2（铺底 v1 + 本次 v2）", apiEventCount(t, svc, projectID))
	}

	// 明细：2 added / 1 updated / 1 removed
	detail, err := svc.GetApiChangeEvent(res.EventID)
	if err != nil {
		t.Fatalf("GetApiChangeEvent: %v", err)
	}
	counts := map[string]int{}
	var updatedItem *store.ApiChangeItem
	for i := range detail.Items {
		counts[detail.Items[i].ChangeType]++
		if detail.Items[i].ChangeType == store.ApiChangeTypeUpdated {
			updatedItem = &detail.Items[i]
		}
	}
	if counts[store.ApiChangeTypeAdded] != 2 || counts[store.ApiChangeTypeUpdated] != 1 ||
		counts[store.ApiChangeTypeRemoved] != 1 || len(detail.Items) != 4 {
		t.Fatalf("明细分布 = %v（共 %d 条）, want 2 added / 1 updated / 1 removed", counts, len(detail.Items))
	}
	// updated 行 diff 载荷精确到 oldPwd 那一个字段
	if updatedItem == nil || len(updatedItem.Diff) != 1 {
		t.Fatalf("updated 明细 diff 应恰好 1 条: %+v", updatedItem)
	}
	d := updatedItem.Diff[0]
	if d.Section != apidoc.SectionRequestBody || d.Key != "oldPwd" || d.Op != apidoc.OpUpdated ||
		!d.Breaking || d.Rule != "request-property-became-required" {
		t.Fatalf("diff 载荷不对: %+v", d)
	}
	before, _ := d.Before.(map[string]any)
	after, _ := d.After.(map[string]any)
	if before == nil || after == nil || before["required"] != float64(0) || after["required"] != float64(1) {
		// DiffItem.Before/After 经 JSON 落库再回读，是 map 形态；required 用 -1/0/1 哨兵
		t.Fatalf("before/after 片段应携带 required 0→1: before=%v after=%v", d.Before, d.After)
	}
	if updatedItem.Breaking != store.ApiBreakingYes || updatedItem.DeclaredNote != "oldPwd 由可选改为必填" {
		t.Fatalf("updated 明细头不对: breaking=%d note=%q", updatedItem.Breaking, updatedItem.DeclaredNote)
	}

	// 定义被正确更新：oldPwd 必填=1；删除的条目软删；新增条目 active
	pw := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	fields, err := svc.store.ListApiFields(store.ApiOwnerEndpoint, pw.ID)
	if err != nil {
		t.Fatalf("ListApiFields: %v", err)
	}
	var oldPwd *store.ApiField
	for i := range fields {
		if fields[i].Name == "oldPwd" {
			oldPwd = &fields[i]
		}
	}
	if oldPwd == nil || oldPwd.Required != 1 || oldPwd.Kind != store.ApiFieldKindBody {
		t.Fatalf("oldPwd 定义未更新: %+v", oldPwd)
	}
	del := mustEntryByIdentity(t, svc, projectID, "DELETE|/api/v1/demo/user/{}")
	if del.Status != store.ApiEntryStatusRemoved || del.DeletedAt == 0 {
		t.Fatalf("删除条目应软删: status=%s deletedAt=%d", del.Status, del.DeletedAt)
	}
	added := mustEntryByIdentity(t, svc, projectID, "GET|/api/v1/demo/login-log")
	if added.Status != store.ApiEntryStatusActive || added.DomainID == "" {
		t.Fatalf("新增条目应 active 且挂领域: %+v", added)
	}
	// 默认列表过滤 removed
	list, err := svc.ListApiEntries(ApiEntryQuery{ProjectID: projectID})
	if err != nil {
		t.Fatalf("ListApiEntries: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("默认列表应 3 条（过滤 removed）: %d", len(list))
	}
	// 条目详情的历史事件反向关联（R9/AC7）
	entryDetail, err := svc.GetApiEntry(pw.ID)
	if err != nil {
		t.Fatalf("GetApiEntry: %v", err)
	}
	if len(entryDetail.Events) != 2 {
		t.Fatalf("修改密码应有 2 条历史事件（v1 added + v2 updated）: %+v", entryDetail.Events)
	}
	if entryDetail.Events[0].Seq != 2 || entryDetail.Events[0].ChangeType != store.ApiChangeTypeUpdated {
		t.Fatalf("历史事件应 seq DESC: %+v", entryDetail.Events[0])
	}
	if len(entryDetail.RequestFields) != 2 || len(entryDetail.ResponseFields) != 1 {
		t.Fatalf("详情字段分组不对: req=%d resp=%d", len(entryDetail.RequestFields), len(entryDetail.ResponseFields))
	}
}

// ===================== AC2 幂等 =====================

func TestApiDocImportAC2Idempotent(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)
	first := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true})
	eventsBefore := apiEventCount(t, svc, projectID)
	pwBefore := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")

	// 同一份文档第二次提交：diff 载荷全空 → 不生成事件、不改定义、零写入
	if first.ProjectID != projectID {
		t.Fatalf("首次导入项目 id 不一致: %s vs %s", first.ProjectID, projectID)
	}
	second := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true})
	if second.EventID != 0 || second.Seq != 0 {
		t.Fatalf("第二次导入不应生成事件: %+v", second)
	}
	if !strings.Contains(second.Message, "无变更") {
		t.Fatalf("结果应说明「无变更」: %q", second.Message)
	}
	if n := apiEventCount(t, svc, projectID); n != eventsBefore {
		t.Fatalf("事件总数变了: %d → %d", eventsBefore, n)
	}
	pwAfter := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	if pwAfter.UpdatedAt != pwBefore.UpdatedAt {
		t.Fatalf("条目 updated_at 变了: %d → %d（第二次导入必须零写入）", pwBefore.UpdatedAt, pwAfter.UpdatedAt)
	}
	if pwAfter.ID != pwBefore.ID {
		t.Fatalf("条目 id 变了: %s → %s", pwBefore.ID, pwAfter.ID)
	}
}

// ===================== AC3 拒收与原子性 =====================

// apiTableNames 9 张 api_* 表全集（AC3「零残留」断言的覆盖面，PRD R4）。
var apiTableNames = []string{
	"api_projects", "api_domains", "api_entries", "api_fields", "api_schemas",
	"api_examples", "api_change_events", "api_change_items", "api_import_batches",
}

// apiRawTableCounts 对测试库开第二个独立只读连接（sql.Open 与 store 同一驱动），
// 返回 9 张 api_* 表各自的总行数。用全局计数而不是按项目 id 查：拒收后任何一张表
// 都不允许多出/改动一行孤儿数据（按项目查会漏掉挂在「回滚掉的幽灵项目 id」上的行）。
func apiRawTableCounts(t *testing.T) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", lastTestDBPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	counts := make(map[string]int, len(apiTableNames))
	for _, table := range apiTableNames {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}
	return counts
}

// assertApiTablesZero 断言 9 张表全部 0 行（全新库上整批拒收后用）。
func assertApiTablesZero(t *testing.T) {
	t.Helper()
	for _, table := range apiTableNames {
		if n := apiRawTableCounts(t)[table]; n != 0 {
			t.Fatalf("拒收后 %s 必须零残留，实际 %d 行", table, n)
		}
	}
}

// assertApiTablesUnchanged 断言拒收前后 9 张表行数逐一不变（既有项目上用快照对比）。
func assertApiTablesUnchanged(t *testing.T, before map[string]int) {
	t.Helper()
	after := apiRawTableCounts(t)
	for _, table := range apiTableNames {
		if before[table] != after[table] {
			t.Fatalf("拒收后 %s 行数变了: %d → %d（必须零残留）", table, before[table], after[table])
		}
	}
}

// assertNoProjectResidue 断言项目名在库里零残留（9 张表都不可能挂到一个不存在的项目上）。
func assertNoProjectResidue(t *testing.T, svc *Service, name string) {
	t.Helper()
	if _, found, err := svc.store.FindApiProjectByName(name); err != nil {
		t.Fatalf("FindApiProjectByName: %v", err)
	} else if found {
		t.Fatalf("拒收后项目不应存在: %s", name)
	}
}

// assertCountsUnchanged 断言既有项目的条目/事件数没有变化（拒收零残留）。
func assertCountsUnchanged(t *testing.T, svc *Service, projectID string, wantEntries, wantEvents int) {
	t.Helper()
	if n := len(apiAllEntries(t, svc, projectID)); n != wantEntries {
		t.Fatalf("拒收后条目数 = %d, want %d（必须零残留）", n, wantEntries)
	}
	if n := apiEventCount(t, svc, projectID); n != wantEvents {
		t.Fatalf("拒收后事件数 = %d, want %d（必须零残留）", n, wantEvents)
	}
}

func TestApiDocImportAC3Rejections(t *testing.T) {
	// —— 解析级拒收：每种都整批拒收且数据库零残留（新项目根本不被创建）——
	parseCases := []struct {
		name string
		doc  string
		code string
	}{
		{"缺 meta 块",
			"## 一、用户管理\n\n### 1.1 查询\n\n- **URL**：`GET /api/v1/demo/x`\n- **变更**：新增\n",
			apidoc.ErrSpecBlockMissing},
		{"URL 行半角冒号",
			strings.Replace(demoBaselineDoc(), "- **URL**：`POST", "- **URL:** `POST", 1),
			apidoc.ErrURLFormat},
		{"缺 位置 列",
			strings.Replace(demoBaselineDoc(),
				"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n|---|---|---|---|---|---|\n| oldPwd | String | 否 | Body | | 旧密码 |\n| newPwd | String | 是 | Body | | 新密码 |",
				"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 说明 |\n|---|---|---|---|\n| oldPwd | String | 否 | 旧密码 |", 1),
			apidoc.ErrPosColMissing},
		{"引用式写法",
			strings.Replace(demoBaselineDoc(),
				"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n|---|---|---|---|---|---|\n| oldPwd | String | 否 | Body | | 旧密码 |\n| newPwd | String | 是 | Body | | 新密码 |",
				"#### 请求参数\n\n同新增订阅。", 1),
			apidoc.ErrCrossRef},
	}
	for _, c := range parseCases {
		t.Run(c.name, func(t *testing.T) {
			svc := newTestService(t)
			_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: c.doc, ProjectName: "bad-svc"})
			if err == nil {
				t.Fatal("应整批拒收")
			}
			if !strings.Contains(err.Error(), c.code) {
				t.Fatalf("错误信息应带 %s: %v", c.code, err)
			}
			assertNoProjectResidue(t, svc, "bad-svc")
			assertApiTablesZero(t) // 全新库上拒收 → 9 张表全 0 行
		})
	}

	// —— 编排级拒收：声明与库状态矛盾（对照既有项目，拒收后 9 张表零变化）——
	t.Run("声明修改但程序算出无变化", func(t *testing.T) {
		svc := newTestService(t)
		projectID := seedDemoBaseline(t, svc)
		// 一个真新增 + 一个「修改」但与库中定义完全一致 → 明细非空 → 交叉校验拒收
		doc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "fix", "混合批次", "验证交叉校验", "REQ-2") +
			"## 一、用户管理\n\n" +
			"### 1.1 修改密码\n\n修改当前用户密码。\n\n" +
			"- **URL**：`POST /api/v1/demo/password`\n" +
			"- **Content-Type**：`application/json`\n" +
			"- **认证**：需要登录\n" +
			"- **变更**：修改\n" +
			"- **变更点**：声称改了但其实没改\n\n" +
			apiParamSection("| oldPwd | String | 否 | Body | | 旧密码 |", "| newPwd | String | 是 | Body | | 新密码 |") +
			apiRespSection("| message | String | 响应消息 |") +
			"### 1.5 全新接口\n\n真新增。\n\n" +
			"- **URL**：`GET /api/v1/demo/fresh`\n" +
			"- **认证**：需要登录\n" +
			"- **变更**：新增\n\n" +
			apiRespSection("| message | String | 响应消息 |")
		before := apiRawTableCounts(t)
		_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: doc})
		if err == nil || !strings.Contains(err.Error(), apidoc.ErrUpdatedNoDiff) {
			t.Fatalf("应报 %s: %v", apidoc.ErrUpdatedNoDiff, err)
		}
		assertCountsUnchanged(t, svc, projectID, 2, 1)
		assertApiTablesUnchanged(t, before) // 9 张表逐一零残留
		if _, found, _ := svc.store.FindApiEntryByIdentity(projectID, "GET|/api/v1/demo/fresh"); found {
			t.Fatal("拒收后真新增的接口也不得写入（原子性）")
		}
	})

	t.Run("声明集合与 diff 不一致", func(t *testing.T) {
		svc := newTestService(t)
		projectID := seedDemoBaseline(t, svc)
		// 声明「未变更」但程序算出类型变化（String → Long）→ E-DIFF-MISMATCH-UNCHANGED
		doc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "fix", "口径批次", "验证声明集合交叉校验", "REQ-3") +
			"## 一、用户管理\n\n" +
			"### 1.1 修改密码\n\n修改当前用户密码。\n\n" +
			"- **URL**：`POST /api/v1/demo/password`\n" +
			"- **Content-Type**：`application/json`\n" +
			"- **认证**：需要登录\n" +
			"- **变更**：未变更\n\n" +
			apiParamSection("| oldPwd | Long | 否 | Body | | 旧密码 |", "| newPwd | String | 是 | Body | | 新密码 |") +
			apiRespSection("| message | String | 响应消息 |")
		before := apiRawTableCounts(t)
		_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: doc})
		if err == nil || !strings.Contains(err.Error(), apidoc.ErrDiffMismatchUnc) {
			t.Fatalf("应报 %s: %v", apidoc.ErrDiffMismatchUnc, err)
		}
		assertCountsUnchanged(t, svc, projectID, 2, 1)
		assertApiTablesUnchanged(t, before) // 9 张表逐一零残留
		pw := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
		fields, _ := svc.store.ListApiFields(store.ApiOwnerEndpoint, pw.ID)
		for _, f := range fields {
			if f.Name == "oldPwd" && f.TypeKey != "String" {
				t.Fatalf("拒收后定义不得被改写: %+v", f)
			}
		}
	})

	t.Run("声明删除但库里找不到", func(t *testing.T) {
		svc := newTestService(t)
		projectID := seedDemoBaseline(t, svc)
		doc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "fix", "删不存在的", "验证删除闸门", "REQ-4") +
			"## 一、用户管理\n\n" +
			"### 1.9 幽灵接口\n\n库里没有。\n\n" +
			"- **URL**：`GET /api/v1/demo/ghost`\n" +
			"- **变更**：删除\n" +
			"- **变更点**：下线幽灵接口\n"
		before := apiRawTableCounts(t)
		_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: doc, Confirmed: true})
		if err == nil || !strings.Contains(err.Error(), apidoc.ErrDeleteNotFound) {
			t.Fatalf("应报 %s: %v", apidoc.ErrDeleteNotFound, err)
		}
		assertCountsUnchanged(t, svc, projectID, 2, 1)
		assertApiTablesUnchanged(t, before) // 9 张表逐一零残留
	})
}

// ===================== AC4 无噪声 =====================

func TestApiDocImportAC4NoNoise(t *testing.T) {
	svc := newTestService(t)
	base := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-doc", "docs", "报表接口", "铺底", "REQ-0") +
		"## 一、报表\n\n" +
		"### 1.1 报表查询\n\n原始说明。\n\n" +
		"- **URL**：`GET /api/v1/demo/report`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiRespSection("| message | String | 响应消息 |") +
		"#### 返回示例\n\n```json\n{\"message\": \"ok\"}\n```\n"
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: base})
	projectID := res.ProjectID
	entry := mustEntryByIdentity(t, svc, projectID, "GET|/api/v1/demo/report")

	// 只改中文说明、接口名与示例 → 不算接口变更、不进事件明细（doc_hash 域）
	polished := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-doc", "docs", "报表接口润色", "全局措辞润色", "REQ-0") +
		"## 一、报表\n\n" +
		"### 1.1 查询报表\n\n润色后的说明：查询统计报表数据。\n\n" +
		"- **URL**：`GET /api/v1/demo/report`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：未变更\n\n" +
		apiRespSection("| message | String | 响应消息 |") +
		"#### 返回示例\n\n```json\n{\"message\": \"success\"}\n```\n"
	res2 := commitApiDoc(t, svc, ApiDocImportRequest{DocText: polished})
	if res2.EventID != 0 {
		t.Fatalf("措辞润色不应生成事件: %+v", res2)
	}
	if n := apiEventCount(t, svc, projectID); n != 1 {
		t.Fatalf("事件总数 = %d, want 1（只有铺底事件）", n)
	}
	if !strings.Contains(res2.Message, "仅同步") {
		t.Fatalf("结果应说明仅同步文本: %q", res2.Message)
	}
	after := mustEntryByIdentity(t, svc, projectID, "GET|/api/v1/demo/report")
	if after.ID != entry.ID || after.Name != "查询报表" || !strings.Contains(after.Description, "润色后的说明") {
		t.Fatalf("条目原文应被同步: %+v", after)
	}
	examples, err := svc.store.ListApiExamples(store.ApiOwnerEndpoint, after.ID)
	if err != nil || len(examples) != 1 || !strings.Contains(examples[0].Content, "success") {
		t.Fatalf("示例应更新为最新值: %v %+v", err, examples)
	}
}

// ===================== AC5 初始化导入（golden fixture）=====================

func readApiFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "apidoc", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func TestApiDocImportAC5InitGolden(t *testing.T) {
	svc := newTestService(t)

	// 用户服务：20 条
	userRes := commitApiDoc(t, svc, ApiDocImportRequest{
		DocText: readApiFixture(t, "user-service.md"), ProjectName: "user-svc-golden", InitMode: true})
	if userRes.EventID != 0 {
		t.Fatalf("init 不应产生事件: %+v", userRes)
	}
	userEntries := apiAllEntries(t, svc, userRes.ProjectID)
	if len(userEntries) != 20 {
		t.Fatalf("用户服务条目数 = %d, want 20", len(userEntries))
	}
	// GET /api/v1.0/user 与 base_path 逐字相等 → 正常入库而非空串（绝不剥离 base_path，D18）
	self := mustEntryByIdentity(t, svc, userRes.ProjectID, "GET|/api/v1.0/user")
	if self.PathRaw != "/api/v1.0/user" {
		t.Fatalf("GET|/api/v1.0/user path_raw = %q", self.PathRaw)
	}

	// 论文服务：11 条；save 与 saveBatch 是两条不被合并
	paperRes := commitApiDoc(t, svc, ApiDocImportRequest{
		DocText: readApiFixture(t, "paper-service.md"), ProjectName: "paper-svc-golden", InitMode: true})
	paperEntries := apiAllEntries(t, svc, paperRes.ProjectID)
	if len(paperEntries) != 11 {
		t.Fatalf("论文服务条目数 = %d, want 11", len(paperEntries))
	}
	save := mustEntryByIdentity(t, svc, paperRes.ProjectID, "POST|/api/v1.0/paper/rpc/arxiv/save")
	batch := mustEntryByIdentity(t, svc, paperRes.ProjectID, "POST|/api/v1.0/paper/rpc/arxiv/saveBatch")
	if save.ID == batch.ID {
		t.Fatal("save 与 saveBatch 必须是两条独立条目")
	}
	// 内部 RPC 入库且 visibility=internal（R11）
	if save.Visibility != store.ApiVisibilityInternal || batch.Visibility != store.ApiVisibilityInternal {
		t.Fatalf("内部 RPC 应 visibility=internal: save=%s batch=%s", save.Visibility, batch.Visibility)
	}
	// 列表默认过滤 internal：11 - 2 = 9
	publicList, err := svc.ListApiEntries(ApiEntryQuery{ProjectID: paperRes.ProjectID})
	if err != nil {
		t.Fatalf("ListApiEntries: %v", err)
	}
	if len(publicList) != 9 {
		t.Fatalf("默认列表 = %d, want 9（过滤 2 条内部 RPC）", len(publicList))
	}
	if apiEventCount(t, svc, paperRes.ProjectID) != 0 || apiEventCount(t, svc, userRes.ProjectID) != 0 {
		t.Fatal("init 导入不得产生变更事件")
	}
	// 幂等：同一份 golden 二次 init（项目已非空 → 需显式放行，§13.12）→ 零写入（无变更）
	again := commitApiDoc(t, svc, ApiDocImportRequest{
		DocText: readApiFixture(t, "user-service.md"), ProjectName: "user-svc-golden",
		InitMode: true, AllowInitOnNonEmpty: true})
	if again.EventID != 0 || !strings.Contains(again.Message, "无变更") {
		t.Fatalf("golden 二次 init 应无变更: %+v", again)
	}
	if n := len(apiAllEntries(t, svc, userRes.ProjectID)); n != 20 {
		t.Fatalf("二次 init 后条目数 = %d, want 20（零写入）", n)
	}
}

// ===================== AC6 确认闸（D16 服务层强制）=====================

func TestApiDocImportAC6ConfirmGate(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)

	deleteDoc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "refactor", "下线删除用户", "合规要求", "REQ-5") +
		"## 一、用户管理\n\n" +
		"### 1.2 删除用户\n\n按 id 删除用户。\n\n" +
		"- **URL**：`DELETE /api/v1/demo/user/{id}`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：删除\n" +
		"- **变更点**：下线物理删除能力\n"
	// 含 removed 未确认 → 一行不写（9 张表零残留）
	before := apiRawTableCounts(t)
	_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: deleteDoc})
	if err == nil || !strings.Contains(err.Error(), "确认") {
		t.Fatalf("含删除未确认应被服务层拒写: %v", err)
	}
	assertCountsUnchanged(t, svc, projectID, 2, 1)
	assertApiTablesUnchanged(t, before)
	if e := mustEntryByIdentity(t, svc, projectID, "DELETE|/api/v1/demo/user/{}"); e.Status != store.ApiEntryStatusActive {
		t.Fatalf("未确认时不得软删: %s", e.Status)
	}
	// 确认后写入
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: deleteDoc, Confirmed: true})
	if res.Seq != 2 || res.Breaking != store.ApiBreakingConfirm {
		t.Fatalf("确认后应生成 v2 且 breaking=2: %+v", res)
	}
	if e := mustEntryByIdentity(t, svc, projectID, "DELETE|/api/v1/demo/user/{}"); e.Status != store.ApiEntryStatusRemoved {
		t.Fatalf("确认后应软删: %s", e.Status)
	}

	// breaking=1（必填翻转）未确认同样拒写
	breakingDoc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "fix", "旧密码必填", "安全加固", "REQ-6") +
		"## 一、用户管理\n\n" +
		"### 1.1 修改密码\n\n修改当前用户密码。\n\n" +
		"- **URL**：`POST /api/v1/demo/password`\n" +
		"- **Content-Type**：`application/json`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：修改\n" +
		"- **变更点**：oldPwd 改为必填\n\n" +
		apiParamSection("| oldPwd | String | 是 | Body | | 旧密码 |", "| newPwd | String | 是 | Body | | 新密码 |") +
		apiRespSection("| message | String | 响应消息 |")
	beforeBreaking := apiRawTableCounts(t)
	if _, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: breakingDoc}); err == nil ||
		!strings.Contains(err.Error(), "破坏") {
		t.Fatalf("breaking=1 未确认应被拒写: %v", err)
	}
	assertCountsUnchanged(t, svc, projectID, 2, 2)
	assertApiTablesUnchanged(t, beforeBreaking) // 9 张表逐一零残留
	// 纯新增（非破坏）→ 无需确认直接写入
	pureAdd := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "feature", "新增查询", "补齐查询能力", "REQ-7") +
		"## 一、用户管理\n\n" +
		"### 1.6 查询用户\n\n按 id 查询。\n\n" +
		"- **URL**：`GET /api/v1/demo/user`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiRespSection("| message | String | 响应消息 |")
	pure := commitApiDoc(t, svc, ApiDocImportRequest{DocText: pureAdd}) // 不带 Confirmed
	if pure.Seq != 3 || pure.Breaking != store.ApiBreakingNo {
		t.Fatalf("纯新增应直接写入且 breaking=0: %+v", pure)
	}
}

// ===================== init 闸门（spec-draft §13.12）=====================

func TestApiDocImportInitGateNonEmpty(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)

	initDoc := "# 演示服务散文档\n\n## 一、用户管理\n\n### 1.9 散装接口\n\n- **URL**：`GET /api/v1/demo/loose`\n"
	before := apiRawTableCounts(t)
	_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: initDoc, ProjectName: "demo-svc", InitMode: true})
	if err == nil || !strings.Contains(err.Error(), "初始化") {
		t.Fatalf("非空项目 init 应被拒: %v", err)
	}
	assertCountsUnchanged(t, svc, projectID, 2, 1)
	assertApiTablesUnchanged(t, before) // 拒收的 init 不得写入任何表（含批次）
	// 显式放行（界面向导勾选）→ 允许
	res := commitApiDoc(t, svc, ApiDocImportRequest{
		DocText: initDoc, ProjectName: "demo-svc", InitMode: true, AllowInitOnNonEmpty: true})
	if res.EventID != 0 {
		t.Fatalf("init 不产生事件: %+v", res)
	}
	mustEntryByIdentity(t, svc, projectID, "GET|/api/v1/demo/loose")
	// 预览同样要暴露闸门（initBlocked）
	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: initDoc, ProjectName: "demo-svc", InitMode: true})
	if err != nil {
		t.Fatalf("PreviewApiDocImport: %v", err)
	}
	if !pv.InitBlocked || pv.OK {
		t.Fatalf("非空项目 init 预览应 initBlocked 且 OK=false: %+v", pv)
	}
}

// ===================== owner_scope 映射（spec-draft §13.13）=====================

func TestApiDocImportSchemaOwnerScopeMapping(t *testing.T) {
	svc := newTestService(t)
	doc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-vo", "feature", "订阅查询", "验证命名结构作用域", "REQ-8") +
		"## 一、订阅\n\n" +
		"### 1.1 查询订阅\n\n查询当前用户订阅。\n\n" +
		"- **URL**：`GET /api/v1/demo/subscription`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiRespSection("| data | DemoVO | 订阅信息 |") +
		"**DemoVO 结构**：\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| id | String | 订阅ID |\n"
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: doc})
	entry := mustEntryByIdentity(t, svc, res.ProjectID, "GET|/api/v1/demo/subscription")
	schemas, err := svc.store.ListApiSchemas(res.ProjectID)
	if err != nil {
		t.Fatalf("ListApiSchemas: %v", err)
	}
	if len(schemas) != 1 || schemas[0].Name != "DemoVO" {
		t.Fatalf("命名结构落库不对: %+v", schemas)
	}
	// 落库 owner_scope 必须是 entry id，而不是解析层的 identity_key
	if schemas[0].OwnerScope != entry.ID {
		t.Fatalf("owner_scope = %q, want entry id %q", schemas[0].OwnerScope, entry.ID)
	}
	if strings.Contains(schemas[0].OwnerScope, "|") {
		t.Fatalf("owner_scope 不得是 identity_key: %q", schemas[0].OwnerScope)
	}
	sfs, err := svc.store.ListApiFields(store.ApiOwnerSchema, schemas[0].ID)
	if err != nil || len(sfs) != 1 || sfs[0].Name != "id" || sfs[0].Kind != store.ApiFieldKindBody {
		t.Fatalf("命名结构字段落库不对: %v %+v", err, sfs)
	}
	// 条目详情带命名结构
	detail, err := svc.GetApiEntry(entry.ID)
	if err != nil {
		t.Fatalf("GetApiEntry: %v", err)
	}
	if len(detail.Schemas) != 1 || len(detail.Schemas[0].Fields) != 1 {
		t.Fatalf("详情应带命名结构与字段: %+v", detail.Schemas)
	}
}

// ===================== doc_text 存档（D12）与列表大文本隔离 =====================

func TestApiDocChangeEventDocTextArchive(t *testing.T) {
	svc := newTestService(t)
	seedDemoBaseline(t, svc)
	docText := demoAC1Doc()
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: docText, DocRef: "D:/docs/demo-ac1.md", Confirmed: true})

	// 原文单独取回，与导入输入逐字节一致
	got, err := svc.GetApiChangeEventDocText(res.EventID)
	if err != nil {
		t.Fatalf("GetApiChangeEventDocText: %v", err)
	}
	if got != docText {
		t.Fatalf("doc_text 与输入不一致（len %d vs %d）", len(got), len(docText))
	}
	// 列表 SQL 不带 doc_text
	evs, err := svc.ListApiChangeEvents(ApiEventQuery{ProjectID: res.ProjectID})
	if err != nil {
		t.Fatalf("ListApiChangeEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("事件数 = %d, want 2", len(evs))
	}
	for _, ev := range evs {
		if ev.DocText != "" {
			t.Fatal("事件列表不得携带 doc_text")
		}
	}
	if evs[0].Seq != 2 || evs[1].Seq != 1 {
		t.Fatalf("列表应 seq DESC: %d, %d", evs[0].Seq, evs[1].Seq)
	}
	if evs[0].DocRef != "D:/docs/demo-ac1.md" || evs[0].Label != "REQ-1" ||
		evs[0].Reason != "安全要求旧密码必填并新增登录日志接口" || evs[0].Kind != "feature" {
		t.Fatalf("事件头字段不对: %+v", evs[0])
	}
	if evs[0].Meta["docSha256"] == nil || evs[0].Meta["docSha256"] == "" {
		t.Fatalf("meta 扩展槽应存 docSha256: %+v", evs[0].Meta)
	}
	// 详情也不含 doc_text，但明细带 diff 载荷
	detail, err := svc.GetApiChangeEvent(res.EventID)
	if err != nil {
		t.Fatalf("GetApiChangeEvent: %v", err)
	}
	if detail.Event.DocText != "" {
		t.Fatal("事件详情不得携带 doc_text（原文单独取）")
	}
	var removedItem *store.ApiChangeItem
	for i := range detail.Items {
		if detail.Items[i].ChangeType == store.ApiChangeTypeRemoved {
			removedItem = &detail.Items[i]
		}
	}
	if removedItem == nil || len(removedItem.Diff) == 0 {
		t.Fatalf("removed 明细必须带「必填参数+响应字段」最小片段: %+v", removedItem)
	}
	// label 过滤与聚合视图数据源（D10/AC7）
	labeled, err := svc.ListApiChangeEvents(ApiEventQuery{ProjectID: res.ProjectID, Label: "REQ-1"})
	if err != nil || len(labeled) != 1 || labeled[0].Seq != 2 {
		t.Fatalf("label 过滤不对: %v %+v", err, labeled)
	}
	labels, err := svc.ListApiChangeLabels(res.ProjectID)
	if err != nil || len(labels) != 2 || labels[0] != "REQ-1" || labels[1] != "REQ-0" {
		t.Fatalf("labels = %v (%v), want [REQ-1 REQ-0]（seq DESC）", labels, err)
	}
	// 批次表：keys_json 记录本次声明的 key 集
	batches, err := svc.store.ListApiImportBatches("demo-svc|/api/v1/demo|演示服务接口变更文档", 0)
	if err != nil || len(batches) != 2 {
		t.Fatalf("批次记录 = %d (%v), want 2", len(batches), err)
	}
	if len(batches[0].Keys) != 4 || batches[0].DeclaredMode != "incremental" || batches[0].DocFingerprint == "" {
		t.Fatalf("批次内容不对: %+v", batches[0])
	}
}

// ===================== 手工编辑不产生事件 =====================

func TestSaveApiEntryManualNoEvent(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)
	entry := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")

	e := entry
	e.Description = "手工修改的说明"
	if err := svc.SaveApiEntry(e); err != nil {
		t.Fatalf("SaveApiEntry: %v", err)
	}
	if n := apiEventCount(t, svc, projectID); n != 1 {
		t.Fatalf("手工编辑不得产生事件: %d", n)
	}
	after := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	if after.Description != "手工修改的说明" || after.ID != entry.ID {
		t.Fatalf("手工编辑未生效: %+v", after)
	}
	// 改方法 = 改身份 → 拒收（中文错误）
	bad := after
	bad.Method = "PUT"
	if err := svc.SaveApiEntry(bad); err == nil || !strings.Contains(err.Error(), "身份") {
		t.Fatalf("改方法应被拒: %v", err)
	}
	// 非法方法 / 空名称 / 路径不以 / 开头
	if err := svc.SaveApiEntry(store.ApiEntry{ProjectID: projectID, Name: "x", Method: "FETCH", PathRaw: "/x"}); err == nil {
		t.Fatal("非法方法应被拒")
	}
	if err := svc.SaveApiEntry(store.ApiEntry{ProjectID: projectID, Name: "", Method: "GET", PathRaw: "/x"}); err == nil {
		t.Fatal("空名称应被拒")
	}
	if err := svc.SaveApiEntry(store.ApiEntry{ProjectID: projectID, Name: "x", Method: "GET", PathRaw: "x"}); err == nil {
		t.Fatal("路径不以 / 开头应被拒")
	}
	// 手工新建 + 同 key 重复新建被拒
	manual := store.ApiEntry{ProjectID: projectID, Name: "手工接口", Method: "GET", PathRaw: "/api/v1/demo/manual"}
	if err := svc.SaveApiEntry(manual); err != nil {
		t.Fatalf("手工新建: %v", err)
	}
	if err := svc.SaveApiEntry(store.ApiEntry{ProjectID: projectID, Name: "又一个", Method: "GET", PathRaw: "/api/v1/demo/manual"}); err == nil ||
		!strings.Contains(err.Error(), "已存在") {
		t.Fatalf("同 key 重复新建应被拒: %v", err)
	}
	// 软删 + 恢复
	manualID := mustEntryByIdentity(t, svc, projectID, "GET|/api/v1/demo/manual").ID
	if err := svc.DeleteApiEntry(manualID); err != nil {
		t.Fatalf("DeleteApiEntry: %v", err)
	}
	if e := mustEntryByIdentity(t, svc, projectID, "GET|/api/v1/demo/manual"); e.Status != store.ApiEntryStatusRemoved {
		t.Fatalf("应软删: %s", e.Status)
	}
	if err := svc.RestoreApiEntry(manualID); err != nil {
		t.Fatalf("RestoreApiEntry: %v", err)
	}
	if e := mustEntryByIdentity(t, svc, projectID, "GET|/api/v1/demo/manual"); e.Status != store.ApiEntryStatusActive || e.DeletedAt != 0 {
		t.Fatalf("应恢复: status=%s deletedAt=%d", e.Status, e.DeletedAt)
	}
	if apiEventCount(t, svc, projectID) != 1 {
		t.Fatal("软删/恢复也不得产生事件（手工管理路径）")
	}
}

// ===================== 项目级联删除（9 张表零残留）=====================

func TestDeleteApiProjectCascade(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true})
	voDoc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "feature", "补一个 VO", "级联删除用例", "REQ-9") +
		"## 一、用户管理\n\n" +
		"### 1.7 查询概览\n\n带命名结构。\n\n" +
		"- **URL**：`GET /api/v1/demo/overview`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiRespSection("| data | OverviewVO | 概览 |") +
		"**OverviewVO 结构**：\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| total | Int | 总数 |\n"
	voRes := commitApiDoc(t, svc, ApiDocImportRequest{DocText: voDoc})
	_ = res

	pw := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	schemas, _ := svc.store.ListApiSchemas(projectID)
	if len(schemas) == 0 {
		t.Fatal("用例前提：应存在命名结构")
	}
	events, _ := svc.ListApiChangeEvents(ApiEventQuery{ProjectID: projectID})
	if len(events) != 3 {
		t.Fatalf("用例前提：事件数 = %d, want 3", len(events))
	}

	if err := svc.DeleteApiProject(projectID); err != nil {
		t.Fatalf("DeleteApiProject: %v", err)
	}
	// 9 张表逐一验证零残留
	if _, found, _ := svc.store.FindApiProjectByName("demo-svc"); found {
		t.Fatal("api_projects 残留")
	}
	if ds, _ := svc.ListApiDomains(projectID); len(ds) != 0 {
		t.Fatalf("api_domains 残留: %d", len(ds))
	}
	if es := apiAllEntries(t, svc, projectID); len(es) != 0 {
		t.Fatalf("api_entries 残留: %d", len(es))
	}
	if fs, _ := svc.store.ListApiFields(store.ApiOwnerEndpoint, pw.ID); len(fs) != 0 {
		t.Fatalf("api_fields(endpoint) 残留: %d", len(fs))
	}
	if xs, _ := svc.store.ListApiExamples(store.ApiOwnerEndpoint, pw.ID); len(xs) != 0 {
		t.Fatalf("api_examples 残留: %d", len(xs))
	}
	if ss, _ := svc.store.ListApiSchemas(projectID); len(ss) != 0 {
		t.Fatalf("api_schemas 残留: %d", len(ss))
	}
	if fs, _ := svc.store.ListApiFields(store.ApiOwnerSchema, schemas[0].ID); len(fs) != 0 {
		t.Fatalf("api_fields(schema) 残留: %d", len(fs))
	}
	if evs, _ := svc.ListApiChangeEvents(ApiEventQuery{ProjectID: projectID}); len(evs) != 0 {
		t.Fatalf("api_change_events 残留: %d", len(evs))
	}
	if items, _ := svc.store.ListApiChangeItems(voRes.EventID); len(items) != 0 {
		t.Fatalf("api_change_items 残留: %d", len(items))
	}
	if bs, _ := svc.store.ListApiImportBatches("demo-svc|/api/v1/demo|演示服务接口变更文档", 0); len(bs) != 0 {
		t.Fatalf("api_import_batches 残留: %d", len(bs))
	}
}

// ===================== 预览（只读）与规范导出 =====================

func TestPreviewApiDocImportReadOnly(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)
	entriesBefore := len(apiAllEntries(t, svc, projectID))
	eventsBefore := apiEventCount(t, svc, projectID)

	req := ApiDocImportRequest{DocText: demoAC1Doc()}
	pv, err := svc.PreviewApiDocImport(req)
	if err != nil {
		t.Fatalf("PreviewApiDocImport: %v", err)
	}
	if !pv.OK || !pv.NeedConfirm || pv.Breaking != store.ApiBreakingYes {
		t.Fatalf("预览裁决不对: ok=%v needConfirm=%v breaking=%d", pv.OK, pv.NeedConfirm, pv.Breaking)
	}
	if pv.NextSeq != 2 {
		t.Fatalf("nextSeq = %d, want 2", pv.NextSeq)
	}
	if pv.Stats.Added != 2 || pv.Stats.Updated != 1 || pv.Stats.Removed != 1 {
		t.Fatalf("预览 stats = %+v", pv.Stats)
	}
	if len(pv.Changes) != 4 {
		t.Fatalf("逐接口预览 = %d 条, want 4", len(pv.Changes))
	}
	var sawUpdated bool
	for _, c := range pv.Changes {
		if c.ChangeType == store.ApiChangeTypeUpdated {
			sawUpdated = true
			if len(c.Diff) != 1 || c.Diff[0].Key != "oldPwd" || c.DeclaredNote == "" || c.ParseState == "" {
				t.Fatalf("updated 预览行不对: %+v", c)
			}
		}
	}
	if !sawUpdated {
		t.Fatal("预览缺 updated 行")
	}
	// 领域候选（原始标题 / 归一名 / visibility / 条目数）
	if len(pv.Domains) != 1 || pv.Domains[0].Name != "用户管理" || pv.Domains[0].EntryCount != 4 ||
		pv.Domains[0].RawTitle != "一、用户管理" {
		t.Fatalf("领域候选不对: %+v", pv.Domains)
	}
	// 只读：库零变化
	if len(apiAllEntries(t, svc, projectID)) != entriesBefore || apiEventCount(t, svc, projectID) != eventsBefore {
		t.Fatal("预览不得写库")
	}

	// 坏文档：OK=false 且 issues 带错误码（文档 bug 与变更分栏，changes 为空）
	bad, err := svc.PreviewApiDocImport(ApiDocImportRequest{
		DocText: "## 一、x\n\n### 1.1 y\n\n- **URL:** `GET /y`\n", ProjectName: "bad-svc"})
	if err != nil {
		t.Fatalf("坏文档预览不应返回 Go error: %v", err)
	}
	if bad.OK || len(bad.Issues) == 0 || len(bad.Changes) != 0 {
		t.Fatalf("坏文档预览不对: ok=%v issues=%d changes=%d", bad.OK, len(bad.Issues), len(bad.Changes))
	}
	var sawCode bool
	for _, is := range bad.Issues {
		if is.Code == apidoc.ErrURLFormat && is.Line > 0 {
			sawCode = true
		}
	}
	if !sawCode {
		t.Fatalf("预览 issues 应含 %s（带行号）: %+v", apidoc.ErrURLFormat, bad.Issues)
	}

	// 领域改名/合并 override 在预览可见
	pvOv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: demoAC1Doc(),
		DomainOverrides: []ApiDomainOverride{{RawTitle: "一、用户管理", Name: "账号与日志"}}})
	if err != nil {
		t.Fatalf("PreviewApiDocImport(override): %v", err)
	}
	if len(pvOv.Domains) != 1 || pvOv.Domains[0].Name != "账号与日志" {
		t.Fatalf("override 改名未生效: %+v", pvOv.Domains)
	}
	// override 生效后提交：领域按新名落库
	resOv := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true,
		DomainOverrides: []ApiDomainOverride{{RawTitle: "一、用户管理", Name: "账号与日志"}}})
	domains, err := svc.ListApiDomains(resOv.ProjectID)
	if err != nil {
		t.Fatalf("ListApiDomains: %v", err)
	}
	var sawRenamed bool
	for _, d := range domains {
		if d.Name == "账号与日志" {
			sawRenamed = true
		}
	}
	if !sawRenamed {
		t.Fatalf("改名后的领域未落库: %+v", domains)
	}
	added := mustEntryByIdentity(t, svc, resOv.ProjectID, "GET|/api/v1/demo/login-log")
	if added.DomainID == "" {
		t.Fatal("条目应挂到改名后的领域")
	}
}

func TestExportApiDocSpecSingleSource(t *testing.T) {
	svc := newTestService(t)
	text, err := svc.ExportApiDocSpec()
	if err != nil {
		t.Fatalf("ExportApiDocSpec: %v", err)
	}
	// AC9 前提：与 apidoc.SpecText 同一常量（skill prompt 与界面导出字节一致）
	if text != apidoc.SpecText || !strings.Contains(text, "apidoc-meta") {
		t.Fatal("导出规范必须与 apidoc.SpecText 字节一致")
	}
}

// ===================== 列表查询参数 =====================

func TestListApiEntriesQueryFilters(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)
	commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true})

	// keyword：名称模糊
	byName, err := svc.ListApiEntries(ApiEntryQuery{ProjectID: projectID, Keyword: "密码"})
	if err != nil || len(byName) != 1 || byName[0].Method != "POST" {
		t.Fatalf("按名称搜索不对: %v %+v", err, byName)
	}
	// keyword：路径模糊（大小写不敏感）
	byPath, err := svc.ListApiEntries(ApiEntryQuery{ProjectID: projectID, Keyword: "LOGIN-LOG"})
	if err != nil || len(byPath) != 2 {
		t.Fatalf("按路径搜索不对: %v %+v", err, byPath)
	}
	// includeRemoved
	withRemoved, err := svc.ListApiEntries(ApiEntryQuery{ProjectID: projectID, IncludeRemoved: true})
	if err != nil || len(withRemoved) != 4 {
		t.Fatalf("includeRemoved 不对: %v %+v", err, withRemoved)
	}
	// 缺 projectID → 中文错误
	if _, err := svc.ListApiEntries(ApiEntryQuery{}); err == nil || !strings.Contains(err.Error(), "项目") {
		t.Fatalf("缺 projectID 应中文报错: %v", err)
	}
	if _, err := svc.ListApiChangeEvents(ApiEventQuery{}); err == nil {
		t.Fatal("事件列表缺 projectID 应报错")
	}
}

// ===================== ① 手工编辑：服务端保留列（读-改-写）=====================

// assertRetainedColumnsIntact 逐字节比对服务端保留列（漏一个就是静默数据损坏）。
func assertRetainedColumnsIntact(t *testing.T, tag string, got, want store.ApiEntry) {
	t.Helper()
	for _, c := range []struct {
		col       string
		got, want any
	}{
		{"id", got.ID, want.ID},
		{"project_id", got.ProjectID, want.ProjectID},
		{"identity_key", got.IdentityKey, want.IdentityKey},
		{"method", got.Method, want.Method},
		{"path_raw", got.PathRaw, want.PathRaw},
		{"base_path_assert", got.BasePathAssert, want.BasePathAssert},
		{"semantic_hash", got.SemanticHash, want.SemanticHash},
		{"doc_hash", got.DocHash, want.DocHash},
		{"parse_state", got.ParseState, want.ParseState},
		{"raw_markdown", got.RawMarkdown, want.RawMarkdown},
		{"status", got.Status, want.Status},
		{"deleted_at", got.DeletedAt, want.DeletedAt},
		{"created_at", got.CreatedAt, want.CreatedAt},
	} {
		if c.got != c.want {
			t.Fatalf("%s: 服务端保留列 %s 被手工编辑改写: %v, want %v", tag, c.col, c.got, c.want)
		}
	}
}

func TestSaveApiEntryKeepsServerOwnedColumns(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)
	before := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	if before.RawMarkdown == "" || before.SemanticHash == "" || before.DocHash == "" ||
		before.BasePathAssert == "" || before.ParseState == "" {
		t.Fatalf("用例前提：保留列应已随导入落库（raw=%d sha=%d doc=%d assert=%q state=%q）",
			len(before.RawMarkdown), len(before.SemanticHash), len(before.DocHash),
			before.BasePathAssert, before.ParseState)
	}

	// 脏值入参：保留列一律被服务端忽略（PR4a 靠前端深拷贝自律，这里把闸门收到服务端）
	dirty := before
	dirty.Name = "手工改的名"
	dirty.Description = "手工写的说明"
	dirty.ParseState = store.ApiParseStateOK
	dirty.RawMarkdown = ""
	dirty.SemanticHash = "deadbeef"
	dirty.DocHash = "cafebabe"
	dirty.BasePathAssert = ""
	dirty.Status = store.ApiEntryStatusRemoved // status/deletedAt 只能通过 Delete/Restore 改
	dirty.DeletedAt = 1700000000000
	dirty.Method = "" // 漏传身份列 = 不改（传了才校验一致性）
	dirty.PathRaw = ""
	if err := svc.SaveApiEntry(dirty); err != nil {
		t.Fatalf("SaveApiEntry(脏值): %v", err)
	}
	after := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	assertRetainedColumnsIntact(t, "脏值入参", after, before)
	// 白名单列确实改到了
	if after.Name != "手工改的名" || after.Description != "手工写的说明" {
		t.Fatalf("白名单列未生效: name=%q desc=%q", after.Name, after.Description)
	}
	// 文本列入参为准（传空即清空）——已在方法注释写明，钉住这条语义
	dirty2 := after
	dirty2.ContentType = ""
	dirty2.Auth = ""
	if err := svc.SaveApiEntry(dirty2); err != nil {
		t.Fatalf("SaveApiEntry(清文本列): %v", err)
	}
	if cleared := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password"); cleared.ContentType != "" || cleared.Auth != "" {
		t.Fatalf("文本列应允许清空: content_type=%q auth=%q", cleared.ContentType, cleared.Auth)
	}
	// 手工编辑不产生事件（§13.19 的另一半）
	if n := apiEventCount(t, svc, projectID); n != 1 {
		t.Fatalf("手工编辑不得产生事件: %d", n)
	}

	// partial 条目：手工编辑不得把「有未解析项」这个事实悄悄改成 ok（store 空值兜底的陷阱）
	userRes := commitApiDoc(t, svc, ApiDocImportRequest{
		DocText: readApiFixture(t, "user-service.md"), ProjectName: "user-svc-partial", InitMode: true})
	var partial store.ApiEntry
	for _, e := range apiAllEntries(t, svc, userRes.ProjectID) {
		if e.ParseState != store.ApiParseStatePartial {
			continue
		}
		// 列表 SQL 有意不带 raw_markdown，必须按 id 取整行做逐字节对照
		full, err := svc.store.GetApiEntry(e.ID)
		if err != nil {
			t.Fatalf("GetApiEntry: %v", err)
		}
		if full.RawMarkdown == "" {
			continue
		}
		partial = full
		break
	}
	if partial.ID == "" {
		t.Fatal("用例前提：init 灌散文文档应产出 parse_state=partial 且 raw_markdown 非空的条目")
	}
	p := partial
	p.ParseState = store.ApiParseStateOK
	p.RawMarkdown = ""
	p.SemanticHash = ""
	p.DocHash = ""
	p.Name = "改了名字"
	if err := svc.SaveApiEntry(p); err != nil {
		t.Fatalf("SaveApiEntry(partial 条目): %v", err)
	}
	got := mustEntryByIdentity(t, svc, userRes.ProjectID, partial.IdentityKey)
	assertRetainedColumnsIntact(t, "partial 条目脏值", got, partial)
	if got.ParseState != store.ApiParseStatePartial {
		t.Fatalf("parse_state 被改写: %s, want partial", got.ParseState)
	}
	if got.Name != "改了名字" {
		t.Fatalf("改名未生效: %q", got.Name)
	}
}

func TestSaveApiEntryManualGuards(t *testing.T) {
	svc := newTestService(t)
	projectID := seedDemoBaseline(t, svc)
	entry := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")

	// 跨项目移动：入参 projectId 与库里不一致即拒（中文错误）
	cross := entry
	cross.ProjectID = "another-project-id"
	if err := svc.SaveApiEntry(cross); err == nil || !strings.Contains(err.Error(), "项目") {
		t.Fatalf("跨项目移动应被拒: %v", err)
	}
	// method/path 传了就必须与库里身份键一致（保留列不接受入参 ≠ 允许改身份）
	badIdentity := entry
	badIdentity.PathRaw = "/api/v1/demo/password2"
	if err := svc.SaveApiEntry(badIdentity); err == nil || !strings.Contains(err.Error(), "身份") {
		t.Fatalf("改路径应被拒: %v", err)
	}

	// 裸载荷（只有 id + name）：漏传的归属/可见性/排序必须原样保留，而不是清零或落到 public
	bare := store.ApiEntry{ID: entry.ID, ProjectID: projectID, Name: "裸载荷"}
	if err := svc.SaveApiEntry(bare); err != nil {
		t.Fatalf("SaveApiEntry(裸载荷): %v", err)
	}
	after := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	if after.DomainID != entry.DomainID || after.Visibility != entry.Visibility || after.Sort != entry.Sort {
		t.Fatalf("漏传列被改写: domain=%s→%s visibility=%s→%s sort=%d→%d",
			entry.DomainID, after.DomainID, entry.Visibility, after.Visibility, entry.Sort, after.Sort)
	}
	assertRetainedColumnsIntact(t, "裸载荷", after, entry)

	// 别的项目的领域不允许挂（中文错误）
	other := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoTwoDomainDoc()})
	otherDomainID := ""
	for _, d := range mustApiDomains(t, svc, other.ProjectID) {
		if !d.Internal {
			otherDomainID = d.ID
		}
	}
	if otherDomainID == "" {
		t.Fatal("用例前提：另一个项目应有一个非 internal 领域")
	}
	mine := entry
	mine.DomainID = otherDomainID
	if err := svc.SaveApiEntry(mine); err == nil || !strings.Contains(err.Error(), "项目") {
		t.Fatalf("挂到别的项目的领域应被拒: %v", err)
	}

	// 同项目内的第二个领域：带改名 override 的真实变更导入会另建终名领域行（D19 条目可移动归属）
	ren := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true,
		DomainOverrides: []ApiDomainOverride{{RawTitle: "一、用户管理", Name: "账号管理"}}})
	if len(ren.CreatedDomains) != 1 || ren.CreatedDomains[0] != "账号管理" {
		t.Fatalf("override 改名应另建终名领域行: %v", ren.CreatedDomains)
	}
	newDomainID := ""
	for _, d := range mustApiDomains(t, svc, projectID) {
		if d.Name == "账号管理" {
			newDomainID = d.ID
		}
	}
	if newDomainID == "" {
		t.Fatal("用例前提：改名后的领域行应存在")
	}
	current := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	mv := current
	mv.DomainID = newDomainID
	mv.Sort = 77
	if err := svc.SaveApiEntry(mv); err != nil {
		t.Fatalf("SaveApiEntry(移动归属): %v", err)
	}
	moved := mustEntryByIdentity(t, svc, projectID, "POST|/api/v1/demo/password")
	if moved.DomainID != newDomainID || moved.Sort != 77 {
		t.Fatalf("归属/排序未生效: domain=%s sort=%d", moved.DomainID, moved.Sort)
	}
	assertRetainedColumnsIntact(t, "移动归属", moved, current)
}

// demoTwoDomainDoc 两个领域的规范文档（其一是「（内部）」章节 → 领域 internal=true），
// 供领域手工改名/排序/删除与「保留列不被入参清空」用例使用。
func demoTwoDomainDoc() string {
	return "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-two", "feature", "两个领域铺底", "领域手工管理用例铺底", "REQ-20") +
		"## 一、用户管理\n\n" +
		"### 1.1 查询用户\n\n按 id 查询用户。\n\n" +
		"- **URL**：`GET /api/v1/demo/two/user`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：新增\n\n" +
		apiRespSection("| message | String | 响应消息 |") +
		"## 二、RPC 论文写入接口（内部）\n\n" +
		"### 2.1 内部刷新\n\n内部 RPC 刷新。\n\n" +
		"- **URL**：`POST /api/v1/demo/two/rpc-refresh`\n" +
		"- **认证**：内部Token\n" +
		"- **变更**：新增\n\n" +
		apiRespSection("| message | String | 响应消息 |")
}

// mustApiDomains 项目下领域列表（按 sort；失败即 Fatal）。
func mustApiDomains(t *testing.T, svc *Service, projectID string) []store.ApiDomain {
	t.Helper()
	list, err := svc.ListApiDomains(projectID)
	if err != nil {
		t.Fatalf("ListApiDomains: %v", err)
	}
	return list
}

// ===================== ② 预览回显文档 meta =====================

func TestPreviewApiDocImportEchoesDocMeta(t *testing.T) {
	svc := newTestService(t)
	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: demoBaselineDoc()})
	if err != nil {
		t.Fatalf("PreviewApiDocImport: %v", err)
	}
	want := ApiDocMetaPreview{
		Project: "demo-svc", Service: "演示服务", BasePath: "/api/v1/demo", Scope: "incremental",
		ChangeType: "feature", ChangeTitle: "初始化演示接口",
		ChangeNote: "为后续变更用例铺底", VersionTag: "REQ-0",
	}
	if pv.Meta != want {
		t.Fatalf("meta 回显 = %+v, want %+v", pv.Meta, want)
	}

	// 校验失败的文档也必须回显 meta（用户得知道文档自己声明了什么）
	badDoc := strings.Replace(demoBaselineDoc(), "- **URL**：`POST", "- **URL:** `POST", 1)
	bad, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: badDoc})
	if err != nil {
		t.Fatalf("坏文档预览不应返回 Go error: %v", err)
	}
	if bad.OK || len(bad.Issues) == 0 {
		t.Fatalf("坏文档应 OK=false 且带 issues: ok=%v issues=%d", bad.OK, len(bad.Issues))
	}
	if bad.Meta.ChangeNote != "为后续变更用例铺底" || bad.Meta.Project != "demo-svc" {
		t.Fatalf("校验失败时 meta 也要回显: %+v", bad.Meta)
	}

	// 散文文档（无 apidoc-meta 块）：逐项空串、不报错（前端据此显示「文档未声明」）
	prose, err := svc.PreviewApiDocImport(ApiDocImportRequest{
		DocText: readApiFixture(t, "user-service.md"), ProjectName: "user-svc-prose", InitMode: true})
	if err != nil {
		t.Fatalf("散文文档预览失败: %v", err)
	}
	if prose.Meta != (ApiDocMetaPreview{}) {
		t.Fatalf("散文文档 meta 应全为空串: %+v", prose.Meta)
	}
}

// ===================== ③ 领域手工改名/排序/删除（PRD D19）=====================

func TestSaveApiDomainManual(t *testing.T) {
	svc := newTestService(t)
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoTwoDomainDoc()})
	projectID := res.ProjectID
	domains := mustApiDomains(t, svc, projectID)
	if len(domains) != 2 {
		t.Fatalf("用例前提：领域数 = %d, want 2", len(domains))
	}
	var plain, internal store.ApiDomain
	for _, d := range domains {
		if d.Internal {
			internal = d
		} else {
			plain = d
		}
	}
	if internal.ID == "" || plain.ID == "" {
		t.Fatalf("用例前提：应有一个 internal 领域: %+v", domains)
	}

	// 手工新建领域被拒（D19：领域由文档章节产生）
	if err := svc.SaveApiDomain(store.ApiDomain{ProjectID: projectID, Name: "手工领域"}); err == nil ||
		!strings.Contains(err.Error(), "不支持手工新建") {
		t.Fatalf("手工新建领域应被拒: %v", err)
	}
	// 改名生效；入参不带 internal/notesRaw 也不得被清空（服务端保留列）
	if err := svc.SaveApiDomain(store.ApiDomain{ID: internal.ID, ProjectID: projectID, Name: "内部 RPC 通道"}); err != nil {
		t.Fatalf("SaveApiDomain(改名): %v", err)
	}
	renamed, err := svc.store.GetApiDomain(internal.ID)
	if err != nil {
		t.Fatalf("GetApiDomain: %v", err)
	}
	if renamed.Name != "内部 RPC 通道" || !renamed.Internal || renamed.NotesRaw != internal.NotesRaw ||
		renamed.CreatedAt != internal.CreatedAt {
		t.Fatalf("改名把保留列洗掉了: %+v（原 %+v）", renamed, internal)
	}
	// 排序生效
	if err := svc.SaveApiDomain(store.ApiDomain{ID: plain.ID, Sort: 99}); err != nil {
		t.Fatalf("SaveApiDomain(排序): %v", err)
	}
	ordered := mustApiDomains(t, svc, projectID)
	if len(ordered) != 2 || ordered[0].ID != renamed.ID || ordered[1].ID != plain.ID {
		t.Fatalf("排序未生效: %+v", ordered)
	}
	// 同项目内撞名被拒（中文错误，不是裸的 UNIQUE constraint failed）
	if err := svc.SaveApiDomain(store.ApiDomain{ID: renamed.ID, Name: plain.Name}); err == nil ||
		!strings.Contains(err.Error(), "已存在") {
		t.Fatalf("撞名应被拒: %v", err)
	}
	// 改归属项目被拒
	other := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoBaselineDoc()})
	if err := svc.SaveApiDomain(store.ApiDomain{ID: plain.ID, ProjectID: other.ProjectID, Name: "搬走"}); err == nil ||
		!strings.Contains(err.Error(), "项目") {
		t.Fatalf("改归属项目应被拒: %v", err)
	}
	// 空名 + sort 0 = 什么都不改（「漏传即不改」而不是「漏传即清空」）
	if err := svc.SaveApiDomain(store.ApiDomain{ID: plain.ID, Name: "   "}); err != nil {
		t.Fatalf("SaveApiDomain(空值入参): %v", err)
	}
	untouched, err := svc.store.GetApiDomain(plain.ID)
	if err != nil {
		t.Fatalf("GetApiDomain: %v", err)
	}
	if untouched.Name != plain.Name || untouched.Sort != 99 {
		t.Fatalf("空值入参改了列: name=%q sort=%d（want %q / 99）", untouched.Name, untouched.Sort, plain.Name)
	}
	// 空 id（= 手工新建领域）被拒
	if err := svc.SaveApiDomain(store.ApiDomain{Name: "x"}); err == nil {
		t.Fatal("空 id 应被拒")
	}
	// 手工改领域不产生事件
	if n := apiEventCount(t, svc, projectID); n != 1 {
		t.Fatalf("领域手工编辑不得产生事件: %d", n)
	}
}

func TestDeleteApiDomainOnlyEmpty(t *testing.T) {
	svc := newTestService(t)
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoTwoDomainDoc()})
	projectID := res.ProjectID
	domains := mustApiDomains(t, svc, projectID)
	var internal store.ApiDomain
	for _, d := range domains {
		if d.Internal {
			internal = d
		}
	}
	if internal.ID == "" {
		t.Fatal("用例前提：应有一个 internal 领域")
	}
	// 领域下有 active 条目 → 拒收，错误里要说清条数
	if err := svc.DeleteApiDomain(internal.ID); err == nil ||
		!strings.Contains(err.Error(), "1 个接口") || !strings.Contains(err.Error(), "无法删除") {
		t.Fatalf("删非空领域应被拒并带条数: %v", err)
	}
	// 空 id 与不存在的 id
	if err := svc.DeleteApiDomain(""); err == nil {
		t.Fatal("空 id 应被拒")
	}
	if err := svc.DeleteApiDomain("no-such-domain"); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("不存在的领域应中文报错: %v", err)
	}

	// 把领域里的条目软删干净 → 领域可删
	entries := apiAllEntries(t, svc, projectID)
	var removedID string
	for _, e := range entries {
		if e.DomainID == internal.ID {
			if err := svc.DeleteApiEntry(e.ID); err != nil {
				t.Fatalf("DeleteApiEntry: %v", err)
			}
			removedID = e.ID
		}
	}
	if removedID == "" {
		t.Fatal("用例前提：internal 领域下应有条目")
	}
	if err := svc.DeleteApiDomain(internal.ID); err != nil {
		t.Fatalf("DeleteApiDomain(空领域): %v", err)
	}
	if _, err := svc.store.GetApiDomain(internal.ID); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("领域行应已物理删除: %v", err)
	}
	if n := len(mustApiDomains(t, svc, projectID)); n != len(domains)-1 {
		t.Fatalf("领域数 = %d, want %d", n, len(domains)-1)
	}
	// 软删条目必须留着（事件按 entry_id 跳转，硬删会让引用悬空，R7），只是改挂「未归领域」
	survivor, err := svc.store.GetApiEntry(removedID)
	if err != nil {
		t.Fatalf("领域删除后软删条目应仍存在: %v", err)
	}
	if survivor.Status != store.ApiEntryStatusRemoved || survivor.DomainID != "" {
		t.Fatalf("软删条目应保留且脱离已删领域: status=%s domain=%q", survivor.Status, survivor.DomainID)
	}
	// 历史事件的 domain_name 冗余快照不受影响（这就是物理删可行的理由）
	items, err := svc.store.ListApiChangeItems(res.EventID)
	if err != nil {
		t.Fatalf("ListApiChangeItems: %v", err)
	}
	var sawSnapshot bool
	for _, it := range items {
		if it.EntryID == removedID && it.DomainName == internal.Name {
			sawSnapshot = true
		}
	}
	if !sawSnapshot {
		t.Fatalf("删领域后事件的领域名快照必须仍读得到: %+v", items)
	}
	if n := apiEventCount(t, svc, projectID); n != 1 {
		t.Fatalf("删领域不得产生/销毁事件: %d", n)
	}
}

// ===================== ④ 提交结果卡字段 =====================

func TestCommitApiDocImportResultCards(t *testing.T) {
	svc := newTestService(t)

	// 首次导入新文档：新建项目 + 新建领域，无降级提示
	first := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoBaselineDoc()})
	if !first.CreatedProject {
		t.Fatal("首次导入应标记 createdProject")
	}
	if len(first.CreatedDomains) != 1 || first.CreatedDomains[0] != "用户管理" {
		t.Fatalf("createdDomains = %v, want [用户管理]", first.CreatedDomains)
	}
	if len(first.Notes) != 0 {
		t.Fatalf("首次导入不应有 notes: %v", first.Notes)
	}
	if first.ProjectID == "" {
		t.Fatal("结果必须带回项目 id")
	}

	// 重复导入（幂等短路）：不新建任何东西，中文提示必须如实带回（结果卡唯一解释）
	second := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoBaselineDoc()})
	if second.CreatedProject {
		t.Fatal("幂等重导不得标记 createdProject")
	}
	if len(second.CreatedDomains) != 0 {
		t.Fatalf("幂等重导不得新建领域: %v", second.CreatedDomains)
	}
	if len(second.Notes) != 2 {
		t.Fatalf("幂等重导应有 2 条陈旧声明降级提示: %v", second.Notes)
	}
	for _, n := range second.Notes {
		if !strings.Contains(n, "已存在且一致") {
			t.Fatalf("提示文案不对: %q", n)
		}
	}
	if !strings.Contains(second.Message, "无变更") {
		t.Fatalf("幂等短路汇总不对: %q", second.Message)
	}

	// 导入预览里改名 → 新领域行按「落库终名」新建（含 override 后的名字），项目不新建。
	// 用带真实变更的 AC1 文档：幂等短路的导入根本不写领域行（见上一个用例）。
	third := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true,
		DomainOverrides: []ApiDomainOverride{{RawTitle: "一、用户管理", Name: "账号管理"}}})
	if third.CreatedProject {
		t.Fatal("同项目再导入不得标记 createdProject")
	}
	if len(third.CreatedDomains) != 1 || third.CreatedDomains[0] != "账号管理" {
		t.Fatalf("createdDomains 应是落库终名: %v", third.CreatedDomains)
	}
	if third.EventID == 0 || len(third.Notes) != 0 {
		t.Fatalf("有真实变更时不应有降级提示且应生成事件: %+v", third)
	}

	// 多领域文档：一次导入新建两个领域（含 internal 领域）
	multi := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoTwoDomainDoc()})
	if !multi.CreatedProject || len(multi.CreatedDomains) != 2 {
		t.Fatalf("多领域文档结果卡不对: createdProject=%v domains=%v", multi.CreatedProject, multi.CreatedDomains)
	}
	// 幂等：JS 侧要拿到 [] 而不是 null
	if multi.Notes == nil || third.CreatedDomains == nil {
		t.Fatalf("数组字段不得为 nil: notes=%v domains=%v", multi.Notes, third.CreatedDomains)
	}
}
