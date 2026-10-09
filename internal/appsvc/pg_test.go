package appsvc

import (
	"context"
	"slices"
	"strings"
	"testing"

	"DevCraft/internal/dbx"
	"DevCraft/internal/skill/pg"
)

// ==================== PG 设置：加密落库 / 视图不回传明文 / 留空不覆盖 ====================

// TestPgSettingsRoundtripEncryption 保存→读取的完整回路：
// 密码 AES-GCM 密文落盘（库里绝无明文）、视图只回传 passwordSet、
// 白名单 JSON 数组文本往返一致、端口归一化。
func TestPgSettingsRoundtripEncryption(t *testing.T) {
	svc := newTestService(t)
	in := PgSettings{
		Host: "10.0.0.8", Port: 0, // 未填端口 → 归一化 5432
		User: "reporter", Password: "pg-secret-123",
		Databases: []string{"shop", " crm ", ""}, // 含待归一化项：去空白、丢空项
		Schemas:   []string{"public", "app"},
	}
	if err := svc.SavePgSettings(in); err != nil {
		t.Fatalf("SavePgSettings: %v", err)
	}

	// 落库的密码必须是密文（≠明文，且可被 Box 解密还原）
	enc, ok, err := svc.store.GetSetting(settingPgPassword)
	if err != nil || !ok || enc == "" {
		t.Fatalf("密码未落库: ok=%v err=%v", ok, err)
	}
	if strings.Contains(enc, "pg-secret-123") {
		t.Fatal("密码疑似明文落库")
	}
	if plain, err := svc.box.Decrypt(enc); err != nil || plain != "pg-secret-123" {
		t.Fatalf("密文解密还原失败: plain=%q err=%v", plain, err)
	}

	view, err := svc.GetPgSettings()
	if err != nil {
		t.Fatalf("GetPgSettings: %v", err)
	}
	if view.Host != "10.0.0.8" || view.Port != 5432 || view.User != "reporter" {
		t.Fatalf("基础字段往返不一致: %+v", view)
	}
	if !view.PasswordSet {
		t.Fatal("PasswordSet = false, want true")
	}
	if !slices.Equal(view.Databases, []string{"shop", "crm"}) {
		t.Fatalf("Databases = %v, want [shop crm]（去空白丢空项）", view.Databases)
	}
	if !slices.Equal(view.Schemas, []string{"public", "app"}) {
		t.Fatalf("Schemas = %v, want [public app]", view.Schemas)
	}
}

// TestPgSettingsBlankPasswordKeepsOld 留空密码 = 不修改（与 apiKey 同款语义）：
// 第二次保存只改主机、密码留空，旧密码必须原样保留。
func TestPgSettingsBlankPasswordKeepsOld(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SavePgSettings(PgSettings{Host: "h1", Port: 5432, User: "u", Password: "old-pass"}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := svc.SavePgSettings(PgSettings{Host: "h2", Port: 5433, User: "u2", Password: ""}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if got := svc.pgPassword(); got != "old-pass" {
		t.Fatalf("留空保存后密码 = %q, want old-pass（不得被覆盖）", got)
	}
	view, err := svc.GetPgSettings()
	if err != nil {
		t.Fatalf("GetPgSettings: %v", err)
	}
	if view.Host != "h2" || view.Port != 5433 || view.User != "u2" || !view.PasswordSet {
		t.Fatalf("其他字段应更新而密码保持已设置: %+v", view)
	}
}

// ==================== TestPg：表单值优先 / 留空回退已存密码 ====================

// withFakePgProbe 替换试连实现并返回捕获器（测试结束自动还原）。
func withFakePgProbe(t *testing.T) *probeCapture {
	t.Helper()
	pc := &probeCapture{}
	old := pgProbe
	pgProbe = func(_ context.Context, target dbx.PGTarget, database string) error {
		pc.target, pc.database, pc.calls = target, database, pc.calls+1
		return pc.err
	}
	t.Cleanup(func() { pgProbe = old })
	return pc
}

type probeCapture struct {
	target   dbx.PGTarget
	database string
	calls    int
	err      error
}

// TestTestPgParameterAssembly 参数组装三规则：
// ① 表单值直接生效（未保存也能测，对照 TestSSH）② 密码留空回退已存密码
// ③ 试连库取关注列表第一个，未配置则 postgres。
func TestTestPgParameterAssembly(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SavePgSettings(PgSettings{
		Host: "saved-host", Port: 5432, User: "saved-user", Password: "stored-pass",
		Databases: []string{"shop", "crm"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// ① + ③：表单值优先（含显式密码），库取白名单第一个
	pc := withFakePgProbe(t)
	if err := svc.TestPg(context.Background(), "form-host", 6543, "form-user", "form-pass"); err != nil {
		t.Fatalf("TestPg: %v", err)
	}
	if pc.target.Host != "form-host" || pc.target.Port != 6543 ||
		pc.target.User != "form-user" || pc.target.Password != "form-pass" {
		t.Fatalf("表单值未直接生效: %+v", pc.target)
	}
	if pc.database != "shop" {
		t.Fatalf("试连库 = %q, want shop（关注列表第一个）", pc.database)
	}

	// ②：密码留空 → 回退已存密码（解密后传给试连）
	pc2 := withFakePgProbe(t)
	if err := svc.TestPg(context.Background(), "form-host", 0, "form-user", ""); err != nil {
		t.Fatalf("TestPg: %v", err)
	}
	if pc2.target.Password != "stored-pass" {
		t.Fatalf("留空密码未回退已存密码: %q", pc2.target.Password)
	}
	if pc2.target.Port != 5432 {
		t.Fatalf("端口 0 未归一化为 5432: %d", pc2.target.Port)
	}
}

// TestTestPgValidation 表单校验：缺主机/缺用户名直接中文报错，绝不发起试连。
func TestTestPgValidation(t *testing.T) {
	svc := newTestService(t)
	pc := withFakePgProbe(t)

	err := svc.TestPg(context.Background(), "  ", 5432, "u", "p")
	if err == nil || !strings.Contains(err.Error(), "主机") {
		t.Fatalf("缺主机 err = %v, want 主机提示", err)
	}
	err = svc.TestPg(context.Background(), "h", 5432, "", "p")
	if err == nil || !strings.Contains(err.Error(), "用户名") {
		t.Fatalf("缺用户名 err = %v, want 用户名提示", err)
	}
	if pc.calls != 0 {
		t.Fatal("校验失败仍发起了试连")
	}
}

// ==================== 技能侧回调：白名单实时读取 ====================

// TestPgSkillConfigLive PgSkillConfig 每次实时读库（改设置立即生效，无缓存快照）。
func TestPgSkillConfigLive(t *testing.T) {
	svc := newTestService(t)
	if cfg := svc.PgSkillConfig(); len(cfg.Databases) != 0 {
		t.Fatalf("初始白名单应为空: %+v", cfg)
	}
	if err := svc.SavePgSettings(PgSettings{Host: "h", User: "u",
		Databases: []string{"shop"}, Schemas: []string{"public"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	cfg := svc.PgSkillConfig()
	if !slices.Equal(cfg.Databases, []string{"shop"}) || !slices.Equal(cfg.Schemas, []string{"public"}) {
		t.Fatalf("保存后配置未即时生效: %+v", cfg)
	}
}

// TestPgQueryWithoutConfig 未配置连接信息时，执行器返回中文引导错误（不 panic）。
func TestPgQueryWithoutConfig(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.PgQuery(context.Background(), "shop", "SELECT 1", nil, 10)
	if err == nil || !strings.Contains(err.Error(), "数据库配置") {
		t.Fatalf("err = %v, want 配置引导提示", err)
	}
}

// ==================== 一次性自动补装迁移 ====================

// TestMigratePgSkills 补装迁移三条纪律：
// ① 首次执行把 3 个 pg 技能并集进运维 Agent（不丢已有装配）
// ② 重复执行幂等（标记生效，不重复追加）
// ③ 标记写入后用户取消装配绝不会被"复活"（补装是一次性的，用户数据至上）。
func TestMigratePgSkills(t *testing.T) {
	svc := newTestService(t)
	before, err := svc.store.GetAgent("ops")
	if err != nil {
		t.Fatalf("get ops agent: %v", err)
	}
	// 前置：SeedDefaults 播种的装配里没有 pg 技能（存量安装的状态）
	for _, n := range pg.SkillNames {
		if slices.Contains(before.Skills, n) {
			t.Fatalf("播种装配里不应预置 %s", n)
		}
	}

	// ① 首次迁移：union 追加
	if err := svc.MigratePgSkills(); err != nil {
		t.Fatalf("MigratePgSkills: %v", err)
	}
	after, _ := svc.store.GetAgent("ops")
	for _, n := range before.Skills {
		if !slices.Contains(after.Skills, n) {
			t.Fatalf("原有装配 %s 被迁移丢掉了", n)
		}
	}
	for _, n := range pg.SkillNames {
		if !slices.Contains(after.Skills, n) {
			t.Fatalf("迁移未补装 %s: %v", n, after.Skills)
		}
	}

	// ② 重复执行：装配不变（无重复项）
	if err := svc.MigratePgSkills(); err != nil {
		t.Fatalf("second MigratePgSkills: %v", err)
	}
	again, _ := svc.store.GetAgent("ops")
	if len(again.Skills) != len(after.Skills) {
		t.Fatalf("重复迁移改变了装配: %v → %v", after.Skills, again.Skills)
	}

	// ③ 用户取消装配后再次迁移：绝不复活（标记已写）
	kept := slices.DeleteFunc(slices.Clone(again.Skills), func(n string) bool { return n == pg.SkillQuery })
	if err := svc.store.ReplaceAgentSkills("ops", kept); err != nil {
		t.Fatalf("replace skills: %v", err)
	}
	if err := svc.MigratePgSkills(); err != nil {
		t.Fatalf("third MigratePgSkills: %v", err)
	}
	final, _ := svc.store.GetAgent("ops")
	if slices.Contains(final.Skills, pg.SkillQuery) {
		t.Fatalf("用户已取消的技能被迁移复活: %v", final.Skills)
	}
}

// TestMigratePgSkillsPreservesCustomAssembly 并集语义：迁移前用户自装的
// 任何技能名都原样保留（union 而非替换）。
func TestMigratePgSkillsPreservesCustomAssembly(t *testing.T) {
	svc := newTestService(t)
	if err := svc.store.ReplaceAgentSkills("ops", []string{"my_custom_skill"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := svc.MigratePgSkills(); err != nil {
		t.Fatalf("MigratePgSkills: %v", err)
	}
	row, _ := svc.store.GetAgent("ops")
	want := append([]string{"my_custom_skill"}, pg.SkillNames...)
	// store.agentSkills 按 skill_name 排序返回（ORDER BY），比较前对期望值同样排序
	slices.Sort(want)
	if !slices.Equal(row.Skills, want) {
		t.Fatalf("skills = %v, want %v", row.Skills, want)
	}
}
