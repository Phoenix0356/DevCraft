package appsvc

// 接口文档 chat 技能的集成测试：真 SQLite + 真解析器 + 真注册表，
// 通过注册表拿到技能并 Execute（与 agent 循环的 dispatch 同一条路）。
//
// 钉住的行为（对应本轮验收）：
//   ① 规范文档经技能路径导入成功并生成事件 → TestApiDocSkillImportWritesAndEmitsEvent
//   ② 含删除的文档走审批闸：未批准时 9 张表零残留，批准后写入 → TestApiDocSkillApprovalGate
//   ③ E-* 文档被拒且零残留 → TestApiDocSkillRejectionZeroResidue
//   ④ 技能路径无法触发 init（scope: init 被拒收，哪怕服务层闸门会放行）
//      → TestApiDocSkillCannotTriggerInit
//   ⑤ 正文衍生物经 sanitize 后才入 LLM → TestApiDocSkillMasksSecretsIntoLLM
//   ⑥ AC9 同源：技能交付的规范文本与 ExportApiDocSpec / apidoc.SpecText 逐字节相等
//      → TestApiDocSkillSpecSameSourceAsUIExport
//   ⑦ check 与 import 复用同一套 appsvc 方法（不是第二份解析实现）
//      → TestApiDocSkillCheckIsPreviewApiDocImportProjection
//   ⑧ 补装迁移幂等 → TestMigrateApiDocSkills
//
// 约定与 apidoc_test.go / deploy_test.go 一致：t.TempDir() 真 SQLite、手写断言、无断言库。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"DevCraft/internal/agent"
	"DevCraft/internal/apidoc"
	skillapidoc "DevCraft/internal/skill/apidoc"
	"DevCraft/internal/skill/deploy"
	"DevCraft/internal/store"
)

// attachApiDocSkills 把真技能装进测试服务的注册表（app.go buildService 的同款接线）。
func attachApiDocSkills(t *testing.T, svc *Service) {
	t.Helper()
	if err := skillapidoc.Register(svc.skills, svc); err != nil {
		t.Fatalf("注册接口文档技能: %v", err)
	}
}

// captureFrames 接管 svc.emit，记录所有帧（原闭包继续被调用，doneErrsHook 不受影响）。
func captureFrames(t *testing.T, svc *Service) func() []capturedEvent {
	t.Helper()
	var mu sync.Mutex
	var got []capturedEvent
	inner := svc.emit
	svc.emit = func(event string, payload map[string]any) {
		mu.Lock()
		got = append(got, capturedEvent{event: event, payload: payload})
		mu.Unlock()
		inner(event, payload)
	}
	t.Cleanup(func() { svc.emit = inner })
	return func() []capturedEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedEvent(nil), got...)
	}
}

// runApiDocSkill 按名字执行技能（args 用 map 传，路径自动转 JSON）。
func runApiDocSkill(t *testing.T, svc *Service, ctx context.Context, name string, args map[string]any) (string, error) {
	t.Helper()
	sk, ok := svc.skills.Get(name)
	if !ok {
		t.Fatalf("技能未注册: %s", name)
	}
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return sk.Execute(ctx, data)
}

// writeDocFile 把文档写到临时目录，返回路径（技能靠路径自己读盘）。
func writeDocFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api-change.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	return path
}

// waitForApiDocDone 轮询 api:done 帧（批准后异步执行的收尾信号，形状照 waitForDeployDone）。
func waitForApiDocDone(t *testing.T, get func() []capturedEvent) capturedEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range get() {
			if ev.event == EventApiDocDone {
				return ev
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timeout waiting for api:done frame")
	return capturedEvent{}
}

// onlyApiDocApproval 取出当前唯一一张接口文档审批单 id（注册表是内存态，测试直接读）。
func onlyApiDocApproval(t *testing.T, svc *Service) string {
	t.Helper()
	svc.apiApprovalsMu.Lock()
	defer svc.apiApprovalsMu.Unlock()
	if len(svc.apiApprovals) != 1 {
		t.Fatalf("审批单数量 = %d, want 1", len(svc.apiApprovals))
	}
	for id := range svc.apiApprovals {
		return id
	}
	return ""
}

// skillSessionCtx 带会话 id 的 ctx（技能要靠它把审批单绑到当前会话，同 deploy 的形状）。
func skillSessionCtx() context.Context {
	return deploy.WithSessionID(context.Background(), "sess-apidoc")
}

// ===================== ① 导入闭环 =====================

func TestApiDocSkillImportWritesAndEmitsEvent(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	get := captureFrames(t, svc)
	path := writeDocFile(t, demoBaselineDoc())

	out, err := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillImport, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("技能导入失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "已生成变更事件 v1") || !strings.Contains(out, "新增 2") {
		t.Fatalf("技能回执未如实报告事件与统计: %s", out)
	}
	// 库里的真实结果与界面导入完全同一套方法算出来的：条目 2、事件 1、批次 1
	proj, found, err := svc.store.FindApiProjectByName("demo-svc")
	if err != nil || !found {
		t.Fatalf("项目未创建: found=%v err=%v", found, err)
	}
	if n := len(apiAllEntries(t, svc, proj.ID)); n != 2 {
		t.Fatalf("条目数 = %d, want 2", n)
	}
	if n := apiEventCount(t, svc, proj.ID); n != 1 {
		t.Fatalf("事件数 = %d, want 1", n)
	}
	if counts := apiRawTableCounts(t); counts["api_import_batches"] != 1 {
		t.Fatalf("导入批次应落 1 行: %+v", counts)
	}
	// 纯新增不该推审批帧（直发路径）
	for _, ev := range get() {
		if ev.event == EventApiDocApproval {
			t.Fatalf("纯新增不应产生审批帧")
		}
	}
}

// ===================== ② 删除 → 审批闸 =====================

func TestApiDocSkillApprovalGate(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	get := captureFrames(t, svc)
	projectID := seedDemoBaseline(t, svc)

	deleteDoc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("demo-svc", "refactor", "下线删除用户", "合规要求下线物理删除", "REQ-20") +
		"## 一、用户管理\n\n" +
		"### 1.2 删除用户\n\n按 id 删除用户。\n\n" +
		"- **URL**：`DELETE /api/v1/demo/user/{id}`\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：删除\n" +
		"- **变更点**：改走软删接口\n"
	path := writeDocFile(t, deleteDoc)
	before := apiRawTableCounts(t)

	out, err := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillImport, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("含删除的导入应成功建单而不是报错: %v\n%s", err, out)
	}
	if !strings.Contains(out, "已生成接口文档导入审批单") || !strings.Contains(out, "不要声称导入已经完成") {
		t.Fatalf("回执应讲清等待批准: %s", out)
	}
	// 未批准：9 张表逐一零残留，条目仍是 active，事件仍是 v1
	assertCountsUnchanged(t, svc, projectID, 2, 1)
	assertApiTablesUnchanged(t, before)
	if e := mustEntryByIdentity(t, svc, projectID, "DELETE|/api/v1/demo/user/{}"); e.Status != store.ApiEntryStatusActive {
		t.Fatalf("未批准不得软删: %s", e.Status)
	}
	// 审批卡片帧必须真的推给前端（载荷带审批单 id 与变更摘要，不含正文）
	approvalID := onlyApiDocApproval(t, svc)
	events := get()
	var frame *capturedEvent
	for i := range events {
		if events[i].event != EventApiDocApproval {
			continue
		}
		frame = &events[i]
		if _, leaked := events[i].payload["docText"]; leaked {
			t.Fatal("审批帧不得携带文档正文")
		}
		if events[i].payload["approvalId"] != approvalID {
			t.Fatalf("帧里的 approvalId 与注册表不一致: %v / %s", events[i].payload["approvalId"], approvalID)
		}
		if events[i].payload["sessionId"] != "sess-apidoc" {
			t.Fatalf("审批帧必须绑到当前会话: %v", events[i].payload["sessionId"])
		}
	}
	if frame == nil {
		t.Fatal("未收到 api:approval 帧")
	}

	// 批准 → 异步执行 → api:done success → 条目软删 + 事件 v2
	if err := svc.ApproveApiDocImport(approvalID); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	done := waitForApiDocDone(t, get)
	if done.payload["status"] != "success" {
		t.Fatalf("终态帧 status = %v, want success（summary=%v）", done.payload["status"], done.payload["summary"])
	}
	if e := mustEntryByIdentity(t, svc, projectID, "DELETE|/api/v1/demo/user/{}"); e.Status != store.ApiEntryStatusRemoved {
		t.Fatalf("批准后应软删: %s", e.Status)
	}
	if n := apiEventCount(t, svc, projectID); n != 2 {
		t.Fatalf("批准后事件数 = %d, want 2", n)
	}
	// 结果作为 assistant 消息进会话（重开会话能看到，模型也能引用）
	msgs, err := svc.store.Messages("sess-apidoc", 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content, "接口文档导入完成") {
		t.Fatalf("导入结果应进会话: %+v", msgs)
	}
	// 单次消费门禁（照 takeApproval）：同一张单子不能再批准/拒绝
	if err := svc.ApproveApiDocImport(approvalID); err == nil || !strings.Contains(err.Error(), "不存在或已处理") {
		t.Fatalf("重复批准应失败: %v", err)
	}
}

// TestApiDocSkillApprovalRejectedWritesNothing 拒绝的单子一行不写（含批次）。
func TestApiDocSkillApprovalRejectedWritesNothing(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	get := captureFrames(t, svc)
	projectID := seedDemoBaseline(t, svc)
	before := apiRawTableCounts(t)

	path := writeDocFile(t, "# 演示服务接口变更文档\n\n"+
		apiMetaBlock("demo-svc", "refactor", "下线删除用户", "合规要求", "REQ-21")+
		"## 一、用户管理\n\n### 1.2 删除用户\n\n- **URL**：`DELETE /api/v1/demo/user/{id}`\n"+
		"- **变更**：删除\n- **变更点**：下线\n")
	if _, err := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillImport, map[string]any{"path": path}); err != nil {
		t.Fatalf("建单应成功: %v", err)
	}
	id := onlyApiDocApproval(t, svc)
	if err := svc.RejectApiDocImport(id); err != nil {
		t.Fatalf("拒绝失败: %v", err)
	}
	done := waitForApiDocDone(t, get)
	if done.payload["status"] != "rejected" {
		t.Fatalf("拒绝终态 = %v", done.payload["status"])
	}
	assertCountsUnchanged(t, svc, projectID, 2, 1)
	assertApiTablesUnchanged(t, before)
}

// TestApiDocSkillApprovalRequiresSession 没有会话 id 的 ctx 建不出单子（不静默降级成直发）。
func TestApiDocSkillApprovalRequiresSession(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	seedDemoBaseline(t, svc)
	path := writeDocFile(t, "# 演示服务接口变更文档\n\n"+
		apiMetaBlock("demo-svc", "refactor", "下线删除用户", "合规要求", "REQ-22")+
		"## 一、用户管理\n\n### 1.2 删除用户\n\n- **URL**：`DELETE /api/v1/demo/user/{id}`\n"+
		"- **变更**：删除\n- **变更点**：下线\n")
	_, err := runApiDocSkill(t, svc, context.Background(), skillapidoc.SkillImport, map[string]any{"path": path})
	if err == nil || !strings.Contains(err.Error(), "会话") {
		t.Fatalf("缺会话 id 应报错而不是执行: %v", err)
	}
	svc.apiApprovalsMu.Lock()
	n := len(svc.apiApprovals)
	svc.apiApprovalsMu.Unlock()
	if n != 0 {
		t.Fatalf("报错时不应留下审批单: %d 张", n)
	}
}

// ===================== ③ E-* 拒收零残留 =====================

func TestApiDocSkillRejectionZeroResidue(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	// 引用式写法（E-CROSS-REF）+ 缺 位置 列（E-POS-COL-MISSING）
	badDoc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("bad-svc", "feature", "坏文档", "验证拒收", "REQ-30") +
		"## 一、用户管理\n\n" +
		"### 1.1 查询用户\n\n- **URL**：`GET /api/v1/demo/user`\n- **变更**：新增\n\n" +
		"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 说明 |\n|---|---|---|---|\n| id | Long | 是 | 用户ID |\n"
	path := writeDocFile(t, badDoc)
	before := apiRawTableCounts(t)

	out, err := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillImport, map[string]any{"path": path})
	if err == nil {
		t.Fatalf("E-* 文档必须被拒收: %s", out)
	}
	if !strings.Contains(err.Error(), apidoc.ErrPosColMissing) || !strings.Contains(err.Error(), "数据库零变更") {
		t.Fatalf("拒收错误要带错误码与零变更承诺: %v", err)
	}
	assertApiTablesZero(t) // 连项目都不许留下
	assertApiTablesUnchanged(t, before)
	assertNoProjectResidue(t, svc, "bad-svc")

	// 体检同一份文档：只诊断，同样一行不写
	checkOut, err := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillCheck, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("体检不应报错: %v\n%s", err, checkOut)
	}
	if !strings.Contains(checkOut, apidoc.ErrPosColMissing) || !strings.Contains(checkOut, "以上仅为诊断") {
		t.Fatalf("体检输出应是结构化清单: %s", checkOut)
	}
	assertApiTablesZero(t)
}

// ===================== ④ chat 路径无法触发 init =====================

func TestApiDocSkillCannotTriggerInit(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	// 空项目 + scope: init：服务层闸门是放行的（界面向导正是靠这两条合法灌库），
	// 所以「chat 拒绝 init」必须由技能路径自己的闸保证——本用例钉的就是这个差集。
	initDoc := "# 演示服务存量文档\n\n```apidoc-meta\nspec: 1\nproject: init-svc\nscope: init\n```\n\n" +
		"## 一、用户管理\n\n" +
		"### 1.1 查询用户\n\n- **URL**：`GET /api/v1/demo/user`\n\n" +
		"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 说明 |\n|---|---|---|---|\n| id | Long | 是 | 用户ID |\n"
	path := writeDocFile(t, initDoc)

	// 先证明服务层此时是允许 init 的（否则本用例就没意义了）
	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: initDoc})
	if err != nil {
		t.Fatalf("PreviewApiDocImport: %v", err)
	}
	if pv.Mode != apidoc.ModeInit || pv.InitBlocked || !pv.OK {
		t.Fatalf("前置条件不成立：服务层应允许空项目 init，实际 %+v", pv)
	}

	out, err := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillImport, map[string]any{"path": path})
	if err == nil || !strings.Contains(err.Error(), "init") {
		t.Fatalf("chat 路径必须拒绝 init: err=%v out=%s", err, out)
	}
	assertApiTablesZero(t)

	// 对照：界面向导路径照样能灌（同一套方法，闸门只卡入口身份）
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: initDoc, InitMode: true})
	if res.EventID != 0 || res.Mode != apidoc.ModeInit {
		t.Fatalf("向导 init 应灌库且不产事件: %+v", res)
	}
	mustEntryByIdentity(t, svc, mustApiProjectID(t, svc, "init-svc"), "GET|/api/v1/demo/user")
}

// mustApiProjectID 按项目名取 id（不存在即 Fatal）。
func mustApiProjectID(t *testing.T, svc *Service, name string) string {
	t.Helper()
	p, found, err := svc.store.FindApiProjectByName(name)
	if err != nil || !found {
		t.Fatalf("项目不存在 %s: found=%v err=%v", name, found, err)
	}
	return p.ID
}

// ===================== ⑤ 正文入 LLM 前掩码 =====================

func TestApiDocSkillMasksSecretsIntoLLM(t *testing.T) {
	const bearer = "Bearer abcdefGHIJklmnopQRSTUVwx0123456789"
	const internalToken = "X-Internal-Token: supersecretvalue123456"
	const apiKey = "sk-abcdefghijklmnopqrstuvwxyz123456"
	// 半角冒号的 URL 行 → E-URL-FORMAT，原文行进 Issue.Snippet；
	// 未知 bullet → W-UNPARSED-BLOCK，同样带原文行。两者都是"文档正文衍生物"。
	doc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("mask-svc", "feature", "掩码验证", "验证凭据不外泄", "REQ-40") +
		"## 一、用户管理\n\n" +
		"### 1.1 查询用户\n\n" +
		"- **URL:** `GET /api/v1/demo/user` " + bearer + "\n" +
		"- **内部令牌**：" + internalToken + "\n" +
		"- **apikey**：" + apiKey + "\n" +
		"- **变更**：新增\n"
	path := writeDocFile(t, doc)
	svc := newTestService(t)
	attachApiDocSkills(t, svc)

	out, err := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillCheck, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("体检失败: %v\n%s", err, out)
	}
	for _, secret := range []string{
		"abcdefGHIJklmnopQRSTUVwx0123456789",
		"supersecretvalue123456",
		"abcdefghijklmnopqrstuvwxyz123456",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("凭据原文进了模型上下文（%s…）:\n%s", secret[:10], out)
		}
	}
	for _, want := range []string{"Bearer ***", "***", apidoc.ErrURLFormat} {
		if !strings.Contains(out, want) {
			t.Fatalf("期望掩码/错误码 %q 出现在回执里:\n%s", want, out)
		}
	}
	// 正文本身绝不整篇回传（只有 Issue 片段）
	if strings.Contains(out, apiMetaBlock("mask-svc", "feature", "掩码验证", "验证凭据不外泄", "REQ-40")) {
		t.Fatalf("不得把文档正文整篇回传给模型:\n%s", out)
	}
}

// ===================== ⑥ AC9 规范同源 =====================

// TestApiDocSkillSpecSameSourceAsUIExport 技能交付的规范文本、界面导出、Go 常量三者逐字节相等。
func TestApiDocSkillSpecSameSourceAsUIExport(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	out, err := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillSpec, map[string]any{})
	if err != nil {
		t.Fatalf("apidoc_spec: %v", err)
	}
	ui, err := svc.ExportApiDocSpec()
	if err != nil {
		t.Fatalf("ExportApiDocSpec: %v", err)
	}
	if out != apidoc.SpecText || ui != apidoc.SpecText {
		t.Fatalf("规范文本出现第二份来源：skill %d 字节 / 界面 %d 字节 / 常量 %d 字节",
			len(out), len(ui), len(apidoc.SpecText))
	}
	if !strings.Contains(out, "接口变更文档规范 v1") || !strings.Contains(out, "```apidoc-meta") {
		t.Fatal("规范常量内容异常（缺标题或 meta 骨架）")
	}
}

// ===================== ⑦ 同一套方法，不是第二份解析 =====================

// TestApiDocSkillCheckIsPreviewApiDocImportProjection 技能看到的结论必须等于
// PreviewApiDocImport 的投影：同一份解析、同一套 diff、同一套闸门判据。
func TestApiDocSkillCheckIsPreviewApiDocImportProjection(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	doc := demoAC1Doc() // 与库状态矛盾的声明（新增但已存在/删除但库里没有）→ 编排层 E-*
	path := writeDocFile(t, doc)

	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: doc, DocRef: path})
	if err != nil {
		t.Fatalf("PreviewApiDocImport: %v", err)
	}
	ck, err := svc.ApiDocSkillCheck(context.Background(), skillapidoc.ImportInput{DocText: doc, DocRef: path})
	if err != nil {
		t.Fatalf("ApiDocSkillCheck: %v", err)
	}
	if ck.Message != pv.Message || ck.Stats.Added != pv.Stats.Added || ck.Stats.Updated != pv.Stats.Updated ||
		ck.Stats.Removed != pv.Stats.Removed || ck.Breaking != pv.Breaking ||
		ck.NeedConfirm != pv.NeedConfirm || ck.OK != pv.OK || ck.Mode != pv.Mode ||
		len(ck.Issues) != len(pv.Issues) {
		t.Fatalf("技能结论与向导预览不一致: %+v vs %+v", ck, pv)
	}
	for i := range pv.Issues {
		if ck.Issues[i].Code != pv.Issues[i].Code || ck.Issues[i].Line != pv.Issues[i].Line {
			t.Fatalf("第 %d 条 Issue 不一致: %+v vs %+v", i, ck.Issues[i], pv.Issues[i])
		}
	}
	if len(ck.Changes) != len(pv.Changes) {
		t.Fatalf("变更分栏条数不一致: %d vs %d", len(ck.Changes), len(pv.Changes))
	}
	// 技能路径同样能看到编排层的 E-*（声明与库状态矛盾），而不是自己另算一套
	if !pv.OK && !strings.Contains(ck.Message, "阻断") {
		t.Fatalf("应看到与库矛盾的阻断问题: %+v", ck)
	}

	// 两个入口的最终写入结果也必须同源：技能导入同一份文档 = 向导提交同一份文档
	_, skillErr := runApiDocSkill(t, svc, skillSessionCtx(), skillapidoc.SkillImport, map[string]any{"path": path})
	if skillErr == nil || !strings.Contains(skillErr.Error(), "整批拒收") {
		t.Fatalf("矛盾声明应被拒收: %v", skillErr)
	}
	if _, uiErr := svc.CommitApiDocImport(ApiDocImportRequest{DocText: doc, DocRef: path, Confirmed: true}); uiErr == nil {
		t.Fatal("向导路径提交同一份文档同样应被拒收（同一套闸门）")
	}
	assertApiTablesZero(t)
}

// ===================== ⑧ 补装迁移 =====================

func TestMigrateApiDocSkills(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)

	if err := svc.MigrateApiDocSkills(); err != nil {
		t.Fatalf("MigrateApiDocSkills: %v", err)
	}
	a, err := svc.store.GetAgent(agent.BuiltinOpsAgentID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	for _, n := range skillapidoc.SkillNames {
		if !stringIn(a.Skills, n) {
			t.Fatalf("内置 Agent 应已装配 %s: %v", n, a.Skills)
		}
	}
	// 幂等：第二次跑不产生重复条目
	before := len(a.Skills)
	if err := svc.MigrateApiDocSkills(); err != nil {
		t.Fatalf("第二次 MigrateApiDocSkills: %v", err)
	}
	a2, _ := svc.store.GetAgent(agent.BuiltinOpsAgentID)
	if len(a2.Skills) != before {
		t.Fatalf("补装必须幂等: %d → %d（%v）", before, len(a2.Skills), a2.Skills)
	}
	// 用户后来自己取消勾选 → 标记已存在，启动绝不再塞回去
	if err := svc.store.ReplaceAgentSkills(agent.BuiltinOpsAgentID, []string{"ops_list_containers"}); err != nil {
		t.Fatalf("ReplaceAgentSkills: %v", err)
	}
	if err := svc.MigrateApiDocSkills(); err != nil {
		t.Fatalf("第三次 MigrateApiDocSkills: %v", err)
	}
	a3, _ := svc.store.GetAgent(agent.BuiltinOpsAgentID)
	if len(a3.Skills) != 1 || a3.Skills[0] != "ops_list_containers" {
		t.Fatalf("已补装过就不该再干预用户装配: %v", a3.Skills)
	}
}

// TestApiDocSkillListedInSkillManager 技能元数据自动进前端技能清单（前端零硬编码的根据）。
func TestApiDocSkillListedInSkillManager(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	seen := map[string]SkillInfo{}
	for _, info := range svc.ListSkills() {
		seen[info.Name] = info
	}
	for _, n := range skillapidoc.SkillNames {
		info, ok := seen[n]
		if !ok {
			t.Fatalf("ListSkills 里没有 %s（技能管理/装配区就看不到）", n)
		}
		if !info.Builtin || strings.TrimSpace(info.Description) == "" {
			t.Fatalf("%s 元数据异常: %+v", n, info)
		}
	}
	// Agent 装配校验只认已注册的名字（未注册技能装配必失败——这正是"未装配时不静默假装执行"的根）
	if err := svc.agents.SetSkills(agent.BuiltinOpsAgentID, []string{"apidoc_not_registered"}); err == nil {
		t.Fatal("装配未注册的技能应失败")
	}
	if err := svc.agents.SetSkills(agent.BuiltinOpsAgentID, skillapidoc.SkillNames); err != nil {
		t.Fatalf("装配已注册的接口文档技能: %v", err)
	}
}

// stringIn 名字是否在切片里。
func stringIn(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}
