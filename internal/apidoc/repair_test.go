// 确定性修复器测试（任务 10-20-chat-file-import，PRD R2/D2 + DoD 性质清单）。
// 仓库约定：真文件（golden fixture）+ 手写 t.Fatalf，无断言库。
//
// 三条必证性质（dispatch ①）：
//
//	① 幂等：Repair(Repair(x)) 与 Repair(x) 结果一致（第二次零修复）；
//	② golden fixture：改动必须为 0 或仅白名单内改动，且修复后解析接口数不变（20/11）；
//	③ 语义字段零改动：修复前后解析产物里所有接口的
//	   name/method/path/字段名/类型/必填/说明（含 Position——解析器本就按同一规则推断）
//	   逐字节相等；交叉引用搬运只允许「新增字段」且必须逐字节等于被引用表的内容。
package apidoc

import (
	"reflect"
	"strings"
	"testing"
)

var repairWhitelistKinds = map[string]bool{
	FixBulletForm: true, FixFenceClose: true, FixPosColumn: true, FixCrossRefExpand: true,
}

// assertSemanticsPreserved 性质③：orig 侧每个接口的身份三元组与全部字段必须逐字节保留
// （允许 fixed 侧在字段表「后面」追加搬运来的字段——交叉引用展开的合法产物；禁止删改）。
func assertSemanticsPreserved(t *testing.T, orig, fixed *Document) {
	t.Helper()
	if len(fixed.Endpoints) != len(orig.Endpoints) {
		t.Fatalf("接口数变了: %d → %d", len(orig.Endpoints), len(fixed.Endpoints))
	}
	byKey := map[string]*Endpoint{}
	for _, ep := range fixed.Endpoints {
		byKey[ep.IdentityKey] = ep
	}
	for _, o := range orig.Endpoints {
		f := byKey[o.IdentityKey]
		if f == nil {
			t.Fatalf("接口消失: %s", o.IdentityKey)
		}
		if o.Name != f.Name || o.Method != f.Method || o.PathRaw != f.PathRaw {
			t.Fatalf("接口身份字段变了 %s: %q/%s/%q → %q/%s/%q",
				o.IdentityKey, o.Name, o.Method, o.PathRaw, f.Name, f.Method, f.PathRaw)
		}
		for _, slot := range []struct {
			label         string
			before, after []Field
		}{
			{"path_params", o.PathFields, f.PathFields},
			{"request_params", o.RequestFields, f.RequestFields},
			{"response_params", o.ResponseFields, f.ResponseFields},
		} {
			if len(slot.after) < len(slot.before) {
				t.Fatalf("%s %s 字段被删: %d → %d", o.IdentityKey, slot.label, len(slot.before), len(slot.after))
			}
			for i := range slot.before {
				a, b := slot.before[i], slot.after[i]
				// Line 不参与比较（插入会平移行号），其余逐字节。
				ga, gb := a, b
				ga.Line, gb.Line = 0, 0
				if !reflect.DeepEqual(ga, gb) {
					t.Fatalf("%s %s 第 %d 个字段变了:\n  before=%+v\n  after =%+v", o.IdentityKey, slot.label, i, a, b)
				}
			}
		}
	}
}

// assertWhitelistFixes 性质②前半：每一处改动都必须是白名单四类之一，行号有效，
// 位置列推断类一律 NeedsReview=true（D2）。
func assertWhitelistFixes(t *testing.T, fixes []Fix) {
	t.Helper()
	for _, f := range fixes {
		if !repairWhitelistKinds[f.Kind] {
			t.Fatalf("白名单外的修复类型 %q: %+v", f.Kind, f)
		}
		if f.Line < 1 {
			t.Fatalf("修复缺少有效行号: %+v", f)
		}
		if f.Kind == FixPosColumn && !f.NeedsReview {
			t.Fatalf("位置列补列必须标待人工核对: %+v", f)
		}
		if strings.TrimSpace(f.Note) == "" {
			t.Fatalf("修复必须说明理由: %+v", f)
		}
	}
}

// ===================== golden fixture：三条性质 =====================

func TestRepairGoldenFixturesInvariants(t *testing.T) {
	cases := []struct {
		file    string
		wantEps int
	}{
		{"user-service.md", 20},
		{"paper-service.md", 11},
	}
	for _, c := range cases {
		src := readFixture(t, c.file)
		fixed, fixes, _ := Repair(src)

		assertWhitelistFixes(t, fixes)

		// ① 幂等
		fixed2, fixes2, _ := Repair(fixed)
		if fixed2 != fixed {
			t.Fatalf("%s 修复不幂等: 第二次输出与第一次不一致（%d vs %d 字节）", c.file, len(fixed), len(fixed2))
		}
		if len(fixes2) != 0 {
			t.Fatalf("%s 第二次修复应零改动，实际 %d 条: %+v", c.file, len(fixes2), fixes2)
		}

		// ② 接口数不变（fixture 按各自档位解析；修复后不得新增 E-*）
		origDoc := parseInit(t, src)
		fixedDoc := parseInit(t, fixed)
		if len(origDoc.Endpoints) != c.wantEps {
			t.Fatalf("%s 原接口数 = %d, want %d（fixture 断言漂移？）", c.file, len(origDoc.Endpoints), c.wantEps)
		}
		// ③ 语义字段零改动
		assertSemanticsPreserved(t, origDoc, fixedDoc)

		kinds := map[string]int{}
		for _, f := range fixes {
			kinds[f.Kind]++
		}
		t.Logf("%s: 修复 %d 处 %v，接口数 %d 保持不变", c.file, len(fixes), kinds, len(fixedDoc.Endpoints))

		// 确定性：同输入两次输出字节一致（修复器与解析器同一纪律）
		if again, _, _ := Repair(src); again != fixed {
			t.Fatalf("%s 修复不确定: 同输入两次输出不一致", c.file)
		}
	}
}

// paper fixture 里就有「元素结构见 7.1」「同 7.1」两处引用式写法——
// 交叉引用搬运必须在白名单内发生，且搬运源（7.1 请求参数表）被补过推断位置列，
// 请求侧那条搬运要标待核对。
func TestRepairGoldenPaperCrossRefExpanded(t *testing.T) {
	src := readFixture(t, "paper-service.md")
	_, fixes, fixedDoc := Repair(src)
	var expanded int
	var reviewedFromPosInsert int
	for _, f := range fixes {
		if f.Kind != FixCrossRefExpand {
			continue
		}
		expanded++
		if f.NeedsReview {
			reviewedFromPosInsert++
		}
	}
	if expanded == 0 {
		t.Fatalf("paper fixture 的白名单内交叉引用（7.2 元素结构见 7.1 / 同 7.1）没有被展开: %+v", fixes)
	}
	if reviewedFromPosInsert == 0 {
		t.Fatalf("搬运含推断位置列的表应传递待核对标记，一条都没有")
	}
	// 展开后 7.2 的请求槽位必须真的有了字段（搬运产物逐字节来自 7.1 的表）
	for _, ep := range fixedDoc.Endpoints {
		if ep.Seq == "7.2" {
			if ep.ReqParamsState != SectionTable || len(ep.RequestFields) == 0 {
				t.Fatalf("7.2 请求参数未被展开: state=%s fields=%d", ep.ReqParamsState, len(ep.RequestFields))
			}
			if ep.ReqParamsState == SectionCrossRef {
				t.Fatalf("7.2 仍是 cross_ref")
			}
		}
	}
}

// ===================== 白名单 1：bullet 形态归一 =====================

const repairBulletDoc = "# 用户服务接口变更文档\n\n" +
	"## 一、用户管理\n\n" +
	"### 1.1 新增订阅\n\n" +
	"- **URL:** `POST /api/user/subscribe`\n" +
	"- **Content-Type**: application/json\n" +
	"- **认证:** 无需登录\n" +
	"- **变更**: 新增\n" +
	"- **breaking：** false\n\n" +
	"#### 请求参数\n\n" +
	"| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| uid | Long | 是 | Body | 用户ID |\n\n" +
	"#### 返回参数\n\n" +
	"| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 是否成功 |\n"

func TestRepairBulletFormPositive(t *testing.T) {
	before := Parse(repairBulletDoc, Options{})
	if len(before.Endpoints) != 0 || !before.HasCode(ErrURLFormat) {
		t.Fatalf("前提不成立：方言 URL 行本应导致 0 接口 + E-URL-FORMAT，实际 %d 接口", len(before.Endpoints))
	}
	fixed, fixes, doc := Repair(repairBulletDoc)
	if len(fixes) != 5 {
		t.Fatalf("应归一 5 行方言 bullet，实际 %d: %+v", len(fixes), fixes)
	}
	for _, f := range fixes {
		if f.Kind != FixBulletForm || f.NeedsReview {
			t.Fatalf("bullet 归一类型/待核对标记不对: %+v", f)
		}
		if f.Line < 7 || f.Line > 11 {
			t.Fatalf("bullet 修复行号 %d 不在文档 bullet 区域（7–11 行）", f.Line)
		}
	}
	// 修复后 URL 行可识别、值全部入槽（形态归一只是把文档已写的值还给解析器，不新增语义）
	if len(doc.Endpoints) != 1 {
		t.Fatalf("修复后接口数 = %d, want 1", len(doc.Endpoints))
	}
	ep := doc.Endpoints[0]
	if ep.Method != "POST" || ep.IdentityKey != "POST|/api/user/subscribe" {
		t.Fatalf("URL 还原失败: %+v", ep)
	}
	if ep.ContentType != "application/json" || ep.Auth != AuthNoLogin || ep.Change != ChangeAdded {
		t.Fatalf("元信息值未入槽: ct=%q auth=%q change=%q", ep.ContentType, ep.Auth, ep.Change)
	}
	if ep.BreakingDecl != 0 {
		t.Fatalf("breaking=false 应解析为 0，实际 %d", ep.BreakingDecl)
	}
	if !strings.Contains(fixed, "- **URL**：`POST /api/user/subscribe`") {
		t.Fatalf("修复稿缺少规范形态:\n%s", fixed)
	}
	if again, f2, _ := Repair(fixed); again != fixed || len(f2) != 0 {
		t.Fatalf("bullet 归一不幂等")
	}
}

func TestRepairBulletFormNegatives(t *testing.T) {
	neg := "# 文档\n\n## 一、x\n\n### 1.1 a\n\n" +
		"- **URL**：`GET /a`\n" +
		"- **变更**: 这里把订阅改成了按年计费，原因见需求单。\n" + // 值不在枚举：疑似正文句子，绝不当元信息收
		"- **说明**：冒号：出现在正文里也不该被动。\n\n" + // 白名单外标签
		"```\n- **URL:** `POST /inside/fence`\n```\n\n" + // 围栏内不碰
		"#### 请求参数\n\n无。\n\n#### 返回参数\n\n无。\n"
	fixed, fixes, _ := Repair(neg)
	if len(fixes) != 0 {
		t.Fatalf("反例全不该修，实际: %+v", fixes)
	}
	if fixed != neg {
		t.Fatalf("反例文档必须字节不变:\n%s", fixed)
	}
}

// ===================== 白名单 2：未闭合围栏补闭合 =====================

// 真实形态：接口块正常解析，文末示例代码块忘记闭合（全文标记数为奇数）。
// 修复只在文末补一个闭合标记——解析产物（接口数）修复前后一致，
// 只是把 passFences 的「未闭合代码围栏」警告消掉、让修正稿本身成为合法 markdown。
func TestRepairFenceClose(t *testing.T) {
	const meta = "```apidoc-meta\nspec: 1\nproject: p\nscope: incremental\nchange_type: feature\nchange_title: t\nchange_note: n\n```\n\n"
	odd := meta + "# 文档\n\n## 一、x\n\n### 1.1 a\n\n甲。\n\n" +
		"- **URL**：`GET /api/a`\n- **变更**：新增\n\n" +
		"#### 返回示例\n\n```json\n{ \"ok\": true }\n"
	before := Parse(odd, Options{})
	if len(before.Endpoints) != 1 {
		t.Fatalf("前提：接口应先被解析出来: %d", len(before.Endpoints))
	}
	if !before.HasCode(WarnUnparsedBlock) {
		t.Fatalf("前提：未闭合围栏应报 W-UNPARSED-BLOCK")
	}
	fixed, fixes, doc := Repair(odd)
	if len(fixes) != 1 || fixes[0].Kind != FixFenceClose {
		t.Fatalf("奇数围栏应补一个闭合: %+v", fixes)
	}
	if fixes[0].NeedsReview {
		t.Fatalf("补闭合不产生任何解析语义变化，不该标待核对: %+v", fixes[0])
	}
	if !strings.HasSuffix(fixed, "```") {
		t.Fatalf("修复稿应以闭合标记结尾:\n%s", fixed)
	}
	if len(doc.Endpoints) != 1 {
		t.Fatalf("补闭合不该改变接口数: %d", len(doc.Endpoints))
	}
	if doc.HasCode(WarnUnparsedBlock) {
		t.Fatalf("补闭合后未闭合警告应消失")
	}
	if again, f2, _ := Repair(fixed); again != fixed || len(f2) != 0 {
		t.Fatalf("围栏补闭合不幂等")
	}
	// 偶数围栏：一个字符都不动
	even := "# 文档\n\n```json\n{}\n```\n"
	if fixed2, fixes2, _ := Repair(even); len(fixes2) != 0 || fixed2 != even {
		t.Fatalf("偶数围栏不该动: %d 条修复", len(fixes2))
	}
	// ~~~ 风格按开栏风格补闭合
	tilde := "# 文档\n\n~~~\ncode\n"
	fixed3, fixes3, _ := Repair(tilde)
	if len(fixes3) != 1 || !strings.HasSuffix(fixed3, "~~~") {
		t.Fatalf("~~~ 围栏应按原风格补闭合: %+v", fixes3)
	}
}

// ===================== 白名单 3：补「位置」列 =====================

const repairPosDoc = "```apidoc-meta\nspec: 1\nproject: p\nscope: incremental\nchange_type: feature\nchange_title: t\nchange_note: n\n```\n\n" +
	"# 文档\n\n## 一、x\n\n### 1.1 列表\n\n分页列表。\n\n" +
	"- **URL**：`GET /api/list`\n- **变更**：新增\n\n" +
	"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 说明 |\n|---|---|---|---|\n| page | Int | 否 | 页码 |\n| size | Int | 否 | 每页条数 |\n\n" +
	"#### 路径参数\n\n| 参数名 | 类型 | 必填 | 说明 |\n|---|---|---|---|\n| tenant | String | 是 | 租户 |\n\n" +
	"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| total | Int | 总数 |\n"

func TestRepairPosColumn(t *testing.T) {
	before := Parse(repairPosDoc, Options{})
	if !before.HasCode(ErrPosColMissing) {
		t.Fatalf("前提不成立：原文应触发 E-POS-COL-MISSING")
	}
	fixed, fixes, doc := Repair(repairPosDoc)
	var posFixes []Fix
	for _, f := range fixes {
		if f.Kind == FixPosColumn {
			posFixes = append(posFixes, f)
		}
	}
	if len(posFixes) != 2 {
		t.Fatalf("请求参数/路径参数各补一列，实际 %d: %+v", len(posFixes), fixes)
	}
	for _, f := range posFixes {
		if !f.NeedsReview {
			t.Fatalf("位置列推断必须逐条待核对: %+v", f)
		}
	}
	if doc.HasCode(ErrPosColMissing) {
		t.Fatalf("修复后仍报 E-POS-COL-MISSING")
	}
	ep := doc.Endpoints[0]
	if len(ep.RequestFields) != 2 || ep.RequestFields[0].Position != PosQuery || ep.RequestFields[1].Position != PosQuery {
		t.Fatalf("GET 应推断 Query: %+v", ep.RequestFields)
	}
	if len(ep.PathFields) != 1 || ep.PathFields[0].Position != PosPath {
		t.Fatalf("路径参数表应推断 Path: %+v", ep.PathFields)
	}
	// 其余单元格逐字节不变 + 新列插在「必填」之后
	if !strings.Contains(fixed, "| 参数名 | 类型 | 必填 | 位置 | 说明 |") {
		t.Fatalf("列位不合骨架:\n%s", fixed)
	}
	if !strings.Contains(fixed, "| page | Int | 否 | Query | 页码 |") ||
		!strings.Contains(fixed, "| tenant | String | 是 | Path | 租户 |") {
		t.Fatalf("数据行插值不对:\n%s", fixed)
	}
	// 返回参数表不需要位置列：不动
	if strings.Contains(fixed, "| 字段 | 类型 | 位置 |") {
		t.Fatalf("返回参数表被乱补列")
	}
	// 语义零改动：解析器在修复前本就按同一规则填了推断 Position
	assertSemanticsPreserved(t, Parse(repairPosDoc, Options{Mode: ModeInit}), Parse(fixed, Options{Mode: ModeInit}))
	if again, f2, _ := Repair(fixed); again != fixed || len(f2) != 0 {
		t.Fatalf("位置列补列不幂等")
	}
}

// ===================== 白名单 4：交叉引用确定性展开 =====================

const repairCrossHeader = "```apidoc-meta\nspec: 1\nproject: p\nscope: incremental\nchange_type: feature\nchange_title: t\nchange_note: n\n```\n\n" +
	"# 文档\n\n## 一、x\n\n" +
	"### 1.1 新增订阅\n\n新增。\n\n- **URL**：`POST /api/subscribe`\n- **变更**：新增\n\n" +
	"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| plan | String | 是 | Body | 套餐 |\n\n" +
	"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n\n"

func TestRepairCrossRefExpandByName(t *testing.T) {
	src := repairCrossHeader +
		"### 1.2 取消订阅\n\n取消。\n\n- **URL**：`POST /api/cancel`\n- **变更**：新增\n\n" +
		"#### 请求参数\n\n同新增订阅。\n\n" +
		"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
	before := Parse(src, Options{})
	if !before.HasCode(ErrCrossRef) {
		t.Fatalf("前提不成立：原文应触发 E-CROSS-REF")
	}
	fixed, fixes, doc := Repair(src)
	if len(fixes) != 1 || fixes[0].Kind != FixCrossRefExpand {
		t.Fatalf("应恰好一条搬运修复: %+v", fixes)
	}
	if fixes[0].NeedsReview {
		t.Fatalf("纯搬运（源表无推断列）不该标待核对: %+v", fixes[0])
	}
	if doc.HasCode(ErrCrossRef) {
		t.Fatalf("修复后仍报 E-CROSS-REF")
	}
	cancel := doc.Endpoints[1]
	if cancel.ReqParamsState != SectionTable || len(cancel.RequestFields) != 1 {
		t.Fatalf("1.2 请求参数应展开为一行: state=%s %+v", cancel.ReqParamsState, cancel.RequestFields)
	}
	f := cancel.RequestFields[0]
	if f.Name != "plan" || f.TypeRaw != "String" || f.Required != RequiredYes || f.Position != PosBody || f.Desc != "套餐" {
		t.Fatalf("搬运字段与源表不一致: %+v", f)
	}
	// 原样搬运：表头/数据行字符串在修复稿中出现两次（1.1 的原件 + 1.2 的搬来件）
	if strings.Count(fixed, "| plan | String | 是 | Body | 套餐 |") != 2 {
		t.Fatalf("搬运不是原样:\n%s", fixed)
	}
	// 引用行本身保留（保真，不删用户的字）
	if !strings.Contains(fixed, "同新增订阅。") {
		t.Fatalf("引用式散文被删掉了")
	}
	if again, f2, _ := Repair(fixed); again != fixed || len(f2) != 0 {
		t.Fatalf("交叉引用展开不幂等")
	}
	// 语义零改动：1.1 一侧原封不动（1.2 只是新增了搬运字段）
}

func TestRepairCrossRefExpandBySeq(t *testing.T) {
	src := repairCrossHeader +
		"### 1.2 批量订阅\n\n批量。\n\n- **URL**：`POST /api/batch`\n- **变更**：新增\n\n" +
		"#### 请求参数\n\n请求体为 `List<SubscribeDTO>`（JSON 数组，元素结构见 1.1）。\n\n" +
		"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
	fixed, fixes, doc := Repair(src)
	if len(fixes) != 1 || fixes[0].Kind != FixCrossRefExpand {
		t.Fatalf("编号引用应展开: %+v", fixes)
	}
	if doc.HasCode(ErrCrossRef) {
		t.Fatalf("修复后仍报 E-CROSS-REF:\n%s", fixed)
	}
	batch := doc.Endpoints[1]
	if batch.ReqParamsState != SectionTable || len(batch.RequestFields) != 1 || batch.RequestFields[0].Name != "plan" {
		t.Fatalf("1.2 未按编号搬运 1.1 的表: %+v", batch.RequestFields)
	}
}

func TestRepairCrossRefRefuses(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"被引用对象缺失", repairCrossHeader +
			"### 1.2 取消订阅\n\n- **URL**：`POST /api/cancel`\n- **变更**：新增\n\n" +
			"#### 请求参数\n\n同新增收藏。\n\n"},
		{"多个候选",
			"```apidoc-meta\nspec: 1\nproject: p\nscope: incremental\nchange_type: feature\nchange_title: t\nchange_note: n\n```\n\n" +
				"# 文档\n\n## 一、x\n\n" +
				"### 1.1 新增订阅\n\n- **URL**：`POST /api/a`\n- **变更**：新增\n\n" +
				"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| plan | String | 是 | Body | 套餐 |\n\n" +
				"### 1.2 新增订阅\n\n- **URL**：`POST /api/b`\n- **变更**：新增\n\n" +
				"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| other | String | 是 | Body | 别的 |\n\n" +
				"### 1.3 取消订阅\n\n- **URL**：`POST /api/c`\n- **变更**：新增\n\n" +
				"#### 请求参数\n\n同新增订阅。\n\n"},
		{"编号引用目标不存在", repairCrossHeader +
			"### 1.2 取消订阅\n\n- **URL**：`POST /api/cancel`\n- **变更**：新增\n\n" +
			"#### 请求参数\n\n元素结构见 9.9。\n\n"},
		{"自引用", repairCrossHeader +
			"### 1.2 取消订阅\n\n- **URL**：`POST /api/cancel`\n- **变更**：新增\n\n" +
			"#### 请求参数\n\n同 1.2。\n\n"},
		{"被引用接口该槽位没有表", repairCrossHeader +
			"### 1.2 新增无表\n\n- **URL**：`POST /api/empty`\n- **变更**：新增\n\n" +
			"#### 请求参数\n\n无。\n\n" +
			"### 1.3 取消订阅\n\n- **URL**：`POST /api/cancel`\n- **变更**：新增\n\n" +
			"#### 请求参数\n\n同新增无表。\n\n"},
	}
	for _, c := range cases {
		fixed, fixes, doc := Repair(c.src)
		// AC5/D2：不确定就不改——保留原 E-CROSS-REF，一个字都不动，绝不编造字段表
		if len(fixes) != 0 {
			t.Fatalf("%s: 不该产生任何修复: %+v", c.name, fixes)
		}
		if fixed != c.src {
			t.Fatalf("%s: 文本必须字节不变:\n%s", c.name, fixed)
		}
		if !doc.HasCode(ErrCrossRef) {
			t.Fatalf("%s: 原 E-CROSS-REF 必须保留（只报不改）", c.name)
		}
	}
}

// ===================== AC4 兜底：空文本 / 纯垃圾 =====================

func TestRepairEmptyAndGarbage(t *testing.T) {
	if fixed, fixes, _ := Repair(""); fixed != "" || len(fixes) != 0 {
		t.Fatalf("空输入应原样返回: %q %v", fixed, fixes)
	}
	junk := "完全不是接口文档\n就是一段话\n再来一段\n"
	if fixed, fixes, _ := Repair(junk); fixed != junk || len(fixes) != 0 {
		t.Fatalf("垃圾输入不该乱修: %q %v", fixed, fixes)
	}
	onlyFence := "```python\nprint('hi')\n"
	fixed, fixes, _ := Repair(onlyFence)
	if len(fixes) != 1 || fixes[0].Kind != FixFenceClose || !strings.HasSuffix(fixed, "```") {
		t.Fatalf("只有围栏的输入应只补闭合: %+v", fixes)
	}
}

// ===================== 「解析器已容忍就不修」的核查落点（DoD）=====================

// H4 括号后缀（白名单 5）：parse.go sectionKind 是前缀匹配，本就能识别 → 修复器零 diff。
func TestRepairSkipsToleratedH4Suffix(t *testing.T) {
	src := "```apidoc-meta\nspec: 1\nproject: p\nscope: incremental\nchange_type: feature\nchange_title: t\nchange_note: n\n```\n\n" +
		"# 文档\n\n## 一、x\n\n### 1.1 保存\n\n保存。\n\n" +
		"- **URL**：`POST /api/save`\n- **变更**：新增\n\n" +
		"#### 请求参数（SaveDTO）\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| a | String | 是 | Body | 甲 |\n\n" +
		"#### 返回参数（ApiResponse\\<Boolean\\>）\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
	fixed, fixes, _ := Repair(src)
	if len(fixes) != 0 || fixed != src {
		t.Fatalf("H4 括号后缀已被解析器容忍，不该有任何修复: %+v", fixes)
	}
	doc := Parse(src, Options{})
	if len(doc.Endpoints) != 1 || len(doc.Endpoints[0].RequestFields) != 1 {
		t.Fatalf("前提不成立：前缀匹配本应识别带后缀小节: %+v", doc.Issues)
	}
}

// 章节序号 / --- 分隔线噪声（白名单 6）：解析器已逐类跳过，清理不改变任何解析产物
// → 不实现（改了只会制造无意义 diff）。这里锁死「不修」的行为契约。
func TestRepairSkipsToleratedSeparatorNoise(t *testing.T) {
	src := "# 文档\n\n## 一、x\n\n---\n\n### 1.1 保存\n\n保存。\n\n" +
		"- **URL**：`GET /api/a`\n\n" +
		"#### 请求参数\n\n无。\n\n---\n\n#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
	fixed, fixes, _ := Repair(src)
	if len(fixes) != 0 || fixed != src {
		t.Fatalf("--- 噪声解析器已容忍，不该有任何修复: %+v", fixes)
	}
}
