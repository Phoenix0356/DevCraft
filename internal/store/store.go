// Package store 是持久层（≈ Java 的 DAO 层），用 SQLite 存储会话、消息、
// Agent 定义与设置。没有 ORM，全部是手写 SQL（≈ MyBatis 手写 SQL 的风格）。
//
// 使用纯 Go 驱动 modernc.org/sqlite（由 C 版 SQLite 机器翻译而来），
// 好处是不需要 CGO（C 编译器），交叉编译到任何平台都无痛。
package store

import (
	"database/sql" // Go 标准库的数据库抽象（≈ JDBC：DriverManager + Connection 的统一接口）
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // 下划线导入 = 只为执行包的 init() 副作用：
	// 该 init() 会向 database/sql 注册名为 "sqlite" 的驱动（≈ Class.forName 注册 JDBC 驱动）
)

// Store 持有数据库连接池句柄（≈ DataSource）。
type Store struct {
	db *sql.DB // sql.DB 内部自带连接池且并发安全，全局共享一个即可
}

// Open 打开（或创建）SQLite 数据库文件并执行建表迁移。
func Open(path string) (*Store, error) {
	// sql.Open 只是建立"驱动+连接串"的句柄，并不真正连接（懒加载）
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err) // %w 包装原错误，上层可用 errors.Is/As 解包
	}
	// SQLite 同一时刻只允许一个写者。把连接池上限设为 1，
	// 让并发写在 Go 侧排队，避免 "database is locked" 错误。
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close() // 出错时记得释放资源（Go 没有 try-with-resources，手动处理）
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库连接（应用退出时调用）。
func (s *Store) Close() error { return s.db.Close() }

// migrate 建表。CREATE TABLE IF NOT EXISTS 天然幂等，
// 所以每次启动都跑一遍也没问题（简化版迁移，没有版本号管理）。
func migrate(db *sql.DB) error {
	// 反引号字符串 = 原始字符串（≈ Java 15 文本块 """），换行和缩进原样保留
	schema := `
CREATE TABLE IF NOT EXISTS sessions (          -- 会话表：一个会话绑定一个 Agent
	id         TEXT PRIMARY KEY,                 -- UUID 字符串主键
	title      TEXT NOT NULL DEFAULT '',
	agent_id   TEXT NOT NULL DEFAULT '',
	title_source TEXT NOT NULL DEFAULT '',       -- 标题来源：''=首条消息截断占位 | 'ai'=AI 总结 | 'manual'=手动改名（锁定，AI 永不再覆盖）
	created_at INTEGER NOT NULL,                 -- 毫秒时间戳（SQLite 无日期类型，惯例存整数）
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (          -- 消息表：只存 user/assistant 最终问答，
	id         INTEGER PRIMARY KEY AUTOINCREMENT,-- tool 调用轮次是瞬态的不入库
	session_id TEXT NOT NULL,
	role       TEXT NOT NULL,                    -- user | assistant
	content    TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
CREATE TABLE IF NOT EXISTS agents (            -- Agent 定义表：Agent 是"数据"不是"代码"
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	system_prompt TEXT NOT NULL DEFAULT '',      -- 该 Agent 的人设/规则提示词
	model         TEXT NOT NULL DEFAULT '',      -- 专用模型；为空用全局默认模型
	builtin       INTEGER NOT NULL DEFAULT 0     -- SQLite 没有 boolean，用 0/1
);
CREATE TABLE IF NOT EXISTS agent_skills (      -- Agent↔Skill 装配关系（多对多中间表）
	agent_id   TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	PRIMARY KEY (agent_id, skill_name)           -- 联合主键防重复挂载
);
CREATE TABLE IF NOT EXISTS settings (          -- 键值设置表
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deploy_flows (      -- 部署流程定义表（一键部署）
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL UNIQUE,           -- 流程名（唯一，技能按名查找）
	description TEXT NOT NULL DEFAULT '',       -- 给人与给 LLM 看的用途说明
	target      TEXT NOT NULL DEFAULT 'local',  -- 执行通道：local=本机 shell | ssh=设置页远程主机
	params      TEXT NOT NULL DEFAULT '[]',     -- JSON: [{name, desc, pattern}] 参数声明
	steps       TEXT NOT NULL DEFAULT '[]',     -- JSON: [string] 有序命令步骤（含 {{参数}} 占位符）
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS deploy_history (    -- 部署执行历史表（审计/排错）
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	flow_id     INTEGER NOT NULL DEFAULT 0,     -- 触发时的流程 id（流程后来被删也保留快照）
	flow_name   TEXT NOT NULL DEFAULT '',
	params      TEXT NOT NULL DEFAULT '{}',     -- JSON: {"参数名":"值"} 本次执行使用的参数
	session_id  TEXT NOT NULL DEFAULT '',       -- 触发部署的会话
	target      TEXT NOT NULL DEFAULT '',       -- 执行通道描述（本机 / ssh://...）
	started_at  INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL DEFAULT '',       -- success | failed | canceled
	detail      TEXT NOT NULL DEFAULT ''        -- 步骤日志与错误（输出已截断脱敏）
);
CREATE INDEX IF NOT EXISTS idx_deploy_history_session ON deploy_history(session_id);
CREATE TABLE IF NOT EXISTS api_projects (        -- 项目表：一个后端服务/代码仓库（PRD R1）
    id          TEXT PRIMARY KEY,                 -- UUID 主键（google/uuid，仓库无 nanoid 之类约定）
    name        TEXT NOT NULL UNIQUE,             -- 服务标识 = meta 里的 project（如 paper-manager）
    display     TEXT NOT NULL DEFAULT '',         -- 显示名 = meta 里的 service（缺省同 name）
    base_path   TEXT NOT NULL DEFAULT '',         -- 仅用于校验与日后导出，不参与身份键
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_domains (          -- 领域表：文档一级 ## 章节（PRD D1）
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    name       TEXT NOT NULL,                     -- 归一后的领域名（剥中文序号前缀、尾部「接口」）
    internal   INTEGER NOT NULL DEFAULT 0,        -- 1 = 由「（内部）」限定语降级而来
    notes_raw  TEXT NOT NULL DEFAULT '',          -- 章级 preamble（认证方式/调用方），下渗给子接口
    sort       INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (project_id, name)
);
CREATE INDEX IF NOT EXISTS idx_api_domains_project ON api_domains(project_id);
CREATE TABLE IF NOT EXISTS api_entries (          -- 接口条目：只存当前态，不存快照（PRD Goal-1）
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
    domain_id        TEXT NOT NULL,
    name             TEXT NOT NULL,               -- 接口中文名（不参与 diff，只存最新值）
    method           TEXT NOT NULL,               -- 已大写归一
    path_raw         TEXT NOT NULL,               -- 原样（含 {id}），渲染与复制 curl 用
    identity_key     TEXT NOT NULL,               -- "PUT|/api/v1.0/user/subscription/{}"
    base_path_assert TEXT NOT NULL DEFAULT '',    -- 导入时的断言凭证，剥/不剥都留痕
    content_type     TEXT NOT NULL DEFAULT '',
    auth             TEXT NOT NULL DEFAULT '',    -- 需要登录 | 无需登录 | 内部Token | ''(未知)
    visibility       TEXT NOT NULL DEFAULT 'public', -- public | internal（RPC 端点默认过滤）
    semantic_hash    TEXT NOT NULL DEFAULT '',    -- 结构化事实指纹（进 diff 判定）
    doc_hash         TEXT NOT NULL DEFAULT '',    -- 中文说明/标题/示例指纹（只算措辞变更）
    description      TEXT NOT NULL DEFAULT '',    -- 说明段落原文（不参与 diff）
    parse_state      TEXT NOT NULL DEFAULT 'ok',  -- ok | partial | unparsed（不许猜的落地位）
    raw_markdown     TEXT NOT NULL DEFAULT '',    -- 该接口整段原文，保真兜底
    status           TEXT NOT NULL DEFAULT 'active', -- active | removed（软删，PRD 已自决）
    deleted_at       INTEGER NOT NULL DEFAULT 0,
    sort             INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    UNIQUE (project_id, identity_key)
);
CREATE INDEX IF NOT EXISTS idx_api_entries_domain  ON api_entries(domain_id);
CREATE INDEX IF NOT EXISTS idx_api_entries_list    ON api_entries(project_id, domain_id, sort);
CREATE INDEX IF NOT EXISTS idx_api_entries_status  ON api_entries(project_id, status);
CREATE TABLE IF NOT EXISTS api_fields (           -- 参数/返回/VO 字段：一行一字段（PRD 已自决「结构化字段表」）
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_kind      TEXT NOT NULL,                -- endpoint | schema
    owner_id        TEXT NOT NULL,                -- → api_entries.id / api_schemas.id
    kind            TEXT NOT NULL,                -- request|response|path|header（owner_kind=schema 时恒 body）
    name            TEXT NOT NULL,
    type_raw        TEXT NOT NULL DEFAULT '',     -- 显示保真：List\<Long\> / boolean / Boolean
    type_key        TEXT NOT NULL DEFAULT '',     -- 比较归一：List<Long> / Boolean（泛型转义在此消除）
    required        INTEGER NOT NULL DEFAULT -1,  -- -1 无该列（未知）| 0 否 | 1 是（语料实测只有 是/否）
    position        TEXT NOT NULL DEFAULT '',     -- Path|Query|Body|Header；init 模式推断则打 W-POS-INFERRED
    default_value   TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',     -- 中文说明，不进 semantic_hash
    facts_json      TEXT NOT NULL DEFAULT '{}',   -- JSON: {enum:[],fixed:"",statusCodes:[],flags:[]} 从说明抽取
    ref_schema_id   TEXT NOT NULL DEFAULT '',     -- = $ref 语义（VO 引用），空=标量
    depth           INTEGER NOT NULL DEFAULT 0,   -- 最深实测 3 层（论文:258→289→302）
    sort            INTEGER NOT NULL DEFAULT 0,
    UNIQUE (owner_kind, owner_id, kind, name)      -- ★ 字段级 diff 的身份就在这条数据库级唯一索引上
);
CREATE INDEX IF NOT EXISTS idx_api_fields_owner ON api_fields(owner_kind, owner_id, kind);
CREATE INDEX IF NOT EXISTS idx_api_fields_ref   ON api_fields(ref_schema_id);  -- 反查「谁用了这个 VO」
CREATE TABLE IF NOT EXISTS api_schemas (         -- 命名结构（≈ OpenAPI components/schemas 极简版）
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    name        TEXT NOT NULL,                    -- PaperRetrievalVO / IPage / SubscriptionVO
    owner_scope TEXT NOT NULL DEFAULT '',         -- '' = 文档级；否则 = 宿主接口 id（同名不同构必须按作用域隔离）
    raw_markdown TEXT NOT NULL DEFAULT '',
    body_hash   TEXT NOT NULL DEFAULT '',
    ambiguous   INTEGER NOT NULL DEFAULT 0,       -- 1 = 同名多结构，禁止按名自动展开
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (project_id, name, owner_scope)
);
CREATE TABLE IF NOT EXISTS api_examples (         -- 示例与 curl：整块原文，永不结构化、不参与 diff
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_kind TEXT NOT NULL,                     -- endpoint | schema
    owner_id   TEXT NOT NULL,
    kind       TEXT NOT NULL,                     -- req_example|res_example|curl|multipart_raw|error_example
    language   TEXT NOT NULL DEFAULT '',           -- json | bash | text（multipart 原始报文 → text）
    content    TEXT NOT NULL DEFAULT '',
    sort       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_api_examples_owner ON api_examples(owner_kind, owner_id, kind);
CREATE TABLE IF NOT EXISTS api_change_events (   -- 变更事件主表：一次导入 = 一行（无状态机，PRD D10）
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT NOT NULL,
    seq        INTEGER NOT NULL,                   -- 项目内自增版本号 v1 v2 v3（PRD D7）
    label      TEXT NOT NULL DEFAULT '',           -- 需求单号/发布标签，兼任 D10 聚合分组键
    title      TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',           -- 为什么改（= meta change_note，核心诉求）
    kind       TEXT NOT NULL DEFAULT 'feature',    -- feature | fix | refactor | docs（见 Q4/Q5 待拍板）
    breaking   INTEGER NOT NULL DEFAULT 0,         -- 0 无/未判定 | 1 有破坏性 | 2 需人工确认（三态）
    source     TEXT NOT NULL DEFAULT 'import',     -- import | manual
    doc_ref    TEXT NOT NULL DEFAULT '',           -- 来源文档路径
    scope_mode TEXT NOT NULL DEFAULT 'incremental',-- incremental | full | init（删除语义留痕）
    stats      TEXT NOT NULL DEFAULT '{}',         -- JSON: {"added":2,"updated":1,"removed":1,"moved":0}
    meta       TEXT NOT NULL DEFAULT '{}',         -- JSON 扩展槽：doc_sha256 / 证书结果 / 兼容性预留（避免加列迁移）
    doc_text   TEXT NOT NULL DEFAULT '',           -- D12：本次导入文档全文（输入凭证，非接口快照），列表页禁止 SELECT *
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_events_seq  ON api_change_events(project_id, seq);
CREATE INDEX IF NOT EXISTS idx_api_events_list        ON api_change_events(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_api_events_label       ON api_change_events(project_id, label);
CREATE TABLE IF NOT EXISTS api_change_items (        -- 事件明细：本次涉及的每个接口
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id      INTEGER NOT NULL,
    project_id    TEXT NOT NULL,                    -- 冗余，让明细可独立按项目查
    entry_id      TEXT NOT NULL DEFAULT '',         -- 软删后仍保留，供详情页跳转
    domain_name   TEXT NOT NULL DEFAULT '',         -- 冗余快照：领域会被改名/合并
    entry_name    TEXT NOT NULL DEFAULT '',         -- 冗余快照：照 deploy_history.flow_name
    method        TEXT NOT NULL,
    path          TEXT NOT NULL,
    change_type   TEXT NOT NULL,                    -- added | updated | removed | moved | restored
    breaking      INTEGER NOT NULL DEFAULT 0,       -- 三态，D5.2/D16 确认闸的判据
    visibility    TEXT NOT NULL DEFAULT 'public',   -- internal 行在事件详情页默认折叠
    diff          TEXT NOT NULL DEFAULT '[]',       -- JSON: 字段级 diff 载荷（见 §3.2）
    declared_note TEXT NOT NULL DEFAULT '',         -- = 文档的「变更点」原文
    sort          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_api_items_event  ON api_change_items(event_id, sort);
CREATE INDEX IF NOT EXISTS idx_api_items_entry  ON api_change_items(entry_id);
CREATE INDEX IF NOT EXISTS idx_api_items_lookup ON api_change_items(project_id, method, path);
CREATE TABLE IF NOT EXISTS api_import_batches (  -- 导入批次：删除安全的载体
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id    TEXT NOT NULL,
    scope_identity TEXT NOT NULL,                  -- project|base_path|规范化文档标题
    declared_mode TEXT NOT NULL,                   -- incremental | full | init
    doc_fingerprint TEXT NOT NULL DEFAULT '',      -- 文档 sha256，幂等第二道保险
    keys_json     TEXT NOT NULL DEFAULT '[]',      -- JSON: [identity_key] 本次声明的 key 集
    summary_check TEXT NOT NULL DEFAULT '{}',      -- JSON: C1/C2/C3 证书结果
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_api_batch_scope ON api_import_batches(scope_identity, id);
`
	_, err := db.Exec(schema) // Exec 用于不返回行的语句（DDL/INSERT/UPDATE）
	if err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS 只管新库；存量库的表结构演进靠下面的幂等加列。
	return ensureColumns(db)
}

// ensureColumns 为存量库补齐后加的列（本仓库唯一的加列迁移入口）。
// 每一步都必须失败可感知：迁移失败会一路返回到 Open，让启动阶段直接暴露问题，
// 绝不静默跳过（列缺失会让后面的读写全部报错，静默只会把故障推迟到更难定位的地方）。
func ensureColumns(db *sql.DB) error {
	return ensureSessionColumn(db, "title_source",
		`ALTER TABLE sessions ADD COLUMN title_source TEXT NOT NULL DEFAULT ''`)
}

// sessionsTableInfoSQL 查 sessions 表结构。表名在本包内写死成常量，不接任何外部输入
// （PRAGMA 的表名不能走 ? 占位符，只能拼字符串——所以把可拼的部分收敛成常量，
// 杜绝标识符注入面）。
const sessionsTableInfoSQL = `PRAGMA table_info(sessions)`

// ensureSessionColumn 幂等加列：先 PRAGMA table_info 探测列是否存在，缺了才 ALTER TABLE。
// 三种库都要干净通过：
//   - 全新库：CREATE TABLE 已带该列 → 探测命中 → 跳过；
//   - 存量库无该列：探测未命中 → ALTER 加列（DEFAULT 让旧行取空串）；
//   - 存量库已有该列（历史上跑过旧版迁移的机器）：探测命中 → 跳过，绝不重复 ALTER。
func ensureSessionColumn(db *sql.DB, column, alterDDL string) error {
	rows, err := db.Query(sessionsTableInfoSQL)
	if err != nil {
		return fmt.Errorf("读取 sessions 表结构失败: %w", err)
	}
	defer rows.Close()
	exists := false
	for rows.Next() {
		name, err := scanColumnInfo(rows)
		if err != nil {
			return fmt.Errorf("解析 sessions 表结构失败: %w", err)
		}
		if name == column {
			exists = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历 sessions 表结构失败: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(alterDDL); err != nil {
		return fmt.Errorf("为 sessions 添加列 %s 失败: %w", column, err)
	}
	return nil
}

// scanColumnInfo 读 PRAGMA table_info 当前行（列顺序固定：cid, name, type,
// notnull, dflt_value, pk），只取列名。
func scanColumnInfo(rows *sql.Rows) (string, error) {
	var cid, notNull, pk int
	var name, colType string
	var dfltValue sql.NullString
	if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
		return "", err
	}
	return name, nil
}

// sessionColumns 查 sessions 表的列名清单（仅测试用于断言列没有被重复添加）。
func (s *Store) sessionColumns() ([]string, error) {
	rows, err := s.db.Query(sessionsTableInfoSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		name, err := scanColumnInfo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ===================== Sessions（会话 CRUD）=====================

// Session 会话实体。反引号里的是 struct tag：
// `json:"id"` 告诉 JSON 序列化器字段名叫 id（≈ Jackson 的 @JsonProperty("id")）。
// Wails 把 Go 对象传给前端时就是按这些 tag 序列化的。
type Session struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	AgentID     string `json:"agentId"`
	TitleSource string `json:"titleSource"` // 空串=首条消息截断占位 | 'ai' | 'manual'（见标题来源常量）
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// 标题来源取值（sessions.title_source 列的状态机，见 appsvc/title.go）。
const (
	TitleSourceAI     = "ai"     // AI 自动总结写入
	TitleSourceManual = "manual" // 手动改名，永久锁定（AI 不再覆盖）
)

// CreateSession 插入新会话。
// title_source 不在 INSERT 列里：新建会话必然是占位态（空串），由列 DEFAULT 落位，
// 不给调用方"带着别的来源创建"的机会（状态只能靠下面的定向写入推进）。
func (s *Store) CreateSession(sess Session) error {
	now := time.Now().UnixMilli() // 毫秒时间戳
	if sess.CreatedAt == 0 {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, title, agent_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		sess.ID, sess.Title, sess.AgentID, sess.CreatedAt, sess.UpdatedAt, // ? 占位符防注入（≈ PreparedStatement）
	)
	return err
}

// ListSessions 按最后活跃时间倒序返回全部会话。
func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, title, agent_id, title_source, created_at, updated_at FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // defer = 函数返回前必定执行（≈ finally），防止连接泄漏
	var out []Session  // nil 切片可以直接 append，Go 的切片 ≈ 自动扩容的 ArrayList
	for rows.Next() {  // 逐行迭代（≈ ResultSet.next()）
		var sess Session
		// Scan 把当前行各列按顺序读入变量（列顺序必须与 SELECT 一致）
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.AgentID, &sess.TitleSource, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err // & 取地址：Scan 需要指针才能写入（≈ 传引用）
		}
		out = append(out, sess)
	}
	return out, rows.Err() // 迭代结束后必须检查是否有中途错误
}

// GetSession 按 id 查单个会话，不存在时返回带说明的 error。
func (s *Store) GetSession(id string) (Session, error) {
	var sess Session
	// QueryRow 用于"最多一行"的查询，Scan 时若没数据会得到哨兵错误 sql.ErrNoRows
	err := s.db.QueryRow(`SELECT id, title, agent_id, title_source, created_at, updated_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Title, &sess.AgentID, &sess.TitleSource, &sess.CreatedAt, &sess.UpdatedAt)
	if err == sql.ErrNoRows {
		return sess, fmt.Errorf("session %s not found", id)
	}
	return sess, err
}

// ===================== Sessions 定向写入（列所有权）=====================
//
// 这里刻意没有"全字段 UPDATE"的口子：会话行由多个写入方并发演进（占位标题、
// AI 异步总结、手动改名、@前缀切 Agent），全字段回写会把调用方读到的陈旧内存值
// 盖回数据库，冲掉别的写入方刚写进去的列。所以每个写入方只写自己拥有的列，
// 并且把"能不能写"的判断也放进 SQL 条件里（保护逻辑落在库里，不靠调用方自律）。
// 返回值 bool = 是否真的写入了（RowsAffected > 0），false 表示被 WHERE 条件挡下。

// UpdateSessionTitle AI 总结写标题：只在标题不是手动改过的情况下生效。
// title_source='manual' 的行被 WHERE 永久排除——手动改名保护由 SQL 条件保证。
func (s *Store) UpdateSessionTitle(id, title string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET title = ?, title_source = ?, updated_at = ?
		 WHERE id = ? AND title_source <> ?`,
		title, TitleSourceAI, time.Now().UnixMilli(), id, TitleSourceManual)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetSessionTitleManual 手动改名：写标题并把来源锁成 manual（此后 AI 永不再覆盖）。
func (s *Store) SetSessionTitleManual(id, title string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET title = ?, title_source = ?, updated_at = ? WHERE id = ?`,
		title, TitleSourceManual, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetSessionFallbackTitle 首条消息截断的占位标题定向写入。
// 守卫在 SQL 里：仅当标题仍是"新会话"/空 且 未被手动改名时才写——
// 这样并发的 AI 总结先落库后，占位写入自然落空（不会用截断文本盖掉 AI 标题）。
func (s *Store) SetSessionFallbackTitle(id, title string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE sessions SET title = ?, updated_at = ?
		 WHERE id = ? AND (title = '' OR title = '新会话') AND title_source <> ?`,
		title, time.Now().UnixMilli(), id, TitleSourceManual)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetSessionAgentID 只改会话绑定的 Agent（不触碰标题与来源）。
func (s *Store) SetSessionAgentID(id, agentID string) (bool, error) {
	res, err := s.db.Exec(`UPDATE sessions SET agent_id = ?, updated_at = ? WHERE id = ?`,
		agentID, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeleteSession 级联删除会话和它的所有消息。
func (s *Store) DeleteSession(id string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE session_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ===================== Messages（消息）=====================

// Message 消息实体。
type Message struct {
	ID        int64  `json:"id"`
	SessionID string `json:"sessionId"`
	Role      string `json:"role"` // user | assistant | tool
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"`
}

// AppendMessage 追加一条消息，并同步刷新所属会话的 updated_at。
func (s *Store) AppendMessage(m Message) error {
	m.CreatedAt = time.Now().UnixMilli()
	res, err := s.db.Exec(`INSERT INTO messages (session_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		m.SessionID, m.Role, m.Content, m.CreatedAt)
	if err != nil {
		return err
	}
	// LastInsertId 拿到自增 id（失败也不致命，忽略错误只记 id）
	if id, err := res.LastInsertId(); err == nil {
		m.ID = id
	}
	_, err = s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, m.CreatedAt, m.SessionID)
	return err
}

// Messages 返回会话消息；limit>0 时只取最近 limit 条。
// 实现技巧：先按 id DESC 取最近 N 条，再在内存里反转回时间正序。
func (s *Store) Messages(sessionID string, limit int) ([]Message, error) {
	query := `SELECT id, session_id, role, content, created_at FROM messages WHERE session_id = ? ORDER BY id DESC`
	args := []any{sessionID} // []any = []interface{}，动态拼参数列表（any ≈ Object）
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...) // args... 把切片展开成多个参数（≈ varargs）
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 双指针原地反转切片（倒序 → 正序）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i] // Go 支持元组式交换写法
	}
	return out, nil
}

// ===================== Agents（Agent 定义与装配）=====================

// AgentRow Agent 定义实体（对应 agents + agent_skills 两张表）。
type AgentRow struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	SystemPrompt string   `json:"systemPrompt"`
	Model        string   `json:"model"`
	Builtin      bool     `json:"builtin"`
	Skills       []string `json:"skills"` // 挂载的技能名列表（从中间表聚合而来）
}

// UpsertAgent 插入或更新 Agent，并全量替换其技能装配。
func (s *Store) UpsertAgent(a AgentRow) error {
	builtin := 0 // Go 的 bool 不能直接存 SQLite，手工转 0/1
	if a.Builtin {
		builtin = 1
	}
	_, err := s.db.Exec(
		// ON CONFLICT ... DO UPDATE 是 SQLite 的 upsert 语法：
		// 主键冲突时改为更新指定列（excluded 指"本次想插入的那行数据"）
		`INSERT INTO agents (id, name, system_prompt, model, builtin) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name = excluded.name, system_prompt = excluded.system_prompt, model = excluded.model`,
		a.ID, a.Name, a.SystemPrompt, a.Model, builtin)
	if err != nil {
		return err
	}
	// 装配关系采用"先删后插"策略，简单且保证与入参完全一致
	if _, err := s.db.Exec(`DELETE FROM agent_skills WHERE agent_id = ?`, a.ID); err != nil {
		return err
	}
	for _, sk := range a.Skills { // range = for-each
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO agent_skills (agent_id, skill_name) VALUES (?, ?)`, a.ID, sk); err != nil {
			return err
		}
	}
	return nil
}

// ListAgents 返回全部 Agent（含各自挂载的技能列表）。
func (s *Store) ListAgents() ([]AgentRow, error) {
	rows, err := s.db.Query(`SELECT id, name, system_prompt, model, builtin FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		var a AgentRow
		var builtin int
		if err := rows.Scan(&a.ID, &a.Name, &a.SystemPrompt, &a.Model, &builtin); err != nil {
			return nil, err
		}
		a.Builtin = builtin == 1
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 逐个 Agent 查装配表补齐 Skills（N+1 查询，但 Agent 数量极少，可接受）
	for i := range out { // range 索引遍历；out[i] 直接改原切片元素
		skills, err := s.agentSkills(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Skills = skills
	}
	return out, nil
}

// GetAgent 按 id 查单个 Agent（含技能列表）。
func (s *Store) GetAgent(id string) (AgentRow, error) {
	var a AgentRow
	var builtin int
	err := s.db.QueryRow(`SELECT id, name, system_prompt, model, builtin FROM agents WHERE id = ?`, id).
		Scan(&a.ID, &a.Name, &a.SystemPrompt, &a.Model, &builtin)
	if err == sql.ErrNoRows {
		return a, fmt.Errorf("agent %s not found", id)
	}
	if err != nil {
		return a, err
	}
	a.Builtin = builtin == 1
	a.Skills, err = s.agentSkills(id)
	return a, err
}

// agentSkills 查某 Agent 挂载的技能名列表（小写开头 = 包内私有辅助方法）。
func (s *Store) agentSkills(agentID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT skill_name FROM agent_skills WHERE agent_id = ? ORDER BY skill_name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// UpdateAgent 只更新名称/系统提示词/模型三项信息
// （不触碰 builtin 标记与技能装配——装配替换走 ReplaceAgentSkills）。
func (s *Store) UpdateAgent(a AgentRow) error {
	_, err := s.db.Exec(`UPDATE agents SET name = ?, system_prompt = ?, model = ? WHERE id = ?`,
		a.Name, a.SystemPrompt, a.Model, a.ID)
	return err
}

// ReplaceAgentSkills 整组替换某 Agent 的技能装配（事务内先删后插，结果与入参完全一致）。
// 空列表 = 清空装配（纯聊天 Agent，合法状态）。
func (s *Store) ReplaceAgentSkills(agentID string, skills []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // 已 Commit 后 Rollback 是 no-op（防御性）
	if _, err := tx.Exec(`DELETE FROM agent_skills WHERE agent_id = ?`, agentID); err != nil {
		return err
	}
	for _, sk := range skills {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_skills (agent_id, skill_name) VALUES (?, ?)`, agentID, sk); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteAgent 删除 Agent 及其装配关系（级联清理中间表，避免孤儿行）。
func (s *Store) DeleteAgent(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM agent_skills WHERE agent_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agents WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ===================== Settings（键值设置）=====================

// GetSetting 读设置项。返回 (值, 是否存在, 错误)——Go 惯用多返回值代替 Optional。
func (s *Store) GetSetting(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil // 不存在不是错误，用第二个返回值表达
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting 写设置项（存在则覆盖）。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// ===================== DeployFlows（部署流程定义）=====================

// 部署目标取值（执行通道）。
const (
	TargetLocal = "local" // 本机 shell 执行
	TargetSSH   = "ssh"   // 设置页配置的 SSH 远程主机执行
)

// FlowParam 流程参数声明：名字 + 说明 + 可选校验正则（全匹配语义）。
type FlowParam struct {
	Name    string `json:"name"`
	Desc    string `json:"desc"`
	Pattern string `json:"pattern"` // 为空 = 不做正则校验
}

// DeployFlow 部署流程定义（声明式：参数声明 + 有序命令步骤模板）。
// 步骤命令里的 {{参数名}} 占位符在执行前经转义后替换（见 shellx 包）。
type DeployFlow struct {
	ID          int64       `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Target      string      `json:"target"` // TargetLocal | TargetSSH
	Params      []FlowParam `json:"params"`
	Steps       []string    `json:"steps"`
	CreatedAt   int64       `json:"createdAt"`
	UpdatedAt   int64       `json:"updatedAt"`
}

// SaveDeployFlow 新增或更新流程。ID==0 = 新建（回填自增 id）；否则按 id 更新。
// 名称唯一约束冲突返回中文错误（前端直接展示）。
// params/steps 两列存 JSON 文本（≈ 把对象序列化进 VARCHAR，读时反序列化）。
func (s *Store) SaveDeployFlow(f *DeployFlow) error {
	paramsJSON, err := json.Marshal(f.Params)
	if err != nil {
		return fmt.Errorf("序列化参数声明失败: %w", err)
	}
	stepsJSON, err := json.Marshal(f.Steps)
	if err != nil {
		return fmt.Errorf("序列化步骤失败: %w", err)
	}
	now := time.Now().UnixMilli()
	if f.ID == 0 {
		// 先查名称冲突（给中文提示，而不是裸的 UNIQUE constraint failed）
		if dup, err := s.deployFlowNameTaken(f.Name, 0); err != nil {
			return err
		} else if dup {
			return fmt.Errorf("部署流程名称已存在: %s", f.Name)
		}
		f.CreatedAt = now
		f.UpdatedAt = now
		res, err := s.db.Exec(
			`INSERT INTO deploy_flows (name, description, target, params, steps, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			f.Name, f.Description, f.Target, string(paramsJSON), string(stepsJSON), f.CreatedAt, f.UpdatedAt)
		if err != nil {
			return err
		}
		if id, err := res.LastInsertId(); err == nil {
			f.ID = id
		}
		return nil
	}
	if dup, err := s.deployFlowNameTaken(f.Name, f.ID); err != nil {
		return err
	} else if dup {
		return fmt.Errorf("部署流程名称已存在: %s", f.Name)
	}
	f.UpdatedAt = now
	_, err = s.db.Exec(
		`UPDATE deploy_flows SET name = ?, description = ?, target = ?, params = ?, steps = ?, updated_at = ? WHERE id = ?`,
		f.Name, f.Description, f.Target, string(paramsJSON), string(stepsJSON), f.UpdatedAt, f.ID)
	return err
}

// deployFlowNameTaken 检查名称是否被"其他"流程占用（excludeID 排除自己）。
func (s *Store) deployFlowNameTaken(name string, excludeID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM deploy_flows WHERE name = ? AND id != ?`, name, excludeID).Scan(&n)
	return n > 0, err
}

// ListDeployFlows 按名称排序返回全部流程。
func (s *Store) ListDeployFlows() ([]DeployFlow, error) {
	rows, err := s.db.Query(`SELECT id, name, description, target, params, steps, created_at, updated_at FROM deploy_flows ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployFlow
	for rows.Next() {
		var f DeployFlow
		var paramsJSON, stepsJSON string
		if err := rows.Scan(&f.ID, &f.Name, &f.Description, &f.Target, &paramsJSON, &stepsJSON, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		// JSON 列反序列化；坏数据不让整个列表崩掉（该条按空值处理并继续）
		_ = json.Unmarshal([]byte(paramsJSON), &f.Params)
		_ = json.Unmarshal([]byte(stepsJSON), &f.Steps)
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetDeployFlow 按 id 查流程；不存在返回中文错误。
func (s *Store) GetDeployFlow(id int64) (DeployFlow, error) {
	var f DeployFlow
	var paramsJSON, stepsJSON string
	err := s.db.QueryRow(`SELECT id, name, description, target, params, steps, created_at, updated_at FROM deploy_flows WHERE id = ?`, id).
		Scan(&f.ID, &f.Name, &f.Description, &f.Target, &paramsJSON, &stepsJSON, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		return f, fmt.Errorf("部署流程不存在（id=%d）", id)
	}
	if err != nil {
		return f, err
	}
	_ = json.Unmarshal([]byte(paramsJSON), &f.Params)
	_ = json.Unmarshal([]byte(stepsJSON), &f.Steps)
	return f, nil
}

// GetDeployFlowByName 按名称查流程（技能调用时用流程名定位）。
func (s *Store) GetDeployFlowByName(name string) (DeployFlow, error) {
	var f DeployFlow
	var paramsJSON, stepsJSON string
	err := s.db.QueryRow(`SELECT id, name, description, target, params, steps, created_at, updated_at FROM deploy_flows WHERE name = ?`, name).
		Scan(&f.ID, &f.Name, &f.Description, &f.Target, &paramsJSON, &stepsJSON, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		return f, fmt.Errorf("部署流程不存在: %s", name)
	}
	if err != nil {
		return f, err
	}
	_ = json.Unmarshal([]byte(paramsJSON), &f.Params)
	_ = json.Unmarshal([]byte(stepsJSON), &f.Steps)
	return f, nil
}

// DeleteDeployFlow 删除流程（历史记录保留：审计需要，且历史自带流程名快照）。
func (s *Store) DeleteDeployFlow(id int64) error {
	_, err := s.db.Exec(`DELETE FROM deploy_flows WHERE id = ?`, id)
	return err
}

// ===================== DeployHistory（部署执行历史）=====================

// 执行状态取值。
const (
	DeploySuccess  = "success"  // 全部步骤成功
	DeployFailed   = "failed"   // 某步失败或执行超时（中断）
	DeployCanceled = "canceled" // 用户取消（停止按钮）
)

// DeployHistory 一次部署执行的审计记录。
type DeployHistory struct {
	ID         int64             `json:"id"`
	FlowID     int64             `json:"flowId"`
	FlowName   string            `json:"flowName"`
	Params     map[string]string `json:"params"`
	SessionID  string            `json:"sessionId"`
	Target     string            `json:"target"`
	StartedAt  int64             `json:"startedAt"`
	FinishedAt int64             `json:"finishedAt"`
	Status     string            `json:"status"` // DeploySuccess | DeployFailed | DeployCanceled
	Detail     string            `json:"detail"`
}

// InsertDeployHistory 在执行开始时插入记录（此时只有 started_at），回填自增 id。
func (s *Store) InsertDeployHistory(h *DeployHistory) error {
	h.StartedAt = time.Now().UnixMilli()
	paramsJSON, err := json.Marshal(h.Params)
	if err != nil {
		paramsJSON = []byte("{}")
	}
	res, err := s.db.Exec(
		`INSERT INTO deploy_history (flow_id, flow_name, params, session_id, target, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		h.FlowID, h.FlowName, string(paramsJSON), h.SessionID, h.Target, h.StartedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		h.ID = id
	}
	return nil
}

// UpdateDeployHistory 执行结束时回填终态（finished_at / status / detail）。
func (s *Store) UpdateDeployHistory(h DeployHistory) error {
	h.FinishedAt = time.Now().UnixMilli()
	_, err := s.db.Exec(
		`UPDATE deploy_history SET finished_at = ?, status = ?, detail = ? WHERE id = ?`,
		h.FinishedAt, h.Status, h.Detail, h.ID)
	return err
}

// GetDeployHistory 按 id 查执行记录（测试/排错用）。
func (s *Store) GetDeployHistory(id int64) (DeployHistory, error) {
	var h DeployHistory
	var paramsJSON string
	err := s.db.QueryRow(`SELECT id, flow_id, flow_name, params, session_id, target, started_at, finished_at, status, detail FROM deploy_history WHERE id = ?`, id).
		Scan(&h.ID, &h.FlowID, &h.FlowName, &paramsJSON, &h.SessionID, &h.Target, &h.StartedAt, &h.FinishedAt, &h.Status, &h.Detail)
	if err == sql.ErrNoRows {
		return h, fmt.Errorf("部署执行记录不存在（id=%d）", id)
	}
	if err != nil {
		return h, err
	}
	_ = json.Unmarshal([]byte(paramsJSON), &h.Params)
	return h, nil
}
