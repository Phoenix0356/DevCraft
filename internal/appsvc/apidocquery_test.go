package appsvc

// apidocquery_test.go 是只读查询技能 apidoc_query 的集成测试（任务 10-21）：
// 真 SQLite + 真解析器 + 真注册表，数据由界面向导同一套 CommitApiDocImport 灌入，
// 再经注册表 dispatch 六个 action（AC1–AC3 的「工具能答出来」等价验证）。
//
// 钉住的事：
//   ① 六个 action 在真实库上逐个可用，输出含真实数据（分层形状见 query_test.go）；
//   ② 只读（AC6/DoD）：全部调用前后 9 张 api_* 表行数逐一不变；
//   ③ 脱敏（AC5）：入库描述里的 Bearer token 出网前被掩码，raw_markdown 不回传；
//   ④ truncated（AC4 的工具侧一半）：limit 小于命中数时显式 truncated=true + total；
//   ⑤ 补装迁移（AC7）：seed.apidoc_query_v1 新标记键、幂等、不复活用户取消的装配、
//      与旧键 seed.apidoc_skills_v1 互不干扰；
//   ⑥ 装配链路（DoD）：ListSkills 呈现 + agents.SetSkills 可装配（前端技能视图零改动的根据）。
//
// 约定与 apidocskill_test.go 一致：t.TempDir() 真 SQLite、手写 t.Fatalf、无断言库。

import (
	"context"
	"strings"
	"testing"

	skillapidoc "DevCraft/internal/skill/apidoc"
	"DevCraft/internal/agent"
)

// seedTwoEventsBaseline 铺两份数据：基线导入（v1）+ 一次含增/改/删的增量导入（v2），
// 让六个 action 都有真实对象可查。返回项目 id。
func seedTwoEventsBaseline(t *testing.T, svc *Service) string {
	t.Helper()
	pid := seedDemoBaseline(t, svc)
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: demoAC1Doc(), Confirmed: true})
	if res.Seq != 2 {
		t.Fatalf("第二次导入应生成事件 v2: %+v", res)
	}
	return pid
}

// ===================== ① + ② 六个 action 真实库闭环 + 只读断言 =====================

func TestApiDocQuerySixActionsRealDB(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	pid := seedTwoEventsBaseline(t, svc)

	// 只读断言（AC6）：调用前快照 9 张 api_* 表行数，六个 action 全部跑完后逐一比对。
	before := apiRawTableCounts(t)
	ctx := context.Background()

	run := func(args map[string]any) string {
		t.Helper()
		out, err := runApiDocSkill(t, svc, ctx, skillapidoc.SkillQuery, args)
		if err != nil {
			t.Fatalf("apidoc_query %v: %v\n%s", args, err, out)
		}
		return out
	}

	// projects（AC1 的规模口径）
	out := run(map[string]any{"action": "projects"})
	for _, want := range []string{"- demo-svc（显示名 演示服务） ｜ 接口 3 ｜ 事件 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("projects 输出缺少 %q:\n%s", want, out)
		}
	}

	// domains（AC1：一次调用答出「有哪些领域、各多少个接口」，不需要读全量定义）
	out = run(map[string]any{"action": "domains", "project": "demo-svc"})
	if !strings.Contains(out, "- 用户管理 ｜ 接口 3") {
		t.Fatalf("domains 输出:\n%s", out)
	}

	// entries：keyword 缩小 + 摘要行形状（AC2 第一跳）
	out = run(map[string]any{"action": "entries", "project": "demo-svc", "keyword": "login"})
	if !strings.Contains(out, "GET /api/v1/demo/login-log ｜") || !strings.Contains(out, "GET /api/v1/demo/login-log/export ｜") {
		t.Fatalf("entries keyword 过滤失效:\n%s", out)
	}
	if strings.Contains(out, "oldPwd") || strings.Contains(out, "返回字段:") {
		t.Fatalf("摘要层泄漏了字段表（R2）:\n%s", out)
	}

	// entries 截断（AC4 工具侧：真实数据也要给出 truncated + total + 缩小指引）
	out = run(map[string]any{"action": "entries", "project": "demo-svc", "limit": 2})
	if !strings.Contains(out, "truncated=true") || !strings.Contains(out, "共 3 条") || !strings.Contains(out, "前 2 条") {
		t.Fatalf("limit=2 必须显式截断:\n%s", out)
	}
	if strings.Count(out, "- [id=") != 2 {
		t.Fatalf("截断后只该有 2 条摘要:\n%s", out)
	}

	// entry：真实详情（AC2 第二跳——id 从摘要行来）
	entry := mustEntryByIdentity(t, svc, pid, "POST|/api/v1/demo/password")
	out = run(map[string]any{"action": "entry", "id": entry.ID})
	for _, want := range []string{
		"POST /api/v1/demo/password ｜ 修改密码",
		"领域 用户管理",
		"oldPwd ｜ String ｜ 必填", // v2 把 oldPwd 改成了必填，详情必须如实反映
		"返回字段:", "message ｜ String",
		"该接口历史事件", "v2 [id=",
		"raw_markdown", // R3 出口说明
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("entry 详情缺少 %q:\n%s", want, out)
		}
	}

	// events（AC3 第一跳）
	out = run(map[string]any{"action": "events", "project": "demo-svc"})
	if !strings.Contains(out, "v2 [id=") || !strings.Contains(out, "v1 [id=") {
		t.Fatalf("events 应列出 v2/v1:\n%s", out)
	}

	// event（AC3 第二跳：明细 + reason）
	evs, err := svc.ListApiChangeEvents(ApiEventQuery{ProjectID: pid, Limit: 1})
	if err != nil || len(evs) == 0 {
		t.Fatalf("ListApiChangeEvents: %v（%d 条）", err, len(evs))
	}
	out = run(map[string]any{"action": "event", "id": evs[0].ID})
	for _, want := range []string{
		"变更事件 v2",
		"为什么改(reason): 安全要求旧密码必填并新增登录日志接口",
		"POST /api/v1/demo/password ｜ 修改密码 ｜ 领域 用户管理 ｜ updated ｜ breaking=1",
		"oldPwd", "rule=",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("event 详情缺少 %q:\n%s", want, out)
		}
	}

	// 反例在真实缝上同样成立：项目不存在 → 列出可用项目
	if _, err := runApiDocSkill(t, svc, ctx, skillapidoc.SkillQuery, map[string]any{"action": "entries", "project": "ghost"}); err == nil ||
		!strings.Contains(err.Error(), "demo-svc") {
		t.Fatalf("项目不存在应报错并列出可用项目: %v", err)
	}

	assertApiTablesUnchanged(t, before)
}

// ===================== ③ 真实数据出网前脱敏（AC5）=====================

func TestApiDocQueryMasksSecretsRealDB(t *testing.T) {
	const bearer = "Bearer abcdefGHIJklmnopQRSTUVwx0123456789"
	svc := newTestService(t)
	attachApiDocSkills(t, svc)
	// 描述段里带 Authorization: Bearer（接口文档的常态），入库后查出来必须被掩码。
	doc := "# 演示服务接口变更文档\n\n" + apiMetaBlock("sec-svc", "feature", "脱敏验证", "验证凭据不外泄", "REQ-50") +
		"## 一、用户管理\n\n### 1.1 查询用户\n\n调用需携带 Authorization: " + bearer + "。\n\n" +
		"- **URL**：`GET /api/v1/demo/sec`\n- **认证**：需要登录\n- **变更**：新增\n\n" +
		apiRespSection("| message | String | 响应消息 |")
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: doc})
	entry := mustEntryByIdentity(t, svc, res.ProjectID, "GET|/api/v1/demo/sec")

	out, err := runApiDocSkill(t, svc, context.Background(), skillapidoc.SkillQuery, map[string]any{"action": "entry", "id": entry.ID})
	if err != nil {
		t.Fatalf("entry: %v\n%s", err, out)
	}
	if strings.Contains(out, "abcdefGHIJklmnopQRSTUVwx0123456789") {
		t.Fatalf("入库的 Bearer token 原文泄漏给模型:\n%s", out)
	}
	if !strings.Contains(out, "Bearer ***") {
		t.Fatalf("应看到 Bearer *** 掩码:\n%s", out)
	}
	// raw_markdown 不返回：原文里独有的行（如「### 1.1」标题行）不允许出现在输出里
	if strings.Contains(out, "### 1.1") {
		t.Fatalf("raw_markdown 原文段被回传（R3）:\n%s", out)
	}
}

// ===================== ⑤ 补装迁移（AC7）=====================

func TestMigrateApiDocQuerySkill(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)

	// 前置：SeedDefaults 的出厂装配不含 apidoc_query，两个标记键都不存在
	a0, err := svc.store.GetAgent(agent.BuiltinOpsAgentID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	for _, n := range a0.Skills {
		if n == skillapidoc.SkillQuery {
			t.Fatalf("出厂播种不该已含 %s: %v", skillapidoc.SkillQuery, a0.Skills)
		}
	}
	// 新键必须独立于旧键：只跑本迁移不许写 seed.apidoc_skills_v1（D6 的坑）
	if _, ok, err := svc.store.GetSetting("seed.apidoc_skills_v1"); err != nil || ok {
		t.Fatalf("旧标记键不该被本迁移写入: ok=%v err=%v", ok, err)
	}

	if err := svc.MigrateApiDocQuerySkill(); err != nil {
		t.Fatalf("MigrateApiDocQuerySkill: %v", err)
	}
	a1, err := svc.store.GetAgent(agent.BuiltinOpsAgentID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if !stringIn(a1.Skills, skillapidoc.SkillQuery) {
		t.Fatalf("内置运维 Agent 应已补装 %s: %v", skillapidoc.SkillQuery, a1.Skills)
	}
	// 原有装配一个不丢（并集语义）
	for _, n := range a0.Skills {
		if !stringIn(a1.Skills, n) {
			t.Fatalf("补装丢掉了原有装配 %s: %v", n, a1.Skills)
		}
	}
	// 新标记键已写
	if v, ok, err := svc.store.GetSetting("seed.apidoc_query_v1"); err != nil || !ok || v == "" {
		t.Fatalf("标记键 seed.apidoc_query_v1 应已写入: ok=%v v=%q err=%v", ok, v, err)
	}

	// 幂等：跑两次不多插装配记录
	before := len(a1.Skills)
	if err := svc.MigrateApiDocQuerySkill(); err != nil {
		t.Fatalf("第二次 MigrateApiDocQuerySkill: %v", err)
	}
	a2, _ := svc.store.GetAgent(agent.BuiltinOpsAgentID)
	if len(a2.Skills) != before {
		t.Fatalf("补装必须幂等: %d → %d（%v）", before, len(a2.Skills), a2.Skills)
	}

	// 标记写过之后用户取消勾选 → 启动流程绝不复活
	if err := svc.store.ReplaceAgentSkills(agent.BuiltinOpsAgentID, []string{"ops_list_containers"}); err != nil {
		t.Fatalf("ReplaceAgentSkills: %v", err)
	}
	if err := svc.MigrateApiDocQuerySkill(); err != nil {
		t.Fatalf("第三次 MigrateApiDocQuerySkill: %v", err)
	}
	a3, _ := svc.store.GetAgent(agent.BuiltinOpsAgentID)
	if len(a3.Skills) != 1 || a3.Skills[0] != "ops_list_containers" {
		t.Fatalf("已补装过就不该再干预用户装配: %v", a3.Skills)
	}
}

// ===================== ⑥ 装配链路：ListSkills + Agent 可装配 =====================

func TestApiDocQueryListedAndAssemblable(t *testing.T) {
	svc := newTestService(t)
	attachApiDocSkills(t, svc)

	var found *SkillInfo
	skills := svc.ListSkills()
	for i := range skills {
		if skills[i].Name == skillapidoc.SkillQuery {
			found = &skills[i]
		}
	}
	if found == nil {
		t.Fatalf("ListSkills 里没有 apidoc_query（技能管理界面就看不到、装不上自建 Agent）")
	}
	if !found.Builtin || strings.TrimSpace(found.Description) == "" {
		t.Fatalf("apidoc_query 元数据异常: %+v", *found)
	}
	// 装配链路：agent 侧能取到该技能（照 registerProdSkills / SetSkills 先例）
	if err := svc.agents.SetSkills(agent.BuiltinOpsAgentID, []string{skillapidoc.SkillQuery}); err != nil {
		t.Fatalf("装配 apidoc_query 进内置 Agent: %v", err)
	}
	// 三件套原样在册且 schema 未动（回归红线）
	for _, n := range skillapidoc.SkillNames {
		if sk, ok := svc.skills.Get(n); !ok || strings.Contains(sk.Name(), "query") {
			t.Fatalf("既有技能缺失或漂移: %s", n)
		}
	}
}
