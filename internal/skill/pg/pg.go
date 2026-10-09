// Package pg 实现 PostgreSQL 查询域的 3 个内置技能：
// pg_list_tables / pg_describe_table / pg_query。
//
// 安全模型（三层防护，全部在本包内强制，不依赖调用方自觉）：
//  1. SQL 门禁：去注释/前导空白后必须以 SELECT 或 WITH 开头（大小写不敏感），
//     必须是单语句（分号后仍有内容即拒绝），且字面量挖空后的代码骨架里
//     不得出现写关键字（拦截 WITH 内藏数据修改 CTE 与 SELECT INTO 建表）
//     ——见 checkReadOnlySQL；
//  2. 白名单：db 必须在用户配置的"关注数据库"内，schema 参数（如提供）
//     必须在"关注 schema"内，越界一律中文拒绝；
//  3. 体量防护：行数截断到 limit（默认 200 / 上限 1000）+ 输出字符总量
//     封顶（对照 ops.analyze_logs 的 MaxLogChars 范式）。
//     连接层还有第二道只读保险（dbx 的 SET default_transaction_read_only）。
//
// 依赖注入（对照 ops.EndpointFn / deploy.StepRunner 范式）：
// 技能不直接连库——ConfigFn 实时读白名单配置（改设置立即生效），
// QueryFn 执行查询（真实实现是 appsvc → dbx；单测注入假实现）。
package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"DevCraft/internal/sanitize" // 输出字符总量封顶（与 ops 共用同一套防护工具）
	"DevCraft/internal/skill"    // Skill 接口与注册表
)

// 技能名常量（带 pg_ 命名空间；appsvc 的补装迁移也引用它们）。
const (
	SkillListTables    = "pg_list_tables"
	SkillDescribeTable = "pg_describe_table"
	SkillQuery         = "pg_query"
)

// SkillNames 全部 pg 技能名（补装迁移用它做并集）。
var SkillNames = []string{SkillListTables, SkillDescribeTable, SkillQuery}

// 查询防护参数（对照 ops 的 DefaultLogLines/MaxLogLines/MaxLogChars 三件套）。
const (
	DefaultRowLimit = 200   // pg_query 未指定 limit 时的默认行数上限
	MaxRowLimit     = 1000  // limit 的硬上限（防御模型乱传大数字）
	MaxOutputChars  = 20000 // 输出字符总量上限（控制 token 成本的兜底）
)

// Config 技能可见的 PG 配置子集（白名单）。连接凭据不在这里——
// 技能不需要知道密码，执行查询由注入的 QueryFn 负责（最小知识原则）。
type Config struct {
	Databases []string // 关注数据库白名单（pg.databases）
	Schemas   []string // 关注 schema 白名单（pg.schemas）
}

// ConfigFn 实时读取当前配置（≈ ops.EndpointFn：用回调而非快照，改设置立即生效）。
type ConfigFn func() Config

// QueryFn 在指定数据库上执行一条只读查询并返回渲染好的文本表格。
// 真实实现：appsvc.Service.PgQuery → dbx.RunQuery（连接层强制只读 + 30s 超时）。
// 抽象成函数类型是为了单测可注入（对照 deploy 的 StepRunner 范式）。
type QueryFn func(ctx context.Context, database, query string, args []any, limit int) (string, error)

// Register 把 3 个 PG 技能挂载到注册表（注册范式同 ops.Register）。
func Register(reg *skill.Registry, cfg ConfigFn, query QueryFn) error {
	b := base{cfg: cfg, query: query}
	for _, s := range []skill.Skill{
		&listTables{base: b}, // &struct{} = 创建并取指针（≈ new）
		&describeTable{base: b},
		&queryData{base: b},
	} {
		if err := reg.Register(s); err != nil {
			return err
		}
	}
	return nil
}

// base 三个技能共有的依赖与白名单校验逻辑（组合复用，≈ Java 抽象基类的字段部分）。
type base struct {
	cfg   ConfigFn
	query QueryFn
}

// checkDb 校验 db 参数：非空 + 必须在关注数据库白名单内。
// 越界错误列出可用库名，帮 LLM 自纠（对照 deploy 技能"未找到流程时列出可用名字"）。
func (b base) checkDb(db string) (string, error) {
	db = strings.TrimSpace(db)
	if db == "" {
		return "", fmt.Errorf("参数错误: 需要提供 db（数据库名）")
	}
	c := b.cfg()
	if len(c.Databases) == 0 {
		return "", fmt.Errorf("尚未配置关注数据库，无法查询 %q：请引导用户在设置 →「数据库配置」中添加", db)
	}
	for _, d := range c.Databases {
		if d == db {
			return db, nil
		}
	}
	return "", fmt.Errorf("数据库 %q 不在关注列表内（可用: %s），已拒绝执行", db, strings.Join(c.Databases, ", "))
}

// checkSchema 校验可选的 schema 参数。
// 返回 (schema, 是否提供, 错误)：未提供不报错（调用方自行决定 schema 是否必填）。
func (b base) checkSchema(schema string) (string, bool, error) {
	schema = strings.TrimSpace(schema)
	if schema == "" {
		return "", false, nil
	}
	c := b.cfg()
	if len(c.Schemas) == 0 {
		return "", false, fmt.Errorf("schema %q 不在关注列表内（当前未配置任何关注 schema），已拒绝执行：请引导用户在设置 →「数据库配置」中添加", schema)
	}
	for _, s := range c.Schemas {
		if s == schema {
			return schema, true, nil
		}
	}
	return "", false, fmt.Errorf("schema %q 不在关注列表内（可用: %s），已拒绝执行", schema, strings.Join(c.Schemas, ", "))
}

// whitelistHint 动态拼进 Description 的白名单说明（对照 deploy 技能的动态流程清单：
// Description 每回合实时读取，配置变更下一轮对话即生效，帮 LLM 传对参数）。
func (b base) whitelistHint() string {
	c := b.cfg()
	var parts []string
	if len(c.Databases) > 0 {
		parts = append(parts, "关注数据库: "+strings.Join(c.Databases, ", "))
	} else {
		parts = append(parts, "关注数据库: （尚未配置——调用会被拒绝，请引导用户先在设置 →「数据库配置」中添加）")
	}
	if len(c.Schemas) > 0 {
		parts = append(parts, "关注 schema: "+strings.Join(c.Schemas, ", "))
	}
	return "\n当前配置——" + strings.Join(parts, "；") + "。"
}

// ==================== SQL 只读门禁 ====================

// reReadOnlyPrefix 前缀门禁正则：必须以 SELECT 或 WITH 开头（(?i) 忽略大小写，
// \b 单词边界防止 "SELECTX" 这类前缀伪装）。包级 var + MustCompile：只编译一次。
var reReadOnlyPrefix = regexp.MustCompile(`(?i)^(select|with)\b`)

// reWriteKeyword 写操作关键字门禁：前缀合法（SELECT/WITH）的语句里仍可能藏写路径——
// PG 的"数据修改 CTE"（WITH d AS (DELETE ... RETURNING *) SELECT ...）与
// SELECT ... INTO new_table（建表写数据）都能通过前缀校验。这里在"字面量已挖空"
// 的掩码文本上再扫一遍关键字，把这两类写路径也在门禁层拒掉（第三道防线，
// 连接层的 read-only 事务仍是最终兜底——nextval()/lo_*() 这类副作用函数
// 无法穷举关键字，由 read-only 事务在服务端拒绝）。
// 关键字都选 PG 保留字（不可能作为裸标识符出现）；带引号标识符如 "delete"
// 与字符串字面量如 'DELETE' 已被掩码挖空，不会误伤。
var reWriteKeyword = regexp.MustCompile(`(?i)\b(insert|update|delete|returning|into)\b`)

// checkReadOnlySQL 只读门禁的完整校验，返回"可安全执行的清理后 SQL"。
// 步骤：① 扫描去掉 -- 与 /* */ 注释（字符串字面量原样保留）并统计引号外的分号，
// 同时产出"字面量挖空"的掩码文本 ② 分号只允许出现在末尾一个（多语句拒绝）
// ③ 去注释后必须以 SELECT/WITH 开头 ④ 掩码文本里不得出现写操作关键字
// （拦截 data-modifying CTE 与 SELECT INTO）。
// 拒绝时返回中文说明（会作为工具结果回填给 LLM，让它向用户解释）。
func checkReadOnlySQL(sql string) (string, error) {
	cleaned, masked, semis, err := stripComments(sql)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(cleaned)
	if trimmed == "" {
		return "", fmt.Errorf("SQL 不能为空")
	}
	// 多语句门禁：引号外出现 2 个以上分号，或唯一分号不在末尾（分号后还有内容）
	if semis > 1 {
		return "", fmt.Errorf("只允许执行单条 SQL 语句（检测到 %d 个分号分隔的多条语句），已拒绝执行", semis)
	}
	if semis == 1 && !strings.HasSuffix(trimmed, ";") {
		return "", fmt.Errorf("只允许执行单条 SQL 语句（分号后仍有内容），已拒绝执行")
	}
	trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, ";"))
	if trimmed == "" {
		return "", fmt.Errorf("SQL 不能为空")
	}
	if !reReadOnlyPrefix.MatchString(trimmed) {
		return "", fmt.Errorf("只允许执行以 SELECT 或 WITH 开头的只读查询，该语句已被拒绝（写操作/DDL 一律禁止）")
	}
	// 写关键字门禁：在掩码文本（字面量已挖空）上扫描，防止 WITH 内藏
	// DELETE/UPDATE/INSERT ... RETURNING 的数据修改 CTE，以及 SELECT INTO 建表
	if reWriteKeyword.MatchString(masked) {
		return "", fmt.Errorf("语句中出现写操作关键字（INSERT/UPDATE/DELETE/RETURNING/INTO）：" +
			"WITH 内藏数据修改 CTE 与 SELECT INTO 建表均属写操作，已拒绝执行")
	}
	return trimmed, nil
}

// stripComments 单遍扫描 SQL：注释替换为一个空格（保证 token 分隔），
// 字符串/标识符字面量原样保留，统计"引号外"的分号个数；
// 同时产出掩码文本 masked——字面量与注释的内容全部挖空成空格，只留代码骨架，
// 供写关键字扫描用（'DELETE' 字符串字面量、"delete" 引号标识符都不会误伤）。
//
// 为什么不能用正则简单替换注释：`SELECT '--不是注释'` 里的 -- 在字符串字面量内，
// 正则替换会把它误当注释吃掉，导致门禁判断与 PG 真实解析不一致（安全大忌）。
// 状态机扫描与 PG 词法规则对齐：单引号串支持 ” 转义，块注释支持嵌套。
//
// dollar-quoting（$$...$$ / $tag$...$tag$）刻意不识别：其内容会被当成普通代码
// 扫描——方向上只会"多拒"（fail-closed），绝不会把服务端视为代码的东西当字面量
// 放行。同理 E'...'（服务端把 \' 当转义、字符串只会更长）与未闭合注释/引号，
// 都只会导致更严格的拒绝，不存在"门禁放行但服务端解析成写操作"的分歧方向。
//
// 返回 (清理后的 SQL, 掩码文本, 引号外分号数, 错误)。引号/注释未闭合视为非法
// SQL 直接拒绝——宁可多拒（fail-closed），绝不放行看不懂的语句。
func stripComments(sql string) (cleaned, masked string, semis int, err error) {
	r := []rune(sql)
	n := len(r)
	var b strings.Builder // cleaned：注释→空格，字面量原样保留
	var m strings.Builder // masked：注释与字面量整体挖空，只留代码骨架
	i := 0
	for i < n {
		c := r[i]
		switch {
		case c == '-' && i+1 < n && r[i+1] == '-':
			// 行注释：跳到行尾（换行符保留给外层，天然分隔 token）
			for i < n && r[i] != '\n' {
				i++
			}
			b.WriteRune(' ')
			m.WriteRune(' ')
		case c == '/' && i+1 < n && r[i+1] == '*':
			// 块注释：PG 支持 /* /* 嵌套 */ */，用深度计数。
			// MySQL 风格的 /*! ... */ 在 PG 里同样只是普通块注释，一并吃掉
			depth := 1
			i += 2
			for i < n && depth > 0 {
				if r[i] == '/' && i+1 < n && r[i+1] == '*' {
					depth++
					i += 2
				} else if r[i] == '*' && i+1 < n && r[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth > 0 {
				return "", "", 0, fmt.Errorf("SQL 中存在未闭合的块注释 /* */，已拒绝执行")
			}
			b.WriteRune(' ')
			m.WriteRune(' ')
		case c == '\'' || c == '"':
			// 字符串/引号标识符：cleaned 原样拷贝，masked 整体挖空。
			// 引号内的分号、--、/* 都不做特殊处理（与 PG 词法一致），
			// '' 与 "" 是转义的引号自身。
			quote := c
			b.WriteRune(c)
			i++
			closed := false
			for i < n {
				if r[i] == quote {
					if i+1 < n && r[i+1] == quote { // '' 转义
						b.WriteRune(quote)
						b.WriteRune(quote)
						i += 2
						continue
					}
					b.WriteRune(quote)
					i++
					closed = true
					break
				}
				b.WriteRune(r[i])
				i++
			}
			if !closed {
				return "", "", 0, fmt.Errorf("SQL 中存在未闭合的引号，已拒绝执行")
			}
			m.WriteRune(' ') // 字面量在掩码里只占一个空格（含成对引号）
		case c == ';':
			semis++ // 只有引号外的分号才是语句分隔符
			b.WriteRune(c)
			m.WriteRune(c)
			i++
		default:
			b.WriteRune(c)
			m.WriteRune(c)
			i++
		}
	}
	return b.String(), m.String(), semis, nil
}

// ==================== pg_list_tables ====================

// listTables 技能一：列出关注库/schema 下的表清单（含类型与注释）。
type listTables struct{ base }

func (s *listTables) Name() string { return SkillListTables }

// Description 直接决定 LLM 的路由判断（对照 ops 技能的写法），
// 尾部动态拼接当前白名单，帮模型传对 db/schema 参数。
func (s *listTables) Description() string {
	return "查询 PostgreSQL 数据库中有哪些表（含 schema、表类型与表注释）。" +
		"当用户想了解某个数据库有哪些表、浏览库里的数据对象时使用；" +
		"写数据查询前也建议先用本技能了解结构。仅限查询关注列表内的数据库与 schema。" +
		s.whitelistHint()
}

func (s *listTables) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"db":{"type":"string","description":"数据库名（必须在关注数据库列表内）"},
			"schema":{"type":"string","description":"schema 名（可选；提供则必须在关注 schema 列表内，缺省列出全部非系统 schema）"}
		},
		"required":["db"],
		"additionalProperties":false
	}`)
}

// Execute 查系统目录 pg_catalog（比 information_schema 多拿得到表注释），
// schema 值一律走 $1 参数化——即使白名单被配置错了也不可能拼出注入。
func (s *listTables) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Db     string `json:"db"`
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数错误: 调用参数不是合法 JSON")
	}
	db, err := s.checkDb(in.Db)
	if err != nil {
		return "", err
	}
	schema, provided, err := s.checkSchema(in.Schema)
	if err != nil {
		return "", err
	}
	// relkind: r=表 p=分区表 v=视图 m=物化视图 f=外部表；排除系统 schema
	query := `SELECT n.nspname AS schema, c.relname AS name,
       CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'partitioned' WHEN 'v' THEN 'view'
            WHEN 'm' THEN 'matview' WHEN 'f' THEN 'foreign' END AS kind,
       obj_description(c.oid) AS comment
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','v','m','f')
  AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')
  AND n.nspname NOT LIKE 'pg\_temp%' AND n.nspname NOT LIKE 'pg\_toast\_temp%'`
	var qargs []any
	if provided {
		query += ` AND n.nspname = $1`
		qargs = append(qargs, schema)
	}
	query += ` ORDER BY n.nspname, c.relname`

	out, err := s.query(ctx, db, query, qargs, MaxRowLimit)
	if err != nil {
		return "", fmt.Errorf("查询库 %s 的表清单失败: %w", db, err)
	}
	title := fmt.Sprintf("数据库 %s 的表清单", db)
	if provided {
		title = fmt.Sprintf("数据库 %s（schema: %s）的表清单", db, schema)
	}
	return title + ":\n" + sanitize.CapChars(out, MaxOutputChars), nil
}

// ==================== pg_describe_table ====================

// describeTable 技能二：查看单表的列结构（列名/类型/可空/默认值）。
type describeTable struct{ base }

func (s *describeTable) Name() string { return SkillDescribeTable }

func (s *describeTable) Description() string {
	return "查询 PostgreSQL 中指定表的列结构：列名、数据类型、是否可空、默认值。" +
		"当用户问某张表的结构、有哪些字段/列、字段类型时使用；写复杂查询前也建议先用它确认列名。" +
		s.whitelistHint()
}

func (s *describeTable) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"db":{"type":"string","description":"数据库名（必须在关注数据库列表内）"},
			"schema":{"type":"string","description":"表所在 schema，常见为 public（必须在关注 schema 列表内）"},
			"table":{"type":"string","description":"表名"}
		},
		"required":["db","schema","table"],
		"additionalProperties":false
	}`)
}

func (s *describeTable) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Db     string `json:"db"`
		Schema string `json:"schema"`
		Table  string `json:"table"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数错误: 调用参数不是合法 JSON")
	}
	db, err := s.checkDb(in.Db)
	if err != nil {
		return "", err
	}
	// describe 必须定位到具体 schema（同名表可能存在于多个 schema），schema 必填
	schema, provided, err := s.checkSchema(in.Schema)
	if err != nil {
		return "", err
	}
	if !provided {
		return "", fmt.Errorf("参数错误: 需要提供 schema（表所在模式名，常见为 public）")
	}
	table := strings.TrimSpace(in.Table)
	if table == "" {
		return "", fmt.Errorf("参数错误: 需要提供 table（表名）")
	}
	// pg_attribute 存列信息；attnum>0 排除系统列，attisdropped 排除已删除列；
	// 表名/schema 都走参数化（$1/$2），绝不字符串拼接
	query := `SELECT a.attname AS column_name,
       format_type(a.atttypid, a.atttypmod) AS data_type,
       CASE WHEN a.attnotnull THEN 'NOT NULL' ELSE 'NULL' END AS nullable,
       pg_get_expr(d.adbin, d.adrelid) AS default_value
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`
	out, err := s.query(ctx, db, query, []any{schema, table}, MaxRowLimit)
	if err != nil {
		return "", fmt.Errorf("查询表 %s.%s 结构失败: %w", schema, table, err)
	}
	if strings.Contains(out, "（0 行）") {
		// 空结果 = 目录里没有这张表：给出可行动的提示而不是空表格
		return fmt.Sprintf("数据库 %s 中未找到表 %s.%s 的列信息：表可能不存在，"+
			"可先用 pg_list_tables 确认表名与所在 schema。", db, schema, table), nil
	}
	return fmt.Sprintf("数据库 %s 中表 %s.%s 的列结构:\n%s", db, schema, table, sanitize.CapChars(out, MaxOutputChars)), nil
}

// ==================== pg_query ====================

// queryData 技能三：执行只读 SQL 查询。
type queryData struct{ base }

func (s *queryData) Name() string { return SkillQuery }

func (s *queryData) Description() string {
	return "在指定的 PostgreSQL 数据库上执行只读 SQL 查询，结果以文本表格返回。" +
		"当用户想查询表数据、统计汇总、查某表前 N 条记录时使用。" +
		"仅允许以 SELECT 或 WITH 开头的单条只读语句（写操作、DDL、多语句一律拒绝）；" +
		"行数上限默认 200、最大 1000。不确定表结构时先用 pg_list_tables / pg_describe_table。" +
		s.whitelistHint()
}

func (s *queryData) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"db":{"type":"string","description":"数据库名（必须在关注数据库列表内）"},
			"sql":{"type":"string","description":"以 SELECT 或 WITH 开头的只读 SQL；只允许单条语句，不要以分号结尾拼接多条"},
			"limit":{"type":"integer","description":"返回行数上限，默认 200，最大 1000（超出自动钳制）"}
		},
		"required":["db","sql"],
		"additionalProperties":false
	}`)
}

func (s *queryData) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Db    string `json:"db"`
		Sql   string `json:"sql"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数错误: 调用参数不是合法 JSON")
	}
	db, err := s.checkDb(in.Db)
	if err != nil {
		return "", err
	}
	// 只读门禁：清理注释 → 单语句校验 → SELECT/WITH 前缀校验（顺序有讲究：
	// 先去掉注释才能识别 "/*x*/DELETE" 与 "--c\nSELECT" 这类伪装）
	query, err := checkReadOnlySQL(in.Sql)
	if err != nil {
		return "", err
	}
	// limit 归一化：缺省 200，封顶 1000（与 ops.analyze_logs 的行数归一化同款）
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultRowLimit
	}
	if limit > MaxRowLimit {
		limit = MaxRowLimit
	}
	out, err := s.query(ctx, db, query, nil, limit)
	if err != nil {
		return "", fmt.Errorf("在库 %s 上执行查询失败: %w", db, err)
	}
	// 字符总量封顶（≈ ops 的 MaxLogChars）：截断时 CapChars 会在尾部追加说明
	return fmt.Sprintf("数据库 %s 的查询结果（行数上限 %d）:\n%s", db, limit, sanitize.CapChars(out, MaxOutputChars)), nil
}
