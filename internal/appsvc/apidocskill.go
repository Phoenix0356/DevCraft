// apidocskill.go 是「接口文档」chat 技能（internal/skill/apidoc）的宿主侧实现：
// 规范文本交付、体检/导入的确定性编排入口，以及破坏性导入的待审批单状态机。
//
// 依赖方向：本文件 import 技能包（拿它的 ImportInput/ImportCheck 等类型），
// 技能包绝不 import appsvc——与 appsvc/pg.go 的 PgSkillConfig/PgQuery 完全同构
// （pg.go:234-252），也由 app.go 用 svc 的方法值注入。
//
// 三条服务层红线（本文件是它们的物理位置）：
//  1. 技能路径不另写解析器：Check/Commit 直接复用界面向导同一对方法
//     PreviewApiDocImport / CommitApiDocImport（PRD D8），E-*/W-*/双 hash/diff
//     的判定权全在 internal/apidoc，这里只做类型投影；
//  2. chat 路径永远 InitMode=false 且 AllowInitOnNonEmpty=false（spec-draft §13.26）——
//     初始化灌库只属于界面向导：档位权威闸让「文档自声明 scope: init」在未勾选时一律被
//     服务层拒收，技能侧另有第二道闸（解析结果为 init 即拒执行），本层不给任何放行键；
//  3. 破坏性/删除必须过审批闸（D16）：needConfirm 为真时只创建待审批单并推卡片帧，
//     未批准绝不调用 CommitApiDocImport。审批闸的形状（取出即删除的单次消费门禁 +
//     惰性回收 + 异步执行 + panic 兜底）照搬 deploy.go:83-142 与 deploy.go:200-276，
//     有效期直接复用 approvalTTL，不新造第二套确认机制。
//
// 正文保真（D5.1）：文档全文由技能包 os.ReadFile 读盘后放进 ImportInput.DocText，
// 本层原样转交解析器；待审批单里存的是这份请求，批准后用的仍是它——
// 从读盘到落库，正文字节不经过模型一次。
package appsvc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"DevCraft/internal/agent"                    // BuiltinOpsAgentID（补装迁移的目标 Agent）
	"DevCraft/internal/apidoc"                   // SpecText 单一真相源 + Issue 词表
	skillapidoc "DevCraft/internal/skill/apidoc" // chat 技能侧的输入/输出类型
	"DevCraft/internal/skill/deploy"             // SessionIDFromCtx（会话 id 经 ctx 透传给技能）
	"DevCraft/internal/store"
)

// 接口文档导入的审批帧事件名（帧形与 deploy:* 一致：payload 带 sessionId）。
// 说明：卡片渲染属于前端后续接线（本次范围不改前端组件），帧先按契约推送。
const (
	EventApiDocApproval = "api:approval" // 导入审批卡片（项目/模式/破坏档/逐接口变更摘要）
	EventApiDocDone     = "api:done"     // 导入执行结束（状态 + 总结文本）
)

// apiDocApprovalSummaryLines 审批帧与技能回执里最多列出的变更行数
// （超出只报条数：卡片不是事件详情页，前端/模型都不该被长清单淹掉）。
const apiDocApprovalSummaryLines = 30

// ApiDocApproval 待审批的接口文档导入单（内存态，无需落库；会话绑定 + 过期时间）。
// Req 里带着读盘得到的文档全文——批准后才拿去提交，所以"未批准一行不写"
// 是由数据结构本身保证的（除了这条路没有第二个调用 Commit 的入口）。
type ApiDocApproval struct {
	ID        string              `json:"id"`
	SessionID string              `json:"sessionId"`
	Req       ApiDocImportRequest `json:"-"` // 待执行的导入请求（DocText 是磁盘原文）
	DocRef    string              `json:"docRef"`
	Project   string              `json:"project"`
	Mode      string              `json:"mode"`
	Breaking  int                 `json:"breaking"`
	NextSeq   int                 `json:"nextSeq"`
	Summary   []string            `json:"summary"` // 逐接口变更摘要（已掩码，不含正文）
	Stats     ApiDocImportStats   `json:"stats"`
	CreatedAt int64               `json:"createdAt"`
	ExpiresAt int64               `json:"expiresAt"`
}

// expired 审批单是否已过期（惰性判定，与 deploy 的 Approval.expired 同形）。
func (a *ApiDocApproval) expired() bool { return time.Now().UnixMilli() > a.ExpiresAt }

// ==================== 技能侧四个能力（chat 与向导共用同一套方法）====================

// ApiDocSkillSpec 返回规范全文（技能 apidoc_spec 的落点）。
// 与界面导出走同一个方法、同一个 Go 常量（apidoc.SpecText），
// 这就是 AC9/D15 的"字节一致"——要出现分叉只能靠有人新抄一份文本，而这里没有。
func (s *Service) ApiDocSkillSpec(_ context.Context) (string, error) {
	return s.ExportApiDocSpec()
}

// ApiDocSkillCheck 跑确定性校验 + 与库中现状的 diff 预览（技能 apidoc_check 的落点）。
// 只读：与界面向导调的是同一个 PreviewApiDocImport，全程不写任何表。
// 两个 init 放行键在本方法里硬编码为 false（§13.18），技能与模型都改不动。
func (s *Service) ApiDocSkillCheck(_ context.Context, in skillapidoc.ImportInput) (skillapidoc.ImportCheck, error) {
	pv, err := s.PreviewApiDocImport(apiDocSkillRequest(in, false))
	if err != nil {
		return skillapidoc.ImportCheck{}, err
	}
	return apiDocSkillCheckOf(pv), nil
}

// ApiDocSkillCommit 单事务写入（技能 apidoc_import 的落点）。
// 与界面向导调的是同一个 CommitApiDocImport——E-* 拒收、二次确认闸、init 闸门、
// 幂等短路、事件与批次落库全部复用，技能层零份第二实现（D8）。
//
// confirmed 标记在这里由服务端自己重算，不直接采信调用方传进来的值：
// chat 侧的 Confirmed=true 只代表「预览时判过不需要确认」，而预览与写入之间
// 库里状态可能被界面并发导入改掉。服务端复检出 needConfirm 就中止（零写入），
// 让技能回去走审批闸——D16 的人工确认闸不能建立在"模型可影响的标记"上。
func (s *Service) ApiDocSkillCommit(_ context.Context, in skillapidoc.ImportInput) (skillapidoc.ImportReceipt, error) {
	if in.Confirmed {
		pv, err := s.PreviewApiDocImport(apiDocSkillRequest(in, false))
		if err != nil {
			return skillapidoc.ImportReceipt{}, err
		}
		if pv.NeedConfirm {
			return skillapidoc.ImportReceipt{}, fmt.Errorf(
				"服务端复检：本次导入含接口删除或破坏性变更（breaking=%d），聊天路径已中止且未写入任何数据；"+
					"请重新发起导入以生成待审批单", pv.Breaking)
		}
	}
	res, err := s.CommitApiDocImport(apiDocSkillRequest(in, in.Confirmed))
	if err != nil {
		return skillapidoc.ImportReceipt{}, err
	}
	return skillapidoc.ImportReceipt{
		EventID:        res.EventID,
		Seq:            res.Seq,
		Mode:           res.Mode,
		Breaking:       res.Breaking,
		Stats:          apiDocSkillStats(res.Stats),
		Message:        res.Message,
		Notes:          append([]string(nil), res.Notes...),
		CreatedProject: res.CreatedProject,
		CreatedDomains: append([]string(nil), res.CreatedDomains...),
	}, nil
}

// ApiDocSkillSubmitApproval 创建接口文档导入的待审批单并推送审批卡片帧
// （实现技能侧 DocService 的审批提交能力，对照 SubmitDeployApproval）。
// 本方法绝不写入：它只做「再算一遍预览 → 登记单子 → 推帧」。
// 预览在这里重算而不信技能传回的结论：闸门的判据必须由服务端自己算
// （对照 D16「二次确认要在 API 层强制，不能只靠前端不显示按钮」）。
func (s *Service) ApiDocSkillSubmitApproval(ctx context.Context, in skillapidoc.ImportInput) (string, skillapidoc.ImportCheck, error) {
	sessionID := deploy.SessionIDFromCtx(ctx)
	if strings.TrimSpace(sessionID) == "" {
		return "", skillapidoc.ImportCheck{}, fmt.Errorf("内部错误: 缺少会话 ID，无法创建接口文档审批单")
	}
	req := apiDocSkillRequest(in, false)
	pv, err := s.PreviewApiDocImport(req)
	if err != nil {
		return "", skillapidoc.ImportCheck{}, err
	}
	ck := apiDocSkillCheckOf(pv)
	if !ck.OK {
		return "", ck, fmt.Errorf("接口文档校验未通过，未创建审批单")
	}
	if ck.Mode == apidoc.ModeInit {
		return "", ck, fmt.Errorf("初始化导入（scope: init）只能在界面导入向导里执行，聊天中一律拒绝")
	}
	if !pv.NeedConfirm {
		return "", ck, fmt.Errorf("本次导入不含破坏性变更或接口删除，无需审批，请直接提交")
	}
	return s.submitApiDocApproval(sessionID, req, pv)
}

// submitApiDocApproval 登记待审批单 + 推审批卡片帧（与 SubmitDeployApproval 同形：
// 加锁写入注册表前先惰性回收过期单，避免从不被批准的条目永久滞留）。
func (s *Service) submitApiDocApproval(sessionID string, req ApiDocImportRequest, pv ApiDocImportPreview) (string, skillapidoc.ImportCheck, error) {
	now := time.Now().UnixMilli()
	ap := &ApiDocApproval{
		ID:        uuid.NewString(),
		SessionID: sessionID,
		Req:       req,
		DocRef:    strings.TrimSpace(req.DocRef),
		Project:   pv.ProjectName,
		Mode:      pv.Mode,
		Breaking:  pv.Breaking,
		NextSeq:   pv.NextSeq,
		Summary:   apiDocApprovalSummary(pv.Changes),
		Stats:     pv.Stats,
		CreatedAt: now,
		ExpiresAt: now + approvalTTL.Milliseconds(),
	}
	s.apiApprovalsMu.Lock()
	for id, a := range s.apiApprovals {
		if now > a.ExpiresAt {
			delete(s.apiApprovals, id)
		}
	}
	s.apiApprovals[ap.ID] = ap
	s.apiApprovalsMu.Unlock()

	s.emit(EventApiDocApproval, map[string]any{
		"sessionId":  ap.SessionID,
		"approvalId": ap.ID,
		"docRef":     ap.DocRef,
		"project":    ap.Project,
		"mode":       ap.Mode,
		"breaking":   ap.Breaking,
		"nextSeq":    ap.NextSeq,
		"summary":    ap.Summary,
		"stats":      ap.Stats,
		"expiresAt":  ap.ExpiresAt,
	})
	slog.Info("接口文档导入审批单已创建", "approval", ap.ID, "project", ap.Project,
		"session", ap.SessionID, "breaking", ap.Breaking, "changes", len(ap.Summary))
	return ap.ID, apiDocSkillCheckOf(pv), nil
}

// takeApiDocApproval 审批门禁：取出并原子删除审批单（单次消费）。
// 照 deploy.go:128-142 —— 未找到（不存在/已批准/已拒绝）与已过期都返回错误，
// 写入路径只可能从这里进入，所以重复批准必然第二次失败。
func (s *Service) takeApiDocApproval(id string) (*ApiDocApproval, error) {
	s.apiApprovalsMu.Lock()
	defer s.apiApprovalsMu.Unlock()
	ap := s.apiApprovals[id]
	if ap == nil {
		return nil, fmt.Errorf("接口文档审批单不存在或已处理")
	}
	delete(s.apiApprovals, id) // 取出即删除（单次消费）
	if ap.expired() {
		return nil, fmt.Errorf("接口文档审批单已过期（有效期 %d 分钟），请在对话中重新发起导入", int(approvalTTL.Minutes()))
	}
	return ap, nil
}

// ApproveApiDocImport 批准接口文档导入（审批卡片"批准"按钮的落点）。
// 批准瞬间返回，真正写入在 goroutine 里做——对照 ApproveDeployment + executeApproval
// 的形状：门禁在前台消费掉单子，执行在后台，终态经 api:done 帧回报，
// 绝不让前端卡片悬着（panic 也兜底成 failed 帧）。
//
// 批准 = 人工二次确认成立，所以这里把 Confirmed 置真后提交：
// 破坏性内容唯一的放行理由就是用户刚刚点过批准，而不是模型说了"确认"。
func (s *Service) ApproveApiDocImport(id string) error {
	ap, err := s.takeApiDocApproval(id)
	if err != nil {
		return err
	}
	slog.Info("接口文档导入已批准，开始异步写入", "approval", ap.ID, "project", ap.Project,
		"breaking", ap.Breaking, "changes", len(ap.Summary))
	go s.executeApiDocImport(ap)
	return nil
}

// RejectApiDocImport 拒绝接口文档导入（审批卡片"拒绝"按钮）。
// 拒绝的单子绝不写入；不落批次记录——本模块的批次表只记真正发生过的导入。
func (s *Service) RejectApiDocImport(id string) error {
	ap, err := s.takeApiDocApproval(id)
	if err != nil {
		return err
	}
	slog.Info("接口文档导入已拒绝", "approval", ap.ID, "project", ap.Project)
	s.emit(EventApiDocDone, map[string]any{
		"sessionId":  ap.SessionID,
		"approvalId": ap.ID,
		"status":     "rejected",
		"summary":    fmt.Sprintf("接口文档导入审批单 %s 已被拒绝，未写入任何数据。", ap.ID),
	})
	return nil
}

// executeApiDocImport 批准后执行导入（goroutine 入口）。
// 与 executeApproval 同款兜底：panic 必须落终态帧，否则卡片永远悬着；
// 总结既推帧也作 assistant 消息落库，重开会话能看到结果。
func (s *Service) executeApiDocImport(ap *ApiDocApproval) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("接口文档导入 panic", "approval", ap.ID, "panic", r)
			s.finishApiDocImport(ap, "failed", fmt.Sprintf("接口文档导入遇到内部错误，已中止，未写入任何数据（审批单 %s）。", ap.ID))
		}
	}()
	req := ap.Req
	req.Confirmed = true // 用户批准即二次确认成立（D16 的服务层闸门据此放行）
	res, err := s.CommitApiDocImport(req)
	if err != nil {
		// 拒收/失败：事务已回滚，如实说明"一行都没写"
		slog.Warn("接口文档导入批准后被拒", "approval", ap.ID, "project", ap.Project, "err", err)
		s.finishApiDocImport(ap, "failed", fmt.Sprintf("接口文档导入失败（数据库零变更）: %v", err))
		return
	}
	summary := apiDocImportSummary(ap, res)
	slog.Info("接口文档导入完成（经审批）", "approval", ap.ID, "project", res.ProjectID,
		"mode", res.Mode, "seq", res.Seq, "eventId", res.EventID)
	s.finishApiDocImport(ap, "success", summary)
}

// finishApiDocImport 推终态帧并把总结作 assistant 消息落库（对照 deploy.go:261-274）。
func (s *Service) finishApiDocImport(ap *ApiDocApproval, status, summary string) {
	_ = s.store.AppendMessage(store.Message{SessionID: ap.SessionID, Role: "assistant", Content: summary})
	s.emit(EventApiDocDone, map[string]any{
		"sessionId":  ap.SessionID,
		"approvalId": ap.ID,
		"status":     status, // success | failed | rejected
		"summary":    summary,
	})
}

// apiDocImportSummary 批准后的中文总结（进会话，供用户与后续对话引用）。
func apiDocImportSummary(ap *ApiDocApproval, res ApiDocImportResult) string {
	var b strings.Builder
	if res.EventID > 0 {
		fmt.Fprintf(&b, "接口文档导入完成（审批单 %s）：项目 %s，事件 v%d，%s。",
			ap.ID, ap.Project, res.Seq, apiDocStatsText(res.Stats))
	} else {
		fmt.Fprintf(&b, "接口文档导入完成（审批单 %s）：项目 %s，%s；本次未生成变更事件。",
			ap.ID, ap.Project, apiDocStatsText(res.Stats))
	}
	if res.Message != "" {
		fmt.Fprintf(&b, "\n%s", res.Message)
	}
	for _, n := range res.Notes {
		fmt.Fprintf(&b, "\n提示: %s", n)
	}
	if res.CreatedProject {
		b.WriteString("\n本次新建了接口项目。")
	}
	for _, d := range res.CreatedDomains {
		fmt.Fprintf(&b, "\n本次新建领域: %s", d)
	}
	return b.String()
}

// ===================== 一次性自动补装迁移 =====================

// settingSeedApiDocSkills 接口文档技能补装的幂等标记（对照 settingSeedPgSkills）：
// 只在首次启动时补装一次，此后用户自由增删装配，启动流程绝不再干预。
const settingSeedApiDocSkills = "seed.apidoc_skills_v1"

// MigrateApiDocSkills 把 3 个接口文档技能补装进内置运维 Agent（升级兼容）。
//
// 为什么照抄 MigratePgSkills 的形状（pg.go:276-321）而不是新建一个专用 Agent：
//   - SeedDefaults 是 skip-if-exists（agent.go:49-66），存量安装的运维 Agent 已存在，
//     永远不会自动获得新技能；新会话又默认绑定运维 Agent（service.go NewSession），
//     不补装就意味着"技能注册了但会话里调不到"（AC8 的前半句不成立）；
//   - 并集合并只补差额：用户已装配的技能原样保留，事后在智能体管理里取消勾选
//     也不会被重启覆盖回来（标记已写过 → 直接返回）。
func (s *Service) MigrateApiDocSkills() error {
	if v, ok, err := s.store.GetSetting(settingSeedApiDocSkills); err != nil {
		return err
	} else if ok && v != "" {
		return nil // 已补装过：幂等跳过
	}
	a, err := s.store.GetAgent(agent.BuiltinOpsAgentID)
	if err != nil {
		return fmt.Errorf("补装接口文档技能失败（运维 Agent 不存在）: %w", err)
	}
	seen := make(map[string]bool, len(a.Skills))
	merged := make([]string, 0, len(a.Skills)+len(skillapidoc.SkillNames))
	for _, n := range a.Skills {
		seen[n] = true
		merged = append(merged, n)
	}
	added := 0
	for _, n := range skillapidoc.SkillNames {
		if !seen[n] {
			merged = append(merged, n)
			added++
		}
	}
	if added > 0 {
		if err := s.store.ReplaceAgentSkills(agent.BuiltinOpsAgentID, merged); err != nil {
			return fmt.Errorf("补装接口文档技能失败: %w", err)
		}
	}
	// 标记最后写：中途失败则下次启动重试（宁可重复检查，不可漏装）
	if err := s.store.SetSetting(settingSeedApiDocSkills, "1"); err != nil {
		return err
	}
	slog.Info("接口文档技能补装迁移完成", "agent", agent.BuiltinOpsAgentID, "added", added)
	return nil
}

// ===================== 类型投影与闸门口径 ====================

// apiDocSkillRequest 把技能侧输入拼成编排层请求。
// InitMode 与 AllowInitOnNonEmpty 在这里硬编码为 false（spec-draft §13.26）：
// chat 路径没有任何初始化导入的放行键。§13.26 的档位权威闸使「文档自声明 scope: init」
// 在 InitMode=false 时被服务层直接拒收（此前只有附件通道守住，向导与技能都漏着），
// 技能层那道「解析结果为 init 即拒执行」仍是第二道闸（双闸不删）。
// ⚠️ 任务 10-20 D6 只放宽「聊天附件通道」（前端直接调 PreviewApiDocImport/CommitApiDocImport，
// 不经模型，InitMode=附件条「基线导入」勾选位）；本函数对应的模型驱动 apidoc_import 技能工具
// 保持拒绝 init 一字不改——§13.26 防的就是「文档自声明 init 逃逸校验」。
// AutoFix 同为 false 零值：模型驱动的导入不做机械改写，修复器只属于附件这条确定性通道。
func apiDocSkillRequest(in skillapidoc.ImportInput, confirmed bool) ApiDocImportRequest {
	return ApiDocImportRequest{
		DocText:             in.DocText,
		DocRef:              strings.TrimSpace(in.DocRef),
		ProjectName:         strings.TrimSpace(in.ProjectName),
		Title:               strings.TrimSpace(in.Title),
		Reason:              strings.TrimSpace(in.Reason),
		Label:               strings.TrimSpace(in.Label),
		Kind:                strings.TrimSpace(in.Kind),
		Confirmed:           confirmed,
		InitMode:            false,
		AllowInitOnNonEmpty: false,
	}
}

// apiDocSkillCheckOf 预览结果 → 技能侧结论（只投影技能做判断与转述需要的字段；
// diff 载荷本身不回给模型：那是前端事件详情页要渲染的结构，模型只需身份与档位）。
func apiDocSkillCheckOf(pv ApiDocImportPreview) skillapidoc.ImportCheck {
	changes := make([]skillapidoc.ImportChange, 0, len(pv.Changes))
	for _, c := range pv.Changes {
		changes = append(changes, skillapidoc.ImportChange{
			Method: c.Method, Path: c.Path, Name: c.Name, Domain: c.Domain,
			ChangeType: c.ChangeType, Breaking: c.Breaking, ParseState: c.ParseState,
		})
	}
	return skillapidoc.ImportCheck{
		OK:          pv.OK,
		NeedConfirm: pv.NeedConfirm,
		InitBlocked: pv.InitBlocked,
		Mode:        pv.Mode,
		ProjectName: pv.ProjectName,
		Breaking:    pv.Breaking,
		NextSeq:     pv.NextSeq,
		Message:     pv.Message,
		Notes:       append([]string(nil), pv.Notes...),
		Issues:      append([]apidoc.Issue(nil), pv.Issues...),
		Stats:       apiDocSkillStats(pv.Stats),
		Changes:     changes,
	}
}

// apiDocSkillStats 统计投影（字段一一对应，Moved 在 MVP 增量导入恒 0，不进技能回执）。
func apiDocSkillStats(st ApiDocImportStats) skillapidoc.ImportStats {
	return skillapidoc.ImportStats{
		Added: st.Added, Updated: st.Updated, Removed: st.Removed,
		Restored: st.Restored, Unchanged: st.Unchanged,
	}
}

// apiDocApprovalSummary 逐接口变更摘要行（卡片与技能回执共用）。
// 只取身份与结论：method/path/领域/条目名 + 变更类型 + 破坏档；
// 正文与 diff 载荷不进帧（卡片不是事件详情页，前端要细看走 GetApiChangeEvent）。
func apiDocApprovalSummary(changes []ApiDocChangePreview) []string {
	out := make([]string, 0, len(changes)+1)
	for i, c := range changes {
		if i >= apiDocApprovalSummaryLines {
			out = append(out, fmt.Sprintf("…其余 %d 条请在导入预览/事件详情里查看", len(changes)-i))
			return out
		}
		line := fmt.Sprintf("%s %s｜%s｜领域 %s｜%s", c.Method, c.Path, c.Name, c.Domain, c.ChangeType)
		if c.Breaking > 0 {
			line += fmt.Sprintf("｜breaking=%d", c.Breaking)
		}
		if c.ParseState != "" && c.ParseState != "ok" {
			line += "｜需人工补全"
		}
		out = append(out, line)
	}
	return out
}

// 编译期核对：Service 实现技能包的依赖缝（签名漂移在此立刻失败，
// 对照 deploy.go:39-47 与 app.go 注入处的先例）。
var _ skillapidoc.DocService = (*Service)(nil)

// apiDocStatsText 统计一行话（审批总结进会话用；与技能侧回执同文案）。
func apiDocStatsText(st ApiDocImportStats) string {
	return fmt.Sprintf("新增 %d、修改 %d、删除 %d、恢复 %d、未变更 %d",
		st.Added, st.Updated, st.Removed, st.Restored, st.Unchanged)
}
