package appsvc

// 聊天附件「体检 → 确定性修复 → 重新校验 → 导入」编排测试（任务 10-20-chat-file-import）。
// 仓库约定：newTestService 真 SQLite（t.TempDir）+ 手写 t.Fatalf，无断言库。
//
// AC 对照（本文件覆盖的部分）：
//   AC1 → TestAutoFixPreviewListsFixes         AC2 → TestAutoFixResidualErrorsZeroWrite
//   AC3 → TestAutoFixInitTristate              AC4 → TestAutoFixEmptyAndGarbage
//   AC5 → TestAutoFixCrossRefMissingTarget     AC6 → TestAutoFixFixedTextIsTheRepairedDoc
//   AC7 → TestAutoFixStoredTextIsRepairedDoc
//   另有：AutoFix 关闭时行为不变（界面向导零回归）、Preview/Commit 修复结果一致（确定性）。

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"DevCraft/internal/apidoc"
	skillapidoc "DevCraft/internal/skill/apidoc"
)

// apiInitMetaBlock init 档位 meta 块（无 change_* 字段：init 不要求，R10 不生成事件）。
func apiInitMetaBlock(project string) string {
	return "```apidoc-meta\nspec: 1\nproject: " + project + "\nservice: 演示服务\nbase_path: /api/v1/demo\nscope: init\n```\n\n"
}

// demoAutoFixDoc 附件通道用例文档：3 处 bullet 方言（含唯一硬规则 URL 行的半角冒号）+
// 1 张缺「位置」列的请求参数表。不修复则 0 接口入库（E-URL-FORMAT），修复后规范全绿。
func demoAutoFixDoc() string {
	return "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "feature", "格式修复演示", "验证附件通道确定性修复后正常入库", "REQ-FIX") +
		"## 一、用户管理\n\n" +
		"### 2.1 查询订阅\n\n查询当前用户订阅。\n\n" +
		"- **URL:** `GET /api/v1/demo/subscription`\n" +
		"- **认证**: 需要登录\n" +
		"- **变更**: 新增\n\n" +
		"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 说明 |\n|---|---|---|---|\n| status | String | 否 | 状态 |\n\n" +
		"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
}

// demoUnfixableDoc 修复白名单之外的问题：缺 **变更** 行（D2 明确「只报不改」——
// 增量模式无法推断是新增还是修改）+ 说明文字里的中文冒号句（绝不允许被当元信息归一）。
func demoUnfixableDoc() string {
	return "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "feature", "不可修复演示", "验证修完仍有 E-* 时整批拒收", "REQ-BAD") +
		"## 一、用户管理\n\n" +
		"### 3.1 查询账单\n\n查询账单。\n\n" +
		"- **URL**：`GET /api/v1/demo/bill`\n\n" +
		"账单口径说明：**注意**: 这里的冒号句是正文，不是元信息行。\n\n" +
		"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| month | String | 是 | Query | 月份 |\n\n" +
		"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
}

// demoCrossRefMissingDoc 交叉引用但被引用对象不存在 → 修复器一个字都不改，E-CROSS-REF 保留。
func demoCrossRefMissingDoc() string {
	return "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "feature", "引用缺失演示", "验证引用对象缺失时只报不改", "REQ-XREF") +
		"## 一、用户管理\n\n" +
		"### 4.1 取消订阅\n\n取消。\n\n" +
		"- **URL**：`POST /api/v1/demo/cancel`\n- **变更**：新增\n\n" +
		"#### 请求参数\n\n同新增订阅。\n\n" +
		"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
}

// sha256Hex 测试侧独立复算（AC7 比对用）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ===================== AC1：修复清单逐条可见（行号 + 待核对标记）=====================

func TestAutoFixPreviewListsFixes(t *testing.T) {
	svc := newTestService(t)

	// 不勾选 AutoFix：界面向导行为零回归——问题原样报出，fixes 为空数组。
	plain, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: demoAutoFixDoc()})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if len(plain.Fixes) != 0 {
		t.Fatalf("未启用 AutoFix 不该有修复清单: %+v", plain.Fixes)
	}
	if plain.OK || !plainHasCode(plain, apidoc.ErrURLFormat) {
		t.Fatalf("未启用 AutoFix 时方言 URL 行必须照常拒收: ok=%v", plain.OK)
	}

	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: demoAutoFixDoc(), AutoFix: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	// 3 处 bullet 归一 + 1 处补位置列
	if len(pv.Fixes) != 4 {
		t.Fatalf("修复清单应恰好 4 条，实际 %d: %+v", len(pv.Fixes), pv.Fixes)
	}
	var posFixes, bulletFixes int
	for _, f := range pv.Fixes {
		if f.Line < 1 {
			t.Fatalf("修复条目缺少行号: %+v", f)
		}
		if strings.TrimSpace(f.Before) == "" || strings.TrimSpace(f.After) == "" || strings.TrimSpace(f.Note) == "" {
			t.Fatalf("修复条目必须写清改了什么/为什么: %+v", f)
		}
		switch f.Kind {
		case apidoc.FixBulletForm:
			bulletFixes++
			if f.NeedsReview {
				t.Fatalf("形态归一是确定性改动，不该标待核对: %+v", f)
			}
		case apidoc.FixPosColumn:
			posFixes++
			if !f.NeedsReview {
				t.Fatalf("推断的位置列必须标待人工核对: %+v", f)
			}
		default:
			t.Fatalf("白名单外的修复类型: %+v", f)
		}
	}
	if bulletFixes != 3 || posFixes != 1 {
		t.Fatalf("修复分类计数不对: bullet=%d pos=%d", bulletFixes, posFixes)
	}
	// 修复后重新完整校验通过 → 可进入提交；剩余清单里没有 E-*
	if !pv.OK {
		t.Fatalf("全部问题在白名单内可修，预览应放行: issues=%+v", pv.Issues)
	}
	for _, is := range pv.Issues {
		if is.Level == apidoc.LevelError {
			t.Fatalf("修复后不应残留 E-*: %+v", is)
		}
	}
	if pv.Stats.Added != 1 {
		t.Fatalf("修复后接口应正常进入变更分栏: %+v", pv.Stats)
	}

	// 确定性：预览不写库（只读），提交复用同一 Repair → FixCount 与预览逐条一致
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAutoFixDoc(), AutoFix: true, Confirmed: true})
	if res.FixCount != len(pv.Fixes) {
		t.Fatalf("提交应用修复数 %d 与预览 %d 不一致（确定性被破坏）", res.FixCount, len(pv.Fixes))
	}
	if res.EventID == 0 || res.Seq != 1 {
		t.Fatalf("修完无 E-* 应正常 preview→commit 并生成事件: %+v", res)
	}
}

func plainHasCode(pv ApiDocImportPreview, code string) bool {
	for _, is := range pv.Issues {
		if is.Code == code {
			return true
		}
	}
	return false
}

// ===================== AC2：修完仍有 E-* → 不入库、零残留 =====================

func TestAutoFixResidualErrorsZeroWrite(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: demoUnfixableDoc(), AutoFix: true, Confirmed: true})
	if err == nil {
		t.Fatalf("缺 **变更** 行属白名单外问题，修复后必须整批拒收")
	}
	if !strings.Contains(err.Error(), apidoc.ErrChangeMissing) {
		t.Fatalf("拒收理由应带剩余错误码 %s，实际: %v", apidoc.ErrChangeMissing, err)
	}
	if strings.Contains(err.Error(), "**注意**") {
		t.Fatalf("正文冒号句不该被当成修复对象甚至报错源头: %v", err)
	}
	for table, n := range apiRawTableCounts(t) {
		if n != 0 {
			t.Fatalf("拒收后 %s 残留 %d 行", table, n)
		}
	}
	// 预览侧同样可见：修复清单 + 剩余问题并存（不能静默改完就导，也不能吞掉剩余问题）
	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: demoUnfixableDoc(), AutoFix: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if pv.OK {
		t.Fatalf("剩余 E-* 时预览不得放行")
	}
	if !plainHasCode(pv, apidoc.ErrChangeMissing) {
		t.Fatalf("预览应报出剩余 E-CHANGE-MISSING: %+v", pv.Issues)
	}
}

// ===================== AC3：init 附件三态（D6 放宽一处，§13.12 闸门不动）=====================

func TestAutoFixInitTristate(t *testing.T) {
	svc := newTestService(t)

	// ① 未勾选「基线导入」而文档声明 scope: init → 拒绝（附件通道同意位 = InitMode）
	_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: demoInitAttachDoc(), AutoFix: true})
	if err == nil || !strings.Contains(err.Error(), "基线导入") {
		t.Fatalf("未勾选的基线导入必须被拒并指路，实际: %v", err)
	}
	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: demoInitAttachDoc(), AutoFix: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if pv.OK || !strings.Contains(pv.Message, "基线导入") {
		t.Fatalf("预览同样要拦住未勾选的基线导入: ok=%v msg=%q", pv.OK, pv.Message)
	}
	for table, n := range apiRawTableCounts(t) {
		if n != 0 {
			t.Fatalf("未勾选被拒后 %s 残留 %d 行", table, n)
		}
	}

	// ② 勾选且项目为空 → 成功灌入且不生成事件（R10）
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoInitAttachDoc(), AutoFix: true, InitMode: true})
	if res.EventID != 0 || res.Seq != 0 {
		t.Fatalf("init 导入不得生成事件: %+v", res)
	}
	if res.Stats.Added == 0 || !strings.Contains(res.Message, "初始化导入完成") {
		t.Fatalf("空项目勾选后应灌入成功: %+v", res)
	}
	events, err := svc.store.ListApiChangeEvents(res.ProjectID, 0)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("init 导入后事件表必须有 %d 行，实际 %d 行", 0, len(events))
	}

	// ③ 勾选但项目非空 → §13.12 后端闸门拒收（附件通道 AllowInitOnNonEmpty 恒 false）
	_, err = svc.CommitApiDocImport(ApiDocImportRequest{DocText: demoInitAttachDoc(), AutoFix: true, InitMode: true})
	if err == nil || !strings.Contains(err.Error(), "初始化导入仅允许在空项目执行") {
		t.Fatalf("非空项目的 init 必须被 §13.12 闸门拒收（该闸门一字未改），实际: %v", err)
	}
}

// demoInitAttachDoc init 档位 + 方言 bullet（同时证明修复器在 init 通道照常工作）。
func demoInitAttachDoc() string {
	return "# 演示服务接口变更文档\n\n" + apiInitMetaBlock("demo-svc") +
		"## 一、用户管理\n\n" +
		"### 5.1 查询订阅\n\n查询。\n\n" +
		"- **URL:** `GET /api/v1/demo/init-sub`\n" +
		"- **变更**: 未变更\n\n" +
		"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 说明 |\n|---|---|---|---|\n| q | String | 否 | 关键词 |\n\n" +
		"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
}

// ===================== AC4：空文本 / 无 meta / 纯垃圾 → 明确中文错误，不崩不猜 =====================

func TestAutoFixEmptyAndGarbage(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: "   ", AutoFix: true}); err == nil ||
		!strings.Contains(err.Error(), "文档内容不能为空") {
		t.Fatalf("空文本必须明确拒收: %v", err)
	}
	if _, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: "", AutoFix: true}); err == nil ||
		!strings.Contains(err.Error(), "文档内容不能为空") {
		t.Fatalf("提交空文本必须明确拒收: %v", err)
	}
	junk := "今天天气不错\n适合重构代码\n```\n没关掉的围栏\n"
	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: junk, AutoFix: true})
	if err != nil {
		t.Fatalf("垃圾内容不该抛基础设施错误: %v", err)
	}
	if pv.OK {
		t.Fatalf("垃圾内容不得放行")
	}
	if !plainHasCode(pv, apidoc.ErrSpecBlockMissing) {
		t.Fatalf("垃圾内容要报出缺 meta 块（不猜意图）: %+v", pv.Issues)
	}
	if len(pv.Fixes) != 1 || pv.Fixes[0].Kind != apidoc.FixFenceClose {
		t.Fatalf("垃圾内容只允许围栏补闭合这一类白名单修复: %+v", pv.Fixes)
	}
	if _, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: junk, AutoFix: true}); err == nil {
		t.Fatalf("垃圾内容提交必须被拒")
	}
	for table, n := range apiRawTableCounts(t) {
		if n != 0 {
			t.Fatalf("垃圾内容被拒后 %s 残留 %d 行", table, n)
		}
	}
}

// ===================== AC5：被引用对象缺失 → 一个字不改、只报 =====================

func TestAutoFixCrossRefMissingTarget(t *testing.T) {
	svc := newTestService(t)
	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: demoCrossRefMissingDoc(), AutoFix: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	for _, f := range pv.Fixes {
		if f.Kind == apidoc.FixCrossRefExpand {
			t.Fatalf("被引用对象缺失时绝不允许编造展开: %+v", f)
		}
	}
	if !plainHasCode(pv, apidoc.ErrCrossRef) {
		t.Fatalf("原 E-CROSS-REF 必须保留上报: %+v", pv.Issues)
	}
	if pv.OK {
		t.Fatalf("残留 E-CROSS-REF 不得放行")
	}
	if _, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: demoCrossRefMissingDoc(), AutoFix: true}); err == nil {
		t.Fatalf("提交同样必须拒收")
	}
	// 展开成功的一侧（被引用对象存在）在 apidoc 包 TestRepairCrossRefExpandByName 已锁行为；
	// 这里再证编排层把「能修的修掉、不能修的照报」合并进同一份预览。
	okDoc := demoCrossRefMissingDoc() +
		"### 4.2 新增订阅\n\n新增。\n\n- **URL**：`POST /api/v1/demo/subscribe`\n- **变更**：新增\n\n" +
		"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| plan | String | 是 | Body | 套餐 |\n\n" +
		"#### 返回参数\n\n| 字段 | 类型 | 说明 |\n|---|---|---|\n| ok | boolean | 成功 |\n"
	pv2, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: okDoc, AutoFix: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	var expanded int
	for _, f := range pv2.Fixes {
		if f.Kind == apidoc.FixCrossRefExpand {
			expanded++
		}
	}
	if expanded != 1 || plainHasCode(pv2, apidoc.ErrCrossRef) {
		t.Fatalf("被引用对象存在时应恰好搬运一次并消除 E-CROSS-REF: fixes=%+v issues=%+v", pv2.Fixes, pv2.Issues)
	}
}

// ===================== AC7：入库文本 = 文件原文/修复稿本身（sha256 比对）=====================

func TestAutoFixStoredTextIsRepairedDoc(t *testing.T) {
	svc := newTestService(t)
	fixed, fixes, _ := apidoc.Repair(demoAutoFixDoc()) // 测试侧独立复现修复稿（同一个确定性函数）
	want := sha256Hex(fixed)

	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAutoFixDoc(), AutoFix: true, Confirmed: true})
	if len(fixes) == 0 || res.FixCount != len(fixes) {
		t.Fatalf("用例前提不成立: fixes=%d res.FixCount=%d", len(fixes), res.FixCount)
	}
	// 批次行：doc_fingerprint = sha256(修复稿)
	batches, err := svc.store.ListApiImportBatches("demo-svc|/api/v1/demo|演示服务接口变更文档", 0)
	if err != nil || len(batches) != 1 {
		t.Fatalf("批次应恰好 1 行: %d err=%v", len(batches), err)
	}
	if batches[0].DocFingerprint != want {
		t.Fatalf("批次指纹不是修复稿的 sha256: %s vs %s", batches[0].DocFingerprint, want)
	}
	// 事件行：meta.docSha256 同值；doc_text 逐字节 = 修复稿（不存在第三份"AI 改写版"）
	events, err := svc.store.ListApiChangeEvents(res.ProjectID, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("事件应恰好 1 行: %d err=%v", len(events), err)
	}
	if got, _ := events[0].Meta["docSha256"].(string); got != want {
		t.Fatalf("事件 meta.docSha256 不对: %q", got)
	}
	if events[0].Meta["autoFix"] != true {
		t.Fatalf("事件 meta 应记录 autoFix 标记: %+v", events[0].Meta)
	}
	if stored, err := svc.store.GetApiChangeEventDocText(events[0].ID); err != nil || stored != fixed {
		t.Fatalf("事件留档全文必须逐字节等于修复稿: err=%v", err)
	}
	// 原始文件字节绝不会被改写进库（入库的是修复稿；原文仍在用户手里，D7）
	if bad := sha256Hex(demoAutoFixDoc()); bad == want {
		t.Fatalf("用例失真：修复稿必须与原文不同 sha")
	}
}

// ===================== 技能通道闸门回归：§13.18 不放行 init、不做 AutoFix =====================

func TestSkillChannelStillRejectsInitAndNeverAutoFixes(t *testing.T) {
	// apiDocSkillRequest 是两个 init 键与 AutoFix 的唯一拼装点：逐键核对（编译不过即回归）。
	req := apiDocSkillRequest(skillapidoc.ImportInput{DocText: demoAutoFixDoc()}, true)
	if req.InitMode || req.AllowInitOnNonEmpty || req.AutoFix {
		t.Fatalf("模型驱动通道必须三个键全 false（§13.18 不放宽）: %+v", req)
	}
}

// ===================== AC6/D7：修正稿全文随预览与结果卡带回前端 =====================

// 前端「下载修正后的 md」只能拿后端算好的修复稿，绝不允许在前端再实现一遍修复器。
// 零指针约定：未启用 AutoFix 的通道（界面向导 / 模型驱动技能）fixedText 恒为空串。
func TestAutoFixFixedTextIsTheRepairedDoc(t *testing.T) {
	svc := newTestService(t)
	original := demoAutoFixDoc()
	fixed, fixes, _ := apidoc.Repair(original) // 测试侧独立复现（同一个确定性函数 → 必然同字节）
	if len(fixes) == 0 || fixed == original {
		t.Fatalf("用例前提不成立：该文档必须真的会被修出东西")
	}

	pv := previewApiDoc(t, svc, ApiDocImportRequest{DocText: original, AutoFix: true})
	if pv.FixedText != fixed {
		t.Fatalf("预览的 fixedText 必须逐字节等于修复稿（下载的就是它）")
	}
	if pv.FixedText == original {
		t.Fatalf("fixedText 不该是原文——那等于让前端下到未修复的版本")
	}
	if sha256Hex(pv.FixedText) != sha256Hex(fixed) {
		t.Fatalf("fixedText 的 sha 与修复稿不一致")
	}

	// 提交结果同样带回实际入库的那份文本（= 修复稿本身，与 AC7 的指纹同源）
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: original, AutoFix: true, Confirmed: true})
	if res.FixedText != fixed {
		t.Fatalf("结果卡的 fixedText 必须等于真正入库的修复稿")
	}
	if res.FixedText != pv.FixedText {
		t.Fatalf("预览与提交给出的修正稿不一致（确定性被破坏）")
	}
	// 入库正文逐字节 = fixedText（下载件与库内件同源，不存在第三份文本）
	events, err := svc.store.ListApiChangeEvents(res.ProjectID, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("事件应恰好 1 行: %d err=%v", len(events), err)
	}
	if stored, err := svc.store.GetApiChangeEventDocText(events[0].ID); err != nil || stored != res.FixedText {
		t.Fatalf("事件留档全文与 fixedText 不一致: err=%v", err)
	}

	// 未启用 AutoFix 的通道：空串（不是 null、不是原文）——界面向导没有「修正稿」这个概念
	wizard, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: original})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if wizard.FixedText != "" || len(wizard.Fixes) != 0 {
		t.Fatalf("界面向导通道不该带修正稿与修复清单: %q / %d 条", wizard.FixedText, len(wizard.Fixes))
	}
}

// previewApiDoc 预览的测试快捷方式（失败即 t.Fatalf）。
func previewApiDoc(t *testing.T, svc *Service, req ApiDocImportRequest) ApiDocImportPreview {
	t.Helper()
	pv, err := svc.PreviewApiDocImport(req)
	if err != nil {
		t.Fatalf("PreviewApiDocImport: %v", err)
	}
	return pv
}
