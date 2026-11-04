package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

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
