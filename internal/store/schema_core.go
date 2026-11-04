package store

// schemaCore 核心业务表 DDL：sessions / messages / agents / agent_skills /
// settings / deploy_flows / deploy_history。
// 全部 CREATE ... IF NOT EXISTS（幂等），存量库的列演进交给 migrate.go 的
// ensureColumns；新库直接在这里把列建全。
const schemaCore = `
CREATE TABLE IF NOT EXISTS sessions (          -- 会话表：一个会话绑定一个 Agent
	id         TEXT PRIMARY KEY,                 -- UUID 字符串主键
	title      TEXT NOT NULL DEFAULT '',
	agent_id   TEXT NOT NULL DEFAULT '',
	title_source TEXT NOT NULL DEFAULT '',       -- 标题来源：''=首条消息截断占位 | 'ai'=AI 总结 | 'manual'=手动改名（锁定，AI 永不再覆盖）
	pinned       INTEGER NOT NULL DEFAULT 0,      -- 置顶标记：0 普通会话 | 1 置顶（列表排序前置，侧栏分组依据）
	pinned_at    INTEGER NOT NULL DEFAULT 0,      -- 置顶时间毫秒；0 = 未置顶（置顶组内倒序的依据，与 pinned 同步落位）
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
`
