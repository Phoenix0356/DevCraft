// agents.go 是智能体管理的对外聚合（与 skills.go 同风格）：
// 把 Agent 注册表里的定义数据整理成前端管理弹窗可直接渲染的明细
// （基本信息 + 已装配技能 + 可选技能全集，技能自带内置/自定义分类标记），
// 并提供信息保存与装配替换两个写入口（校验在编排层与注册表层双层完成）。
//
// 装配变更即时落库、无需通知聊天侧：Runner 每回合实时读装配，
// 下一轮对话的工具列表自动变化。
package appsvc

import (
	"fmt"
	"strings"
)

// AgentSkillInfo 装配区/已装配列表中单个技能的展示元数据。
type AgentSkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"` // 描述（实时值；部署技能含动态流程清单）
	// Builtin 分类标记：内置/自定义。来自技能注册表侧（Registry.IsBuiltin），
	// 本期全部内置；为下期自定义技能预留，前端只认标记不按名字判断。
	Builtin bool `json:"builtin"`
}

// AgentDetail 单个智能体的管理明细（智能体管理弹窗列表卡片与详情的数据源）。
type AgentDetail struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Model        string           `json:"model"`
	SystemPrompt string           `json:"systemPrompt"`
	Builtin      bool             `json:"builtin"`
	Skills       []AgentSkillInfo `json:"skills"`          // 已装配技能（卡片计数与详情回显）
	Available    []AgentSkillInfo `json:"availableSkills"` // 可选技能全集（装配区复选列表，含分类标记）
}

// ListAgentsDetail 返回全部智能体的明细（管理弹窗打开时拉取）。
func (s *Service) ListAgentsDetail() ([]AgentDetail, error) {
	rows, err := s.agents.List()
	if err != nil {
		return nil, err
	}
	out := make([]AgentDetail, 0, len(rows))
	for _, row := range rows {
		out = append(out, AgentDetail{
			ID:           row.ID,
			Name:         row.Name,
			Model:        row.Model,
			SystemPrompt: row.SystemPrompt,
			Builtin:      row.Builtin,
			Skills:       s.assembledSkillsInfo(row.Skills),
			Available:    s.allSkillsInfo(),
		})
	}
	return out, nil
}

// assembledSkillsInfo 把已装配技能名转成展示元数据；未注册的名字防御性跳过
// （正常不会发生——装配写入前都经过注册表校验）。
func (s *Service) assembledSkillsInfo(names []string) []AgentSkillInfo {
	out := make([]AgentSkillInfo, 0, len(names))
	for _, n := range names {
		sk, ok := s.skills.Get(n)
		if !ok {
			continue
		}
		out = append(out, s.skillInfo(sk.Name()))
	}
	return out
}

// allSkillsInfo 全部技能目录（按名有序，由 Registry.All 保证）。
func (s *Service) allSkillsInfo() []AgentSkillInfo {
	all := s.skills.All()
	out := make([]AgentSkillInfo, 0, len(all))
	for _, sk := range all {
		out = append(out, s.skillInfo(sk.Name()))
	}
	return out
}

// skillInfo 按名字取单个技能的展示元数据（描述实时值 + 注册表侧分类标记）。
func (s *Service) skillInfo(name string) AgentSkillInfo {
	sk, _ := s.skills.Get(name)
	desc := ""
	if sk != nil {
		desc = sk.Description()
	}
	return AgentSkillInfo{Name: name, Description: desc, Builtin: s.skills.IsBuiltin(name)}
}

// SaveAgent 保存智能体信息（名称/模型/系统提示词）。
// 校验：名称非空（模型可为空=跟随全局默认；系统提示词允许为空，
// 由前端提示建议填写）。builtin 标记沿用库中记录，调用方无法篡改。
func (s *Service) SaveAgent(id, name, model, systemPrompt string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("智能体名称不能为空")
	}
	row, err := s.agents.Get(id)
	if err != nil {
		return fmt.Errorf("智能体不存在: %s", id)
	}
	row.Name = name
	row.Model = strings.TrimSpace(model)
	row.SystemPrompt = systemPrompt
	return s.agents.Update(row)
}

// SetAgentSkills 整组替换智能体的技能装配（空列表 = 纯聊天智能体，合法）。
// 未知技能名在注册表层被拒绝。
func (s *Service) SetAgentSkills(id string, skillNames []string) error {
	return s.agents.SetSkills(id, skillNames)
}
