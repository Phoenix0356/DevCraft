// apidocquery.go 是聊天侧只读查询技能 apidoc_query 的注入缝实现（任务 10-21，PRD D5）：
// 每个方法都是界面向导既在用的那六个查询方法（ListApiProjects / ListApiDomains /
// ListApiEntries / GetApiEntry / ListApiChangeEvents / GetApiChangeEvent）的薄投影——
// 零新 SQL、零新业务逻辑，同一套读路径两个入口（向导 & 聊天智能体）。
//
// 为什么必须存在这一层「方法名不同」的适配（而不是 app.go 直接注入方法值）：
// skill 包不能 import appsvc（appsvc/apidocskill.go 已 import skill/apidoc，反向即循环），
// 而 GetApiEntry / GetApiChangeEvent 的返回类型（ApiEntryDetail / ApiChangeEventDetail）
// 是 appsvc 类型，技能侧只能拿自己定义的最小视图——所以由 Service 实现技能包定义的
// QuerySource 接口。这是本包 DocService/ApiDocSkillCheck 既有的同款先例（投影，不是新逻辑）。
//
// 红线（PRD R3/D4）：
//   - EntryView / EventView 里没有 RawMarkdown / DocText 字段——「原文不进模型」钉死在
//     类型层，投影实现连"顺手带上"的字段都不存在；
//   - 本文件只读：绝不调用任何 Save/Commit/Delete 方法（AC6 由接口形状 + 评审共同保证）；
//   - 出网文本的掩码与字符上限在技能层 render() 统一执行（internal/sanitize）。
//
// ⚠️ 本文件的方法只在技能缝上可达，App 不新增任何导出方法 ⇒ 无需重生成 bindings
// （wails-integration.md「What does NOT need regeneration」）。

package appsvc

import (
	"context"
	"fmt"
	"log/slog"

	"DevCraft/internal/agent"
	skillapidoc "DevCraft/internal/skill/apidoc"
	"DevCraft/internal/store"
)

// ===================== QuerySource 实现（六个既有方法的薄投影）=====================

// ApiDocSkillProjects = ListApiProjects 直通（项目行不含大文本，无需裁剪）。
func (s *Service) ApiDocSkillProjects(_ context.Context) ([]store.ApiProject, error) {
	return s.ListApiProjects()
}

// ApiDocSkillDomains = ListApiDomains 直通。
func (s *Service) ApiDocSkillDomains(_ context.Context, projectID string) ([]store.ApiDomain, error) {
	return s.ListApiDomains(projectID)
}

// ApiDocSkillEntries = ListApiEntries：把技能侧的散参数拼回既有查询结构（keyword
// 过滤、internal/removed 默认排除的语义都在 ListApiEntries 里，这里不重复实现）。
func (s *Service) ApiDocSkillEntries(_ context.Context, projectID, domainID, keyword string, includeInternal, includeRemoved bool) ([]store.ApiEntry, error) {
	return s.ListApiEntries(ApiEntryQuery{
		ProjectID:       projectID,
		DomainID:        domainID,
		Keyword:         keyword,
		IncludeInternal: includeInternal,
		IncludeRemoved:  includeRemoved,
	})
}

// ApiDocSkillEntry = GetApiEntry 的投影：条目表头只留聊天回答需要的列，
// ⚠️ RawMarkdown 刻意不投影（R3 类型闸——整段接口原文绝不进模型）。
func (s *Service) ApiDocSkillEntry(_ context.Context, entryID string) (skillapidoc.EntryView, error) {
	d, err := s.GetApiEntry(entryID)
	if err != nil {
		return skillapidoc.EntryView{}, err
	}
	e := d.Entry
	v := skillapidoc.EntryView{
		ID:             e.ID,
		ProjectID:      e.ProjectID,
		DomainID:       e.DomainID,
		Name:           e.Name,
		Method:         e.Method,
		Path:           e.PathRaw,
		Description:    e.Description,
		Auth:           e.Auth,
		ContentType:    e.ContentType,
		Visibility:     e.Visibility,
		Status:         e.Status,
		ParseState:     e.ParseState,
		BasePathAssert: e.BasePathAssert,
		PathFields:     d.PathFields,
		RequestFields:  d.RequestFields,
		ResponseFields: d.ResponseFields,
		Examples:       d.Examples,
		Events:         d.Events,
	}
	for _, sc := range d.Schemas {
		v.Schemas = append(v.Schemas, skillapidoc.EntrySchemaView{
			Name:      sc.Schema.Name,
			Ambiguous: sc.Schema.Ambiguous,
			Fields:    sc.Fields,
		})
	}
	return v, nil
}

// ApiDocSkillEvents = ListApiChangeEvents：limit<=0 时取全量（技能层需要真实 total
// 才能如实报告 truncated——R2；列表 SQL 本就不带 doc_text 大列）。
func (s *Service) ApiDocSkillEvents(_ context.Context, projectID string, limit int) ([]store.ApiChangeEvent, error) {
	return s.ListApiChangeEvents(ApiEventQuery{ProjectID: projectID, Limit: limit})
}

// ApiDocSkillEvent = GetApiChangeEvent 的投影：事件头逐列搬运，
// ⚠️ 不投影 DocText（导入原文永远只走界面的 GetApiChangeEventDocText，R3/D12 凭证留档
// 是给界面核对用的，不是给模型的）。明细 diff 原样带过（§13.20：before/after 是对象，
// 由技能层序列化压平、限长）。
func (s *Service) ApiDocSkillEvent(_ context.Context, eventID int64) (skillapidoc.EventView, error) {
	d, err := s.GetApiChangeEvent(eventID)
	if err != nil {
		return skillapidoc.EventView{}, err
	}
	ev := d.Event
	v := skillapidoc.EventView{
		ID:        ev.ID,
		ProjectID: ev.ProjectID,
		Seq:       ev.Seq,
		Label:     ev.Label,
		Title:     ev.Title,
		Reason:    ev.Reason,
		Kind:      ev.Kind,
		Breaking:  ev.Breaking,
		Source:    ev.Source,
		DocRef:    ev.DocRef,
		ScopeMode: ev.ScopeMode,
		Stats:     ev.Stats,
		CreatedAt: ev.CreatedAt,
	}
	for _, it := range d.Items {
		v.Items = append(v.Items, skillapidoc.EventItemView{
			EntryID:      it.EntryID,
			DomainName:   it.DomainName,
			EntryName:    it.EntryName,
			Method:       it.Method,
			Path:         it.Path,
			ChangeType:   it.ChangeType,
			Breaking:     it.Breaking,
			DeclaredNote: it.DeclaredNote,
			Diff:         it.Diff,
		})
	}
	return v, nil
}

// 编译期核对：Service 实现查询技能的只读缝（签名漂移在此立刻失败，
// 对照 apidocskill.go:449 的 DocService 先例）。
var _ skillapidoc.QuerySource = (*Service)(nil)

// ===================== 一次性自动补装迁移（D6/R5）=====================

// settingSeedApiDocQuery 补装的幂等标记。⚠️ 必须是新键：旧键 seed.apidoc_skills_v1
// 在存量安装里已写入，MigrateApiDocSkills 的形状是「标记存在即跳过」——复用它会导致
// 升级安装永远拿不到本技能（"注册了但会话里静默调不到"，PRD D6 点名的坑）。
const settingSeedApiDocQuery = "seed.apidoc_query_v1"

// MigrateApiDocQuerySkill 把 apidoc_query 并集进内置运维 Agent（升级兼容）。
// 形状照抄 MigrateApiDocSkills / MigratePgSkills：标记存在即跳过；并集只补差额、
// 不丢用户已装技能；标记最后写（中途失败下次启动重试）；事后用户取消勾选不复活。
func (s *Service) MigrateApiDocQuerySkill() error {
	if v, ok, err := s.store.GetSetting(settingSeedApiDocQuery); err != nil {
		return err
	} else if ok && v != "" {
		return nil // 已补装过：幂等跳过
	}
	a, err := s.store.GetAgent(agent.BuiltinOpsAgentID)
	if err != nil {
		return fmt.Errorf("补装接口查询技能失败（运维 Agent 不存在）: %w", err)
	}
	seen := make(map[string]bool, len(a.Skills))
	merged := make([]string, 0, len(a.Skills)+1)
	for _, n := range a.Skills {
		seen[n] = true
		merged = append(merged, n)
	}
	added := 0
	if !seen[skillapidoc.SkillQuery] {
		merged = append(merged, skillapidoc.SkillQuery)
		added = 1
	}
	if added > 0 {
		if err := s.store.ReplaceAgentSkills(agent.BuiltinOpsAgentID, merged); err != nil {
			return fmt.Errorf("补装接口查询技能失败: %w", err)
		}
	}
	// 标记最后写：宁可重复检查，不可漏装
	if err := s.store.SetSetting(settingSeedApiDocQuery, "1"); err != nil {
		return err
	}
	slog.Info("apidoc_query 技能补装迁移完成", "agent", agent.BuiltinOpsAgentID, "added", added)
	return nil
}
