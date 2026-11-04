package store

import (
	"database/sql"
	"fmt"
)

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
