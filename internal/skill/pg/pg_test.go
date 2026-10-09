package pg

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"DevCraft/internal/skill"
)

// ==================== 测试替身 ====================

// fakeQuery 记录最近一次调用参数并返回预设结果的假 QueryFn
// （对照 deploy 测试注入假 StepRunner 的范式：技能包不碰真数据库）。
type fakeQuery struct {
	gotDb    string
	gotSQL   string
	gotArgs  []any
	gotLimit int
	result   string
	err      error
	calls    int
}

func (f *fakeQuery) fn(_ context.Context, database, query string, args []any, limit int) (string, error) {
	f.calls++
	f.gotDb, f.gotSQL, f.gotArgs, f.gotLimit = database, query, args, limit
	return f.result, f.err
}

// staticConfig 返回固定的白名单配置。
func staticConfig(dbs, schemas []string) ConfigFn {
	return func() Config { return Config{Databases: dbs, Schemas: schemas} }
}

// exec 调用技能的 Execute 并把参数打包成 JSON（模拟 LLM 生成的调用参数）。
func exec(t *testing.T, s interface {
	Execute(context.Context, json.RawMessage) (string, error)
}, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return s.Execute(context.Background(), raw)
}

// ==================== SQL 只读门禁 ====================

// TestCheckReadOnlySQLAccepts 必须放行的合法只读语句。
func TestCheckReadOnlySQLAccepts(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string // 期望的清理后 SQL
	}{
		{"plain select", "SELECT * FROM t", "SELECT * FROM t"},
		{"lowercase", "select id from t", "select id from t"},
		{"mixed case", "sElEcT 1", "sElEcT 1"},
		{"leading whitespace", "  \n\t SELECT 1", "SELECT 1"},
		{"trailing semicolon", "SELECT 1;", "SELECT 1"},
		{"trailing semicolon + space", "SELECT 1 ; ", "SELECT 1"},
		{"with CTE", "WITH x AS (SELECT 1) SELECT * FROM x", "WITH x AS (SELECT 1) SELECT * FROM x"},
		{"line comment before", "-- 查一下\nSELECT 1", "SELECT 1"},
		{"block comment before", "/* 注释 */ SELECT 1", "SELECT 1"},
		{"nested block comment", "/* a /* b */ c */ SELECT 1", "SELECT 1"},
		{"inline comment preserved sql", "SELECT 1 -- 尾注释", "SELECT 1"},
		{"semicolon inside string literal", `SELECT 'a;b'`, `SELECT 'a;b'`},
		{"comment markers inside string", `SELECT '--x', '/*y*/'`, `SELECT '--x', '/*y*/'`},
		{"escaped quote in literal", `SELECT 'it''s'`, `SELECT 'it''s'`},
		// 末尾分号后只剩注释 = 与 PG 词法一致的单语句（注释被清理后无实义内容）
		{"trailing comment after semicolon", "SELECT 1; -- x", "SELECT 1"},
		// 写关键字只出现在字面量里（掩码已挖空）不得误伤
		{"write keyword inside string literal", `SELECT 'DELETE FROM t' AS note`, `SELECT 'DELETE FROM t' AS note`},
		{"write keyword inside quoted identifier", `SELECT "delete" FROM t`, `SELECT "delete" FROM t`},
		{"identifier containing keyword", "SELECT * FROM order_updates", "SELECT * FROM order_updates"},
		// -- 后无空格的行注释同样按注释处理（PG 词法：-- 起注释不要求空格）
		{"line comment without space", "SELECT 1--c", "SELECT 1"},
		{"mixed case with CTE", "WiTh x As (SeLeCt 1) SeLeCt * FrOm x", "WiTh x As (SeLeCt 1) SeLeCt * FrOm x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := checkReadOnlySQL(c.sql)
			if err != nil {
				t.Fatalf("checkReadOnlySQL(%q) 意外拒绝: %v", c.sql, err)
			}
			if got != c.want {
				t.Fatalf("checkReadOnlySQL(%q) = %q, want %q", c.sql, got, c.want)
			}
		})
	}
}

// TestCheckReadOnlySQLRejects 必须拒绝的危险/非法语句（含注释绕过与多语句注入）。
func TestCheckReadOnlySQLRejects(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		wantSub string // 错误信息里应包含的关键词（中文说明）
	}{
		{"delete", "DELETE FROM t", "只允许执行以 SELECT 或 WITH 开头"},
		{"update", "UPDATE t SET a = 1", "只允许执行以 SELECT 或 WITH 开头"},
		{"insert", "INSERT INTO t VALUES (1)", "只允许执行以 SELECT 或 WITH 开头"},
		{"drop", "DROP TABLE t", "只允许执行以 SELECT 或 WITH 开头"},
		{"truncate", "TRUNCATE t", "只允许执行以 SELECT 或 WITH 开头"},
		{"ddl create", "CREATE TABLE x (id int)", "只允许执行以 SELECT 或 WITH 开头"},
		{"comment bypass delete", "/* 无害注释 */ DELETE FROM t", "只允许执行以 SELECT 或 WITH 开头"},
		{"line comment bypass", "-- c\nDROP TABLE t", "只允许执行以 SELECT 或 WITH 开头"},
		{"multi statement", "SELECT 1; DELETE FROM t", "单条 SQL 语句"},
		{"multi statement trailing content", "SELECT 1; SELECT 2;", "单条 SQL 语句"},
		{"double semicolon", "SELECT 1;;", "单条 SQL 语句"},
		{"statement after semicolon", "SELECT 1; x", "单条 SQL 语句"},
		{"empty", "   ", "不能为空"},
		{"only comment", "-- nothing", "不能为空"},
		{"only semicolon", ";", "不能为空"},
		{"unterminated string", "SELECT 'abc", "未闭合的引号"},
		{"unterminated block comment", "SELECT 1 /* oops", "未闭合的块注释"},
		{"prefix disguise", "SELECTX 1", "只允许执行以 SELECT 或 WITH 开头"},
		// ---- 写路径绕过（前缀合法也必须拦下）----
		{"data-modifying CTE delete", "WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", "写操作关键字"},
		{"data-modifying CTE update", "with u as (update t set a = 1 returning *) select * from u", "写操作关键字"},
		{"data-modifying CTE insert", "WITH i AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM i", "写操作关键字"},
		{"select into table", "SELECT * INTO newtab FROM t", "写操作关键字"},
		// CTE 写关键字藏在注释后/大小写混合，掩码扫描同样命中
		{"CTE delete after comment", "/* ok */ WITH d AS (DELETE FROM t) SELECT 1", "写操作关键字"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := checkReadOnlySQL(c.sql)
			if err == nil {
				t.Fatalf("checkReadOnlySQL(%q) 意外放行", c.sql)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("err = %q, want containing %q", err.Error(), c.wantSub)
			}
		})
	}
}

// TestCheckReadOnlySQLBypassAttempts 以攻击者视角构造的绕过用例集中回归：
// 每条都必须被拒绝（fail-closed），绝不依赖"服务端 read-only 兜底"才安全。
// 兜底依然存在（dbx.DialPG 在任何查询前 SET default_transaction_read_only = on，
// 设置失败即断开），但门禁层自己必须站住第一道岗。
func TestCheckReadOnlySQLBypassAttempts(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		// MySQL 风格可执行注释 /*! ... */：PG 只当普通块注释，清理后前缀是 DELETE
		{"mysql executable comment", "/*! DELETE FROM t */"},
		{"mysql comment then delete", "/*!x*/ DELETE FROM t"},
		// 注释夹在前缀与写操作之间
		{"comment split prefix", "SEL/**/ECT 1; DROP TABLE t"},
		// 未闭合注释：看不懂的语句一律拒绝
		{"unterminated comment with payload", "SELECT 1 /* ; DELETE FROM t"},
		// 前导括号绕过前缀正则：宁可多拒（PG 里 (SELECT 1) 合法，但门禁 fail-closed）
		{"leading paren", "(SELECT 1)"},
		{"leading paren union", "(SELECT 1) UNION SELECT 2"},
		// dollar-quoting 不识别 → 内容按代码扫描/分号按语句分隔符统计，只会多拒
		{"dollar quoted semicolons", `SELECT $$a; DELETE FROM t; b$$`},
		{"tagged dollar quoted delete", `SELECT $tag$DELETE FROM t$tag$`},
		// 字符串字面量伪装成前缀：真正的语句是后面的 DELETE
		{"string prefix disguise", `'SELECT' ; DELETE FROM t`},
		// 分号后跟注释再跟第二条语句
		{"second statement after comment", "SELECT 1; -- innocent\nDELETE FROM t"},
		// E-string 反斜杠转义分歧：扫描器在 \' 处闭串后看到引号外分号 → 多语句拒绝
		{"escape string divergence", `SELECT E'a\'; DELETE FROM t; --'`},
		// 大小写混合的写 CTE
		{"mixed case write CTE", "WiTh d As (DeLeTe FrOm t ReTuRnInG *) SeLeCt * FrOm d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := checkReadOnlySQL(c.sql); err == nil {
				t.Fatalf("checkReadOnlySQL(%q) 意外放行: %q", c.sql, got)
			}
		})
	}
}

// TestQueryGateBlocksWriteCTE 端到端确认：data-modifying CTE 在 Execute 层
// 就被拒绝，执行器零触达（不依赖连接层 read-only 兜底才有安全）。
func TestQueryGateBlocksWriteCTE(t *testing.T) {
	q := &fakeQuery{result: "x"}
	s := newQuerySkill(staticConfig([]string{"shop"}, nil), q)
	_, err := exec(t, s, map[string]any{
		"db":  "shop",
		"sql": "WITH d AS (DELETE FROM users RETURNING *) SELECT * FROM d",
	})
	if err == nil || !strings.Contains(err.Error(), "写操作") {
		t.Fatalf("err = %v, want 写操作关键字拒绝", err)
	}
	if q.calls != 0 {
		t.Fatal("写 CTE 触达了执行器（必须门禁拦截，而非依赖 read-only 兜底）")
	}
}

// ==================== pg_query：白名单 / limit 钳制 / 输出封顶 ====================

func newQuerySkill(cfg ConfigFn, q *fakeQuery) *queryData {
	return &queryData{base: base{cfg: cfg, query: q.fn}}
}

// TestQueryWhitelist db 不在关注列表内必须拒绝（中文说明含可用列表），且绝不触达执行器。
func TestQueryWhitelist(t *testing.T) {
	q := &fakeQuery{result: "（0 行）"}
	s := newQuerySkill(staticConfig([]string{"shop", "crm"}, []string{"public"}), q)

	_, err := exec(t, s, map[string]any{"db": "secret_db", "sql": "SELECT 1"})
	if err == nil {
		t.Fatal("越界 db 意外放行")
	}
	if !strings.Contains(err.Error(), "不在关注列表内") || !strings.Contains(err.Error(), "shop, crm") {
		t.Fatalf("err = %q, want whitelist wording with available dbs", err.Error())
	}
	if q.calls != 0 {
		t.Fatalf("越界请求触达了执行器（calls=%d），门禁必须在执行前拦截", q.calls)
	}

	// 未配置任何关注库：提示先配置
	s2 := newQuerySkill(staticConfig(nil, nil), &fakeQuery{})
	_, err = exec(t, s2, map[string]any{"db": "shop", "sql": "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), "尚未配置关注数据库") {
		t.Fatalf("err = %v, want 尚未配置关注数据库", err)
	}

	// 缺 db 参数
	_, err = exec(t, s, map[string]any{"sql": "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), "需要提供 db") {
		t.Fatalf("err = %v, want missing-db wording", err)
	}
}

// TestQueryLimitClamp limit 缺省 200、上限 1000、正常值透传。
func TestQueryLimitClamp(t *testing.T) {
	cases := []struct {
		name  string
		limit any // nil = 不传该参数
		want  int
	}{
		{"omitted", nil, DefaultRowLimit},
		{"zero", 0, DefaultRowLimit},
		{"negative", -5, DefaultRowLimit},
		{"normal", 50, 50},
		{"over max", 5000, MaxRowLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQuery{result: "（0 行）"}
			s := newQuerySkill(staticConfig([]string{"shop"}, nil), q)
			args := map[string]any{"db": "shop", "sql": "SELECT 1"}
			if c.limit != nil {
				args["limit"] = c.limit
			}
			if _, err := exec(t, s, args); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if q.gotLimit != c.want {
				t.Fatalf("limit = %d, want %d", q.gotLimit, c.want)
			}
		})
	}
}

// TestQueryGateBeforeExecution 非只读 SQL 在执行前被拦截（执行器零调用）。
func TestQueryGateBeforeExecution(t *testing.T) {
	q := &fakeQuery{result: "x"}
	s := newQuerySkill(staticConfig([]string{"shop"}, nil), q)
	_, err := exec(t, s, map[string]any{"db": "shop", "sql": "SELECT 1; DROP TABLE users"})
	if err == nil || !strings.Contains(err.Error(), "单条 SQL 语句") {
		t.Fatalf("err = %v, want multi-statement rejection", err)
	}
	if q.calls != 0 {
		t.Fatal("危险 SQL 触达了执行器")
	}
}

// TestQueryOutputCap 输出字符总量封顶（≈ ops 的 MaxLogChars），截断在尾部注明。
func TestQueryOutputCap(t *testing.T) {
	q := &fakeQuery{result: strings.Repeat("数", MaxOutputChars+5000)}
	s := newQuerySkill(staticConfig([]string{"shop"}, nil), q)
	out, err := exec(t, s, map[string]any{"db": "shop", "sql": "SELECT 1"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len([]rune(out)) > MaxOutputChars+200 { // 200 = 头部说明与截断标注的余量
		t.Fatalf("输出未封顶: %d runes", len([]rune(out)))
	}
	if !strings.Contains(out, "截断") {
		t.Fatal("截断时未在结果中注明")
	}
}

// ==================== pg_list_tables / pg_describe_table ====================

// TestListTablesSchemaWhitelist schema 参数（如提供）必须在关注 schema 列表内；
// 未提供 schema 时不加过滤条件（列出全部非系统 schema）。
func TestListTablesSchemaWhitelist(t *testing.T) {
	q := &fakeQuery{result: " schema | name\n"}
	s := &listTables{base: base{cfg: staticConfig([]string{"shop"}, []string{"public", "app"}), query: q.fn}}

	// 越界 schema 拒绝且不触达执行器
	_, err := exec(t, s, map[string]any{"db": "shop", "schema": "pg_hack"})
	if err == nil || !strings.Contains(err.Error(), "不在关注列表内") {
		t.Fatalf("err = %v, want schema whitelist rejection", err)
	}
	if q.calls != 0 {
		t.Fatal("越界 schema 触达了执行器")
	}

	// 合法 schema：参数化传入（$1），绝不拼接进 SQL 文本
	if _, err := exec(t, s, map[string]any{"db": "shop", "schema": "app"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(q.gotSQL, "n.nspname = $1") {
		t.Fatalf("schema 过滤未参数化: %s", q.gotSQL)
	}
	if len(q.gotArgs) != 1 || q.gotArgs[0] != "app" {
		t.Fatalf("args = %v, want [app]", q.gotArgs)
	}
	if q.gotDb != "shop" || q.gotLimit != MaxRowLimit {
		t.Fatalf("db = %q, limit = %d", q.gotDb, q.gotLimit)
	}

	// 未提供 schema：SQL 不带过滤条件
	q2 := &fakeQuery{result: "x"}
	s2 := &listTables{base: base{cfg: staticConfig([]string{"shop"}, nil), query: q2.fn}}
	if _, err := exec(t, s2, map[string]any{"db": "shop"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(q2.gotSQL, "$1") || len(q2.gotArgs) != 0 {
		t.Fatalf("未提供 schema 却生成了过滤参数: %s %v", q2.gotSQL, q2.gotArgs)
	}
}

// TestDescribeTableValidation schema 必填、table 必填、空结果给可行动提示。
func TestDescribeTableValidation(t *testing.T) {
	cfg := staticConfig([]string{"shop"}, []string{"public"})

	// schema 参数在关注列表之外 → 拒绝（即使配置了 schema 白名单）
	q := &fakeQuery{}
	s := &describeTable{base: base{cfg: cfg, query: q.fn}}
	_, err := exec(t, s, map[string]any{"db": "shop", "schema": "other", "table": "t"})
	if err == nil || !strings.Contains(err.Error(), "不在关注列表内") {
		t.Fatalf("err = %v, want schema whitelist rejection", err)
	}

	// 缺 schema → 中文提示必填（describe 需要精确定位表）
	_, err = exec(t, s, map[string]any{"db": "shop", "table": "t"})
	if err == nil || !strings.Contains(err.Error(), "需要提供 schema") {
		t.Fatalf("err = %v, want missing-schema wording", err)
	}
	// 缺 table
	_, err = exec(t, s, map[string]any{"db": "shop", "schema": "public"})
	if err == nil || !strings.Contains(err.Error(), "需要提供 table") {
		t.Fatalf("err = %v, want missing-table wording", err)
	}

	// 空结果 → 提示表可能不存在（而不是回一张空表格让 LLM 猜）
	q2 := &fakeQuery{result: " column_name | data_type\n---+---\n（0 行）"}
	s2 := &describeTable{base: base{cfg: cfg, query: q2.fn}}
	out, err := exec(t, s2, map[string]any{"db": "shop", "schema": "public", "table": "ghost"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "未找到表") || !strings.Contains(out, "public.ghost") {
		t.Fatalf("空结果提示不符: %q", out)
	}
	if len(q2.gotArgs) != 2 || q2.gotArgs[0] != "public" || q2.gotArgs[1] != "ghost" {
		t.Fatalf("args = %v, want [public ghost]（schema/table 必须参数化）", q2.gotArgs)
	}
}

// TestRegisterAndMetadata 注册成功 + 元数据契约（技能名带命名空间、
// Schema 是合法 JSON、Description 含路由场景说明）。
func TestRegisterAndMetadata(t *testing.T) {
	reg := skill.NewRegistry()
	q := &fakeQuery{}
	if err := Register(reg, staticConfig([]string{"shop"}, []string{"public"}), q.fn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range SkillNames {
		s, ok := reg.Get(name)
		if !ok {
			t.Fatalf("技能 %s 未注册", name)
		}
		if !json.Valid(s.Parameters()) {
			t.Fatalf("技能 %s 的参数 Schema 不是合法 JSON", name)
		}
		if d := s.Description(); !strings.Contains(d, "PostgreSQL") {
			t.Fatalf("技能 %s 的 Description 缺少路由场景说明: %q", name, d)
		}
	}
}
