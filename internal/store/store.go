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
	_, err := db.Exec(schema) // Exec 用于不返回行的语句（DDL/INSERT/UPDATE）
	if err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}

// ===================== Sessions（会话 CRUD）=====================

// Session 会话实体。反引号里的是 struct tag：
// `json:"id"` 告诉 JSON 序列化器字段名叫 id（≈ Jackson 的 @JsonProperty("id")）。
// Wails 把 Go 对象传给前端时就是按这些 tag 序列化的。
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	AgentID   string `json:"agentId"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// CreateSession 插入新会话。
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
	rows, err := s.db.Query(`SELECT id, title, agent_id, created_at, updated_at FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // defer = 函数返回前必定执行（≈ finally），防止连接泄漏
	var out []Session  // nil 切片可以直接 append，Go 的切片 ≈ 自动扩容的 ArrayList
	for rows.Next() {  // 逐行迭代（≈ ResultSet.next()）
		var sess Session
		// Scan 把当前行各列按顺序读入变量（列顺序必须与 SELECT 一致）
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.AgentID, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
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
	err := s.db.QueryRow(`SELECT id, title, agent_id, created_at, updated_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Title, &sess.AgentID, &sess.CreatedAt, &sess.UpdatedAt)
	if err == sql.ErrNoRows {
		return sess, fmt.Errorf("session %s not found", id)
	}
	return sess, err
}

// UpdateSession 更新标题/绑定 Agent，并刷新 updated_at（用于列表排序）。
func (s *Store) UpdateSession(sess Session) error {
	sess.UpdatedAt = time.Now().UnixMilli()
	_, err := s.db.Exec(`UPDATE sessions SET title = ?, agent_id = ?, updated_at = ? WHERE id = ?`,
		sess.Title, sess.AgentID, sess.UpdatedAt, sess.ID)
	return err
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
