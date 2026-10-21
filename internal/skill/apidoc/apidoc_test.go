package apidoc

// 接口文档技能单测（假编排层 + 真文件）：
// 钉住四件事——① 注册形状（名字前缀/下划线命名空间/参数 schema）；
// ② 保真红线（正文只从磁盘读，参数里塞正文一律拒绝；check 永不写库）；
// ③ D16 分流（非破坏直发 / 破坏走审批闸 / E-* 整批拒收 / init 一律拒绝）；
// ④ 正文衍生物入 LLM 前必须过 sanitize。
//
// 「check 与 import 走的是同一套编排方法」在这里用假实现记账（同一批被调方法、
// 本包零解析逻辑），真 SQLite 的等价性在 internal/appsvc/apidocskill_test.go 里钉。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	docspec "DevCraft/internal/apidoc"
	"DevCraft/internal/skill"
)

// fakeDocs 记录技能调用了哪些编排方法、传了什么（假实现的"账本"）。
type fakeDocs struct {
	specText string
	specErr  error

	check    ImportCheck
	checkErr error
	checks   []ImportInput

	commit    ImportReceipt
	commitErr error
	commits   []ImportInput

	approvalID  string
	approvalErr error
	approvals   []ImportInput
}

func (f *fakeDocs) ApiDocSkillSpec(context.Context) (string, error) { return f.specText, f.specErr }

func (f *fakeDocs) ApiDocSkillCheck(_ context.Context, in ImportInput) (ImportCheck, error) {
	f.checks = append(f.checks, in)
	return f.check, f.checkErr
}

func (f *fakeDocs) ApiDocSkillCommit(_ context.Context, in ImportInput) (ImportReceipt, error) {
	f.commits = append(f.commits, in)
	return f.commit, f.commitErr
}

func (f *fakeDocs) ApiDocSkillSubmitApproval(_ context.Context, in ImportInput) (string, ImportCheck, error) {
	f.approvals = append(f.approvals, in)
	return f.approvalID, f.check, f.approvalErr
}

// writeDoc 写一个真文件（路径由测试给出，正文由技能自己读）。
func writeDoc(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api-change.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	return path
}

// mustArgs 把 map 编成 JSON 参数（失败即 Fatal）。
func mustArgs(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}

// execSkill 按名字取技能并执行（走注册表，与 agent 循环的 dispatch 同一条路）。
func execSkill(t *testing.T, reg *skill.Registry, name string, args json.RawMessage) (string, error) {
	t.Helper()
	sk, ok := reg.Get(name)
	if !ok {
		t.Fatalf("技能未注册: %s", name)
	}
	return sk.Execute(context.Background(), args)
}

// registerFake 注册四个技能到独立注册表（三件套 + apidoc_query），
// 返回注册表与假编排层；查询缝用空假实现（query 用例另有 registerFakeQuery）。
func registerFake(t *testing.T, f *fakeDocs) *skill.Registry {
	t.Helper()
	return registerFakeQuery(t, f, newFakeQuery())
}

func registerFakeQuery(t *testing.T, f *fakeDocs, q QuerySource) *skill.Registry {
	t.Helper()
	reg := skill.NewRegistry()
	if err := Register(reg, f, q); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// ===================== 注册形状 =====================

// TestSkillRegistration 注册成功 + 命名约定：apidoc_ 前缀、含下划线命名空间、
// 匹配 function-calling 的工具名字符集（Registry 强制含 "_"，skill.go:56-58）。
func TestSkillRegistration(t *testing.T) {
	reg := registerFake(t, &fakeDocs{})
	all := append(append([]string{}, SkillNames...), SkillQuery) // 交付三件套 + 只读查询
	names := reg.Names()
	if len(names) != len(all) {
		t.Fatalf("注册技能数 = %d, want %d（%v）", len(names), len(all), names)
	}
	toolNameRe := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	for _, n := range all {
		if !strings.HasPrefix(n, "apidoc_") {
			t.Errorf("技能名 %q 必须以 apidoc_ 开头（命名空间约定）", n)
		}
		if !strings.Contains(n, "_") {
			t.Errorf("技能名 %q 必须含下划线命名空间，否则 Registry.Register 拒收", n)
		}
		if !toolNameRe.MatchString(n) {
			t.Errorf("技能名 %q 不匹配 ^[a-zA-Z0-9_-]+$（供应商会以 400 拒绝）", n)
		}
		if _, ok := reg.Get(n); !ok {
			t.Errorf("技能未注册: %s", n)
		}
		if !reg.IsBuiltin(n) {
			t.Errorf("已注册技能应标记为内置: %s", n)
		}
	}
	// 同名重复注册必须报错（防止静默覆盖成第二份实现）
	if err := Register(reg, &fakeDocs{}, newFakeQuery()); err == nil {
		t.Fatal("重复注册同名技能应报错")
	}
}

// TestSkillDescriptionsAndSchemas 每个技能的 Description 非空且讲清边界，
// Parameters 是合法 JSON Schema；check/import 的 schema 里绝不允许出现正文字段。
func TestSkillDescriptionsAndSchemas(t *testing.T) {
	reg := registerFake(t, &fakeDocs{})
	for _, name := range append(append([]string{}, SkillNames...), SkillQuery) {
		sk, _ := reg.Get(name)
		if strings.TrimSpace(sk.Description()) == "" {
			t.Fatalf("%s Description 为空（模型无法路由）", name)
		}
		var schema map[string]any
		if err := json.Unmarshal(sk.Parameters(), &schema); err != nil {
			t.Fatalf("%s Parameters 不是合法 JSON: %v", name, err)
		}
		if schema["type"] != "object" {
			t.Fatalf("%s schema type = %v, want object", name, schema["type"])
		}
		props, _ := schema["properties"].(map[string]any)
		for banned := range props {
			if banned == "docText" || banned == "content" || banned == "text" {
				t.Fatalf("%s 的 schema 出现正文字段 %q（违反 D5.1 保真红线）", name, banned)
			}
		}
		if name == SkillCheck || name == SkillImport {
			req, _ := schema["required"].([]any)
			if len(req) != 1 || req[0] != "path" {
				t.Fatalf("%s required = %v, want [path]", name, req)
			}
			if schema["additionalProperties"] != false {
				t.Fatalf("%s 必须 additionalProperties:false", name)
			}
		}
	}
}

// ===================== apidoc_spec =====================

// TestSpecSkillReturnsSourceVerbatim 规范文本原样交付（不掩码、不加包装文案）——
// 技能拿到的就是编排层给的那一份常量，逐字节相等（AC9 的技能侧一半）。
func TestSpecSkillReturnsSourceVerbatim(t *testing.T) {
	docs := &fakeDocs{specText: docspec.SpecText}
	reg := registerFake(t, docs)
	out, err := execSkill(t, reg, SkillSpec, mustArgs(t, map[string]any{}))
	if err != nil {
		t.Fatalf("apidoc_spec: %v", err)
	}
	if out != docspec.SpecText {
		t.Fatalf("规范文本必须逐字节等于 apidoc.SpecText（got %d 字节, want %d 字节）", len(out), len(docspec.SpecText))
	}
	// 缺省参数（模型常传 null）同样可用；多余参数拒绝
	if _, err := execSkill(t, reg, SkillSpec, json.RawMessage("null")); err != nil {
		t.Fatalf("null 参数应被容忍: %v", err)
	}
	if _, err := execSkill(t, reg, SkillSpec, mustArgs(t, map[string]any{"path": "x.md"})); err == nil {
		t.Fatal("apidoc_spec 不接受任何参数")
	}
	// 空规范文本 = 单一真相源被破坏，必须报错而不是回一句空话
	emptyReg := registerFake(t, &fakeDocs{})
	if _, err := execSkill(t, emptyReg, SkillSpec, json.RawMessage("{}")); err == nil {
		t.Fatal("规范文本为空时应报错")
	}
}

// ===================== 保真红线：正文只从磁盘读 =====================

// TestCheckSkillReadsFileAndNeverWrites check 走磁盘原文、只调预览方法，绝不触碰写入。
func TestCheckSkillReadsFileAndNeverWrites(t *testing.T) {
	const body = "# 演示服务接口变更文档\n\n正文原文\n"
	path := writeDoc(t, body)
	docs := &fakeDocs{check: ImportCheck{OK: true, Mode: docspec.ModeIncremental, ProjectName: "demo-svc", Message: "无变更"}}
	reg := registerFake(t, docs)

	out, err := execSkill(t, reg, SkillCheck, mustArgs(t, map[string]any{"path": path, "project": "demo-svc"}))
	if err != nil {
		t.Fatalf("apidoc_check: %v", err)
	}
	if len(docs.checks) != 1 {
		t.Fatalf("check 应只调一次编排层预览: %d 次", len(docs.checks))
	}
	got := docs.checks[0]
	if got.DocText != body {
		t.Fatalf("正文必须是磁盘原文，got %q want %q", got.DocText, body)
	}
	if !strings.HasSuffix(filepath.ToSlash(got.DocRef), "api-change.md") {
		t.Fatalf("DocRef 应是解析后的绝对路径: %q", got.DocRef)
	}
	if got.ProjectName != "demo-svc" || got.Confirmed {
		t.Fatalf("check 不应带确认标记: %+v", got)
	}
	if len(docs.commits) != 0 || len(docs.approvals) != 0 {
		t.Fatalf("check 是只读诊断，不得调用写入/审批: commits=%d approvals=%d", len(docs.commits), len(docs.approvals))
	}
	if !strings.Contains(out, "以上仅为诊断：本技能没有写入任何数据。") {
		t.Fatalf("check 输出必须讲清只诊断不写库: %q", out)
	}
}

// TestBodyPassedAsArgumentIsRejected 参数里塞正文一律拒绝（schema 之外的键不接受）。
func TestBodyPassedAsArgumentIsRejected(t *testing.T) {
	path := writeDoc(t, "# 文档\n")
	for _, name := range []string{SkillCheck, SkillImport} {
		reg := registerFake(t, &fakeDocs{})
		_, err := execSkill(t, reg, name, mustArgs(t, map[string]any{
			"path": path, "docText": "被模型改写过的正文",
		}))
		if err == nil || !strings.Contains(err.Error(), "docText") {
			t.Fatalf("%s 应拒绝正文参数并点名该键: %v", name, err)
		}
	}
}

// TestReadDocGuards 路径缺省 / 不存在 / 目录 / 空文件——都要中文报错，不猜路径。
func TestReadDocGuards(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cases := []struct {
		name string
		path string
		want string
	}{
		{"缺路径", "", "需要提供 path"},
		{"不存在", filepath.Join(dir, "nope.md"), "读不到文档"},
		{"目录", dir, "是目录不是文档文件"},
		{"空文件", empty, "文档内容为空"},
	}
	for _, c := range cases {
		reg := registerFake(t, &fakeDocs{})
		if _, err := execSkill(t, reg, SkillCheck, mustArgs(t, map[string]any{"path": c.path})); err == nil ||
			!strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want 含 %q", c.name, err, c.want)
		}
	}
}

// TestCheckMasksDocSecretsBeforeLLM 正文衍生物（Issue 原文片段）入 LLM 前必须掩码（D5.1）。
func TestCheckMasksDocSecretsBeforeLLM(t *testing.T) {
	path := writeDoc(t, "# 文档\n")
	const bearer = "Bearer abcdefGHIJklmnopQRSTUVwx0123456789"
	const internalToken = "X-Internal-Token: supersecretvalue123456"
	docs := &fakeDocs{check: ImportCheck{
		OK: false, Mode: docspec.ModeIncremental, ProjectName: "demo-svc",
		Message: "文档校验未通过",
		Issues: []docspec.Issue{
			{Level: docspec.LevelError, Code: docspec.ErrURLFormat, Line: 7,
				Snippet: "- **URL**：`GET /api/v1/demo/x` " + bearer, Want: "- **URL**：`METHOD /path`"},
			{Level: docspec.LevelWarning, Code: docspec.WarnUnparsedBlock, Line: 9,
				Snippet: "- **Header**：" + internalToken, Want: "规范定义的 bullet"},
		},
	}}
	reg := registerFake(t, docs)
	out, err := execSkill(t, reg, SkillCheck, mustArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("apidoc_check: %v", err)
	}
	for _, secret := range []string{"abcdefGHIJklmnopQRSTUVwx0123456789", "supersecretvalue123456"} {
		if strings.Contains(out, secret) {
			t.Fatalf("凭据 %s… 原文泄漏给模型: %s", secret[:8], out)
		}
	}
	if !strings.Contains(out, "Bearer ***") {
		t.Fatalf("Bearer 应被掩码成 Bearer ***: %s", out)
	}
	if !strings.Contains(out, docspec.ErrURLFormat) || !strings.Contains(out, "第7行") {
		t.Fatalf("输出必须带错误码与行号（模型据此翻译成人话）: %s", out)
	}
}

// ===================== apidoc_import 的 D16 分流 =====================

// TestImportDirectCommitForNonBreaking 纯新增/非破坏 → 直接提交（带确认标记）并回报事件与统计。
func TestImportDirectCommitForNonBreaking(t *testing.T) {
	path := writeDoc(t, "# 文档\n")
	docs := &fakeDocs{
		check: ImportCheck{OK: true, Mode: docspec.ModeIncremental, ProjectName: "demo-svc",
			Stats: ImportStats{Added: 2}, Changes: []ImportChange{{Method: "GET", Path: "/a", Name: "查A", Domain: "领域A", ChangeType: "added"}}},
		commit: ImportReceipt{EventID: 17, Seq: 3, Mode: docspec.ModeIncremental,
			Stats: ImportStats{Added: 2}, Message: "导入成功", CreatedProject: true},
	}
	reg := registerFake(t, docs)
	out, err := execSkill(t, reg, SkillImport, mustArgs(t, map[string]any{"path": path, "label": "REQ-9"}))
	if err != nil {
		t.Fatalf("apidoc_import: %v", err)
	}
	if len(docs.commits) != 1 || len(docs.approvals) != 0 {
		t.Fatalf("非破坏应直接提交且不进审批: commits=%d approvals=%d", len(docs.commits), len(docs.approvals))
	}
	if !docs.commits[0].Confirmed {
		t.Fatal("非破坏直发必须带确认标记")
	}
	if docs.commits[0].DocText != "# 文档\n" {
		t.Fatalf("提交的正文必须是磁盘原文: %q", docs.commits[0].DocText)
	}
	if !strings.Contains(out, "已生成变更事件 v3") || !strings.Contains(out, "新增 2") {
		t.Fatalf("回执要如实报告事件版本与统计: %s", out)
	}
	// 幂等重导（无事件）也要如实说明，不能报"生成了事件"
	noEvent := registerFake(t, &fakeDocs{
		check:  ImportCheck{OK: true, Mode: docspec.ModeIncremental, ProjectName: "demo-svc"},
		commit: ImportReceipt{Mode: docspec.ModeIncremental, Message: "无变更"},
	})
	out2, err := execSkill(t, noEvent, SkillImport, mustArgs(t, map[string]any{"path": path}))
	if err != nil || !strings.Contains(out2, "本次未生成事件") {
		t.Fatalf("空 diff 应报告未生成事件: out=%q err=%v", out2, err)
	}
}

// TestImportBreakingGoesToApprovalGate 含删除/破坏 → 只创建审批单，绝不提交。
func TestImportBreakingGoesToApprovalGate(t *testing.T) {
	path := writeDoc(t, "# 文档\n")
	docs := &fakeDocs{
		approvalID: "approval-42",
		check: ImportCheck{OK: true, NeedConfirm: true, Mode: docspec.ModeIncremental,
			ProjectName: "demo-svc", Breaking: 2, NextSeq: 4,
			Stats:   ImportStats{Updated: 1, Removed: 1},
			Changes: []ImportChange{{Method: "DELETE", Path: "/u/{id}", Name: "删除用户", Domain: "用户", ChangeType: "removed", Breaking: 2}},
		},
	}
	reg := registerFake(t, docs)
	out, err := execSkill(t, reg, SkillImport, mustArgs(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("apidoc_import: %v", err)
	}
	if len(docs.commits) != 0 {
		t.Fatal("未批准前绝不允许调用提交（一行都不该写）")
	}
	if len(docs.approvals) != 1 || docs.approvals[0].Confirmed {
		t.Fatalf("应创建待审批单且不带确认标记: %+v", docs.approvals)
	}
	for _, want := range []string{"approval-42", "在用户于审批卡片点击「批准」之前绝不会写入任何数据", "不要声称导入已经完成"} {
		if !strings.Contains(out, want) {
			t.Fatalf("回执缺少 %q，实际: %s", want, out)
		}
	}
	if !strings.Contains(out, "breaking=2") || !strings.Contains(out, "删除 1") {
		t.Fatalf("回执要讲清破坏档与统计: %s", out)
	}
	// 审批单创建失败 → 原样报错，不许退化成"已导入"
	broken := registerFake(t, &fakeDocs{
		check:       ImportCheck{OK: true, NeedConfirm: true, ProjectName: "demo-svc", Mode: docspec.ModeIncremental},
		approvalErr: errFakeNoSession,
	})
	if _, err := execSkill(t, broken, SkillImport, mustArgs(t, map[string]any{"path": path})); err == nil ||
		!strings.Contains(err.Error(), "会话") {
		t.Fatalf("审批单创建失败应报错: %v", err)
	}
}

// errFakeNoSession 模拟服务端解不出会话 id（Skill 接口没有会话参数，缺 ctx 注入就创建不了单）。
var errFakeNoSession = &fakeErr{msg: "内部错误: 无法识别当前会话，不能创建接口文档审批单"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

// TestImportRejectsErrorDoc 任何 E-* → 整批拒收（后端已保证零写入），错误里带码与行号。
func TestImportRejectsErrorDoc(t *testing.T) {
	path := writeDoc(t, "# 文档\n")
	docs := &fakeDocs{check: ImportCheck{
		OK: false, Mode: docspec.ModeIncremental, ProjectName: "demo-svc",
		Message: "文档校验未通过，共 2 个阻断问题",
		Issues: []docspec.Issue{
			{Level: docspec.LevelError, Code: docspec.ErrCrossRef, Line: 12, Want: "参数表必须原样展开"},
			{Level: docspec.LevelError, Code: docspec.ErrPosColMissing, Line: 18, Want: "请求参数表含「位置」列"},
			{Level: docspec.LevelWarning, Code: docspec.WarnCURLMismatch, Line: 30, Want: "curl 与 URL 行一致"},
		},
	}}
	reg := registerFake(t, docs)
	_, err := execSkill(t, reg, SkillImport, mustArgs(t, map[string]any{"path": path}))
	if err == nil {
		t.Fatal("E-* 文档必须被拒收")
	}
	for _, want := range []string{docspec.ErrCrossRef, "第12行", docspec.ErrPosColMissing, "第18行", "数据库零变更"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒收错误缺少 %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), docspec.WarnCURLMismatch) {
		t.Fatalf("拒收只列 E-*，不该把警告混进来: %v", err)
	}
	if len(docs.commits) != 0 || len(docs.approvals) != 0 {
		t.Fatal("拒收后不得再提交或建单")
	}
}

// TestImportRejectsInitMode §13.18：chat 路径一律拒绝 init——即使服务层闸门会放行
// （空项目 / 向导勾选）也要在技能层拒掉，且绝不建审批单。
func TestImportRejectsInitMode(t *testing.T) {
	path := writeDoc(t, "# 存量散文文档\n")
	docs := &fakeDocs{check: ImportCheck{OK: true, Mode: docspec.ModeInit, ProjectName: "demo-svc",
		Stats: ImportStats{Added: 20}}}
	reg := registerFake(t, docs)
	_, err := execSkill(t, reg, SkillImport, mustArgs(t, map[string]any{"path": path}))
	if err == nil || !strings.Contains(err.Error(), "init") || !strings.Contains(err.Error(), "界面导入向导") {
		t.Fatalf("init 文档必须被拒并指向界面向导: %v", err)
	}
	if len(docs.commits) != 0 || len(docs.approvals) != 0 {
		t.Fatalf("init 拒收后不得写入或建单: commits=%d approvals=%d", len(docs.commits), len(docs.approvals))
	}
	// 服务层 init 闸门拦截（InitBlocked）同样拒收
	blocked := registerFake(t, &fakeDocs{check: ImportCheck{OK: true, Mode: docspec.ModeIncremental,
		ProjectName: "demo-svc", InitBlocked: true}})
	if _, err := execSkill(t, blocked, SkillImport, mustArgs(t, map[string]any{"path": path})); err == nil ||
		!strings.Contains(err.Error(), "初始化导入") {
		t.Fatalf("InitBlocked 应被拒: %v", err)
	}
}

// TestImportKindEnum 覆盖元信息 kind 越界直接拒（不烧一次解析）。
func TestImportKindEnum(t *testing.T) {
	path := writeDoc(t, "# 文档\n")
	reg := registerFake(t, &fakeDocs{})
	if _, err := execSkill(t, reg, SkillImport, mustArgs(t, map[string]any{"path": path, "kind": "chore"})); err == nil ||
		!strings.Contains(err.Error(), "feature/fix/refactor/docs") {
		t.Fatalf("非法 kind 应被拒: %v", err)
	}
}

// TestImportCommitFailureReported 提交失败（含服务层闸门）必须说清"事务已回滚"，
// 不许让模型以为写进去了。
func TestImportCommitFailureReported(t *testing.T) {
	path := writeDoc(t, "# 文档\n")
	docs := &fakeDocs{
		check:     ImportCheck{OK: true, Mode: docspec.ModeIncremental, ProjectName: "demo-svc"},
		commitErr: &fakeErr{msg: "接口文档校验未通过，共 1 个问题：E-ADDED-EXISTS(第9行)"},
	}
	reg := registerFake(t, docs)
	_, err := execSkill(t, reg, SkillImport, mustArgs(t, map[string]any{"path": path}))
	if err == nil || !strings.Contains(err.Error(), "事务已回滚") || !strings.Contains(err.Error(), "E-ADDED-EXISTS") {
		t.Fatalf("提交失败要如实回报: %v", err)
	}
}

// TestCheckReportsGateStates 体检输出必须把「需要确认」「init 文档」这两种下一步讲清，
// 否则模型会以为可以直接导入。
func TestCheckReportsGateStates(t *testing.T) {
	path := writeDoc(t, "# 文档\n")
	docs := &fakeDocs{check: ImportCheck{OK: true, NeedConfirm: true, Mode: docspec.ModeIncremental,
		ProjectName: "demo-svc", Breaking: 1, NextSeq: 2}}
	reg := registerFake(t, docs)
	out, err := execSkill(t, reg, SkillCheck, mustArgs(t, map[string]any{"path": path}))
	if err != nil || !strings.Contains(out, "必须用户在卡片里批准后才落库") {
		t.Fatalf("体检要预告审批闸: %q %v", out, err)
	}

	initDocs := &fakeDocs{check: ImportCheck{OK: true, Mode: docspec.ModeInit, ProjectName: "demo-svc"}}
	initReg := registerFake(t, initDocs)
	out2, err := execSkill(t, initReg, SkillCheck, mustArgs(t, map[string]any{"path": path}))
	if err != nil || !strings.Contains(out2, "chat 路径一律拒绝执行") {
		t.Fatalf("体检要预告 init 在聊天里不可执行: %q %v", out2, err)
	}
}
