package store

// schemaAPI 接口文档模块（api_* 九张表）DDL（实体与 CRUD 见 api.go）。
// 全部 CREATE ... IF NOT EXISTS（幂等）；api_entries.unresolved 这类
// 后加的列由 migrate.go 的 ensureColumns 负责补齐存量库。
const schemaAPI = `
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
    unresolved       TEXT NOT NULL DEFAULT '[]',  -- JSON: 未解析槽位数组（详情页横幅列缺失小节用；存量旧行='[]'，重导治愈）
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
