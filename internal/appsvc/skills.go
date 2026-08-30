// skills.go 是技能元数据的对外聚合：把技能注册表（internal/skill）里的
// 自描述元数据（名称/描述/参数 Schema）整理成前端可直接渲染的列表。
//
// 用途：设置页"技能管理"区（数据驱动，前端零硬编码——后端注册新技能后
// 界面自动反映）。注意 Description 取"实时值"：部署技能（deploy_run_flow）
// 的描述是动态拼接的流程清单，每次调用重新生成，流程增删改后立即反映。
package appsvc

import "encoding/json"

// SkillInfo 单个技能的展示元数据（前端技能卡片与介绍弹窗的数据源）。
type SkillInfo struct {
	Name        string `json:"name"`        // 技能名（下划线命名空间，如 ops_list_containers）
	Description string `json:"description"` // 描述全文（部署技能含动态流程清单，实时值）
	// Parameters 参数的 JSON Schema 原样透传（function-calling 定义）。
	// json.RawMessage 序列化时内嵌为真正的 JSON 对象，前端拿到的就是
	// {type, properties, required, ...} 结构，无需二次解析。
	Parameters json.RawMessage `json:"parameters"`
}

// ListSkills 返回注册表中全部技能的元数据（名称有序，由 Registry.All 保证）。
// 只读操作，不涉及持久层。
func (s *Service) ListSkills() []SkillInfo {
	all := s.skills.All()
	out := make([]SkillInfo, 0, len(all))
	for _, sk := range all {
		out = append(out, SkillInfo{
			Name:        sk.Name(),
			Description: sk.Description(), // 实时调用：动态描述（部署技能）每次都是最新值
			Parameters:  sk.Parameters(),
		})
	}
	return out
}
