// apidoc.go 是「接口文档管理与版本变更追踪」的编排层（PR3 + PR4a 缺口补齐轮）：
// 规范导出、导入预览（只读）、导入提交（单事务）、接口/事件浏览与手工 CRUD。
//
// 契约：.trellis/tasks/10-18-api-docs/spec-draft.md（§13 实现注记必须遵守）与 prd.md（D1–D18）。
// 核心红线：
//   - 解析/diff/校验全部复用 internal/apidoc（确定性程序解析是唯一权威闸门，D5）；
//   - Commit 全链路单事务（store.WithTx），持 Tx 期间只用 Tx 面读写——
//     SetMaxOpenConns(1) 下混用 Store 方法会死锁（spec-draft §13.14）；
//   - 发事件的判据是「字段级 diff 载荷非空」，不是 hash 不等（§13.1：占位符改名
//     会改 semantic_hash 但 diff 为空，属 cosmetic）；
//   - 强制闸门在服务层（不能只靠前端不显示按钮，D16）：E-* 拒收 / 破坏性二次确认 /
//     scope:init 只允许空项目或显式勾选（§13.12）；
//   - 落库 api_schemas.owner_scope 必须是 entry id，解析层 SchemaDef.OwnerScope 是
//     identity_key，两层语义不同、必须映射（§13.13）；
//   - 命名结构歧义（同名不同构 → ambiguous）的 diff 项走降级通道：NeedsConfirm +
//     rule=struct-ambiguous，不参与 E-BREAKING-DENIED / E-DIFF-MISMATCH-UNCHANGED 的
//     「作者违约」裁决（10-21 D2）；无歧义的真实破坏照常整批拒收，闸门不许修哑；
//   - 手工编辑（SaveApiEntry / SaveApiDomain）不产生变更事件（PRD 已裁决，事件链会有洞，已知并接受）；
//     且走服务端读-改-写：store 的 upsert/UPDATE 都是全列写，补齐责任在服务端而不在调用方
//     （§13.19「手工编辑不重算双 hash」由「前端自律」升级为「服务端保证」）；
//   - 面向用户的错误一律中文短句；slog kv 日志只在本层打（store 层无日志）。
//
// 幂等裁决（AC2 与 AC3「声明修改但无变化」的边界，两轮契约合并解读）：
//   - 整份文档算不出任何变更明细（added/updated/removed/restored 全空）→ 视为幂等重导，
//     陈旧的声明（新增但已存在且一致 / 修改但无差异 / 删除但已删）降级为中文提示，
//     零写入返回「无变更」；仅措辞/示例差异时只同步条目文本、不发事件（AC4）；
//   - 文档存在真实变更明细时 → 声明集合与程序 diff 严格交叉校验（D6），任何不一致
//     整批拒收（E-ADDED-EXISTS / E-UPDATED-NO-DIFF / E-DIFF-MISMATCH-* / E-DELETE-NOT-FOUND）。
package appsvc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"DevCraft/internal/apidoc"
	"DevCraft/internal/store"
)

// ===================== DTO（全部纯可序列化：无 func/interface/chan/指针字段，
// json tag 一律 camelCase；可空语义用 -1/''/0 哨兵，照 PgSettings/PgSettingsView 成对先例）=====================

// ApiDomainOverride 导入向导里的领域改名/合并指令（按解析出的原始标题定位）。
type ApiDomainOverride struct {
	RawTitle  string `json:"rawTitle"`  // 文档里的原始 ## 标题（定位键）
	Name      string `json:"name"`      // 改名后的领域名（空 = 不改名）
	MergeInto string `json:"mergeInto"` // 合并目标领域名（非空时优先于 Name）
}

// ApiDocImportRequest 导入请求（预览与提交共用同一份输入）。
type ApiDocImportRequest struct {
	DocText             string              `json:"docText"`             // 文档全文（必填）
	DocRef              string              `json:"docRef"`              // 来源路径（可空，仅溯源）
	ProjectName         string              `json:"projectName"`         // 覆盖 meta.project（空则用 meta 值）
	InitMode            bool                `json:"initMode"`            // ⚠️ 解析档位的唯一权威（spec-draft §13.26，取代 §13.18）：界面向导「初始化模式」勾选 / 聊天附件条「基线导入」勾选 / 技能通道恒 false。文档声明 scope 与它不一致（任一向）一律拒收；仅当文档**没有** meta.scope 时它决定兜底档位
	Confirmed           bool                `json:"confirmed"`           // 破坏性/删除的二次确认标记（D16 服务层强制）
	AllowInitOnNonEmpty bool                `json:"allowInitOnNonEmpty"` // 允许对非空项目执行 init（§13.12 闸门放行键）。⚠️ 聊天附件通道恒传 false——§13.12「init 只允许空项目」的后端闸门语义一字不改（D6 只放宽「不再在入口硬拒 chat」）；模型驱动的 apidoc_import 技能工具在 apiDocSkillRequest 里硬编码 false，不给任何放行键（§13.18）
	AutoFix             bool                `json:"autoFix"`             // 聊天附件通道（任务 10-20）：先跑确定性修复器 apidoc.Repair 再重新完整校验（D2/D4）；界面向导不勾选=行为完全不变；技能通道永不置真（模型驱动的导入不做机械改写）
	DomainOverrides     []ApiDomainOverride `json:"domainOverrides"`     // 领域改名/合并
	Title               string              `json:"title"`               // 覆盖 meta.change_title（空则用 meta 值）
	Reason              string              `json:"reason"`              // 覆盖 meta.change_note
	Label               string              `json:"label"`               // 覆盖 meta.version_tag
	Kind                string              `json:"kind"`                // 覆盖 meta.change_type（feature|fix|refactor|docs）
}

// ApiDocImportStats 导入统计（预览与结果共用；事件头只落前五列，Unchanged 不进事件）。
type ApiDocImportStats struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Removed   int `json:"removed"`
	Moved     int `json:"moved"` // MVP 增量导入不产生（改名走 1 删 + 1 增，spec-draft §8.5），列留全
	Restored  int `json:"restored"`
	Unchanged int `json:"unchanged"`
}

// ApiDocChangePreview 逐接口变更预览（「变更」分栏的一行）。
type ApiDocChangePreview struct {
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	Name         string            `json:"name"`
	Domain       string            `json:"domain"`       // 应用 override 后的领域终名
	Declared     string            `json:"declared"`     // 文档 **变更** 声明原文（新增/修改/删除/未变更/''）
	ChangeType   string            `json:"changeType"`   // added|updated|removed|moved|restored|unchanged
	Breaking     int               `json:"breaking"`     // 三态：0 无 | 1 破坏 | 2 需人工确认
	Diff         []apidoc.DiffItem `json:"diff"`         // 字段级 diff 载荷（updated/removed 行非空）
	DeclaredNote string            `json:"declaredNote"` // **变更点** 原文
	ParseState   string            `json:"parseState"`   // ok|partial|unparsed
	DocOnly      bool              `json:"docOnly"`      // 仅措辞/示例同步，不进事件明细
}

// ApiDomainCandidate 领域候选（预览的改名/合并确认区）。
type ApiDomainCandidate struct {
	RawTitle   string `json:"rawTitle"`
	Name       string `json:"name"`       // 应用 override 后的终名
	MergedInto string `json:"mergedInto"` // 非空 = 该领域被并入 Name 所指领域
	Internal   bool   `json:"internal"`   // （内部）章节 → visibility=internal
	EntryCount int    `json:"entryCount"`
}

// ApiDocMetaPreview 文档自己声明的 meta 回显（导入向导的「文档写了什么」栏）。
// 「为什么改」= changeNote 是本模块的核心价值，预览必须能核对，而不是只给一个空覆盖输入框。
// 散文文档没有 ```apidoc-meta 块时各字段为空串（零指针约定），前端据此显示「文档未声明」。
// 注意是「原样回显」：不做 req 覆盖后的合并值（覆盖后的生效值见 message/stats 与提交结果）。
type ApiDocMetaPreview struct {
	Project     string `json:"project"`     // meta.project → 项目标识（D13）
	Service     string `json:"service"`     // meta.service → 项目显示名
	BasePath    string `json:"basePath"`    // meta.base_path（只做断言，绝不剥离，D18）
	Scope       string `json:"scope"`       // incremental | init | full
	ChangeType  string `json:"changeType"`  // feature | fix | refactor | docs
	ChangeTitle string `json:"changeTitle"` // 事件标题
	ChangeNote  string `json:"changeNote"`  // 为什么改（事件 reason）
	VersionTag  string `json:"versionTag"`  // 需求单号/发布标签（事件 label，D7）
}

// ApiDocFix 确定性修复器（apidoc.Repair）的一处机械修复（任务 10-20，PRD D2/D4）。
// 卡片逐条展示：第几行、改了什么、是否待人工核对、为什么改。修复只允许白名单四类
// （bullet-form / fence-close / pos-column / cross-ref-expand）；语义问题不出现在这里，
// 只出现在剩余 Issues 清单里。Kind/Line 坐标系约定见 internal/apidoc/repair.go 文件头。
type ApiDocFix struct {
	Kind        string `json:"kind"`
	Line        int    `json:"line"`
	Before      string `json:"before"`
	After       string `json:"after"`
	NeedsReview bool   `json:"needsReview"` // 推断类（补位置列 / 搬运含推断列的表）恒 true，沿用 §10 W-POS-INFERRED 语义
	Note        string `json:"note"`
}

// ApiDocImportPreview 导入预览（只读产物；「文档自身问题」与「变更」分栏，PRD Risk 5）。
// Fixes 仅在请求带 AutoFix（聊天附件通道）时非空：修复清单必须可见，不能静默改完就导（D4）。
type ApiDocImportPreview struct {
	OK          bool                  `json:"ok"`          // 可否进入提交（无 E-* 且未被 init 闸门拦截）
	NeedConfirm bool                  `json:"needConfirm"` // 提交时必须带 confirmed=true（D16）
	InitBlocked bool                  `json:"initBlocked"` // scope:init 闸门拦截（项目非空且未勾选放行）
	Breaking    int                   `json:"breaking"`    // 聚合三态
	Mode        string                `json:"mode"`        // incremental|init
	ProjectName string                `json:"projectName"`
	NextSeq     int                   `json:"nextSeq"` // 将分配的事件版本号（0 = 本次不生成事件）
	Meta        ApiDocMetaPreview     `json:"meta"`
	Stats       ApiDocImportStats     `json:"stats"`
	Issues      []apidoc.Issue        `json:"issues"`  // 文档自身 bug + 与库矛盾（E-*/W-*，含行号；AutoFix 时为修复后重新完整校验的剩余问题）
	Fixes       []ApiDocFix           `json:"fixes"`   // 已自动修复清单（未启用 AutoFix = 空数组）
	FixedText   string                `json:"fixedText"` // 修复稿全文（D7/AC6「下载修正后的 md」的数据源）：仅 AutoFix 通道非空；本轮零修复时 = 原文（前端按 fixes 是否为空决定要不要给下载按钮）。未启用 AutoFix = 空串（零指针约定）
	Changes     []ApiDocChangePreview `json:"changes"` // 变更分栏
	Domains     []ApiDomainCandidate  `json:"domains"`
	Notes       []string              `json:"notes"`   // 中文提示（幂等重导的陈旧声明等，不新增错误码）
	Message     string                `json:"message"` // 一句话汇总
}

// ApiDocImportResult 提交结果。EventID/Seq 为 0 = 本次未生成事件（哨兵，零指针约定）。
// Notes/CreatedProject/CreatedDomains 让结果卡能如实说清「本次新建了什么、幂等短路时降级了什么提示」，
// 语义与预览的 notes 同源（陈旧声明降级、init 闸门提示等）。
// FixCount 为本次 AutoFix 通道实际应用的机械修复数（0 = 未启用或文档无需修复）；
// 修复清单全文随事件 meta.fixes 落库（事件不生成时不落），结果卡要显示「已自动修复 N 处」。
type ApiDocImportResult struct {
	EventID        int64             `json:"eventId"`
	Seq            int               `json:"seq"`
	ProjectID      string            `json:"projectId"`
	Mode           string            `json:"mode"`
	Breaking       int               `json:"breaking"`
	Stats          ApiDocImportStats `json:"stats"`
	FixCount       int               `json:"fixCount"`       // 已自动修复处数（AutoFix 通道）
	FixedText      string            `json:"fixedText"`      // 实际入库的那份文本全文 = 修复稿（D7/AC6：结果卡可下载；未启用 AutoFix = 空串）
	Notes          []string          `json:"notes"`          // 中文提示（与预览同源；无提示 = 空数组）
	CreatedProject bool              `json:"createdProject"` // 本次是否新建了项目
	CreatedDomains []string          `json:"createdDomains"` // 本次新建的领域终名（含 DomainOverrides 改名/合并后）
	Message        string            `json:"message"`        // 中文汇总（「无变更」等在此体现）
}

// ApiEntryQuery 接口条目列表查询（R9）。默认过滤 internal 与 removed。
type ApiEntryQuery struct {
	ProjectID       string `json:"projectId"` // 必填
	DomainID        string `json:"domainId"`  // 空 = 全项目
	Keyword         string `json:"keyword"`   // 名称或路径模糊匹配（大小写不敏感）
	IncludeInternal bool   `json:"includeInternal"`
	IncludeRemoved  bool   `json:"includeRemoved"`
}

// ApiEventQuery 变更事件列表查询（AC7：seq DESC + 按 label 聚合过滤）。
type ApiEventQuery struct {
	ProjectID string `json:"projectId"` // 必填
	Label     string `json:"label"`     // 空 = 全部
	Limit     int    `json:"limit"`     // <=0 = 不限
}

// ApiSchemaDetail 命名结构 + 其字段（条目详情用）。
type ApiSchemaDetail struct {
	Schema store.ApiSchema  `json:"schema"`
	Fields []store.ApiField `json:"fields"`
}

// ApiEntryDetail 接口条目详情：条目 + 请求/返回字段 + 命名结构 + 示例与 curl
// + 该接口历史事件（反向关联，R9/AC7）。
type ApiEntryDetail struct {
	Entry          store.ApiEntry           `json:"entry"`
	PathFields     []store.ApiField         `json:"pathFields"`
	RequestFields  []store.ApiField         `json:"requestFields"`
	ResponseFields []store.ApiField         `json:"responseFields"`
	Schemas        []ApiSchemaDetail        `json:"schemas"`
	Examples       []store.ApiExample       `json:"examples"`
	Events         []store.ApiEntryEventRef `json:"events"`
}

// ApiChangeEventDetail 事件详情 = 事件头 + 明细（含 diff 载荷）；doc_text 永远
// 单独走 GetApiChangeEventDocText（列表/详情 SQL 不带大文本列）。
type ApiChangeEventDetail struct {
	Event store.ApiChangeEvent  `json:"event"`
	Items []store.ApiChangeItem `json:"items"`
}

// ===================== 导入编排的读侧抽象 =====================

// apiImportReader 导入规划所需的最小读侧：*store.Store（Preview 只读）与
// *store.Tx（Commit 大事务内）都满足——事务内一律走 Tx 面，绝不混用 Store
// 方法（SetMaxOpenConns(1) 死锁红线，spec-draft §13.14）。
type apiImportReader interface {
	FindApiProjectByName(name string) (store.ApiProject, bool, error)
	FindApiEntryByIdentity(projectID, identityKey string) (store.ApiEntry, bool, error)
	ListApiFields(ownerKind, ownerID string) ([]store.ApiField, error)
	ListApiSchemasByOwner(projectID, ownerScope string) ([]store.ApiSchema, error)
	ListApiDomains(projectID string) ([]store.ApiDomain, error)
	CountActiveApiEntries(projectID string) (int, error)
}

var (
	_ apiImportReader = (*store.Store)(nil)
	_ apiImportReader = (*store.Tx)(nil)
)

// 条目写入动作（包内常量对，不落库）。
const (
	apiActionNone    = ""
	apiActionAdd     = "add"
	apiActionUpdate  = "update"
	apiActionDocSync = "docSync" // 仅措辞/示例同步（AC4：不发事件）
	apiActionRemove  = "remove"
	apiActionRestore = "restore"
)

// 陈旧声明类型（整份文档零变更时降级为提示，否则升级为 E-*，见文件头「幂等裁决」）。
const (
	staleNone         = ""
	staleAddedExists  = "addedExists"
	staleUpdateNoDiff = "updateNoDiff"
	staleDeleteGone   = "deleteGone"
)

// ===================== 规范导出（D15/R12 单一真相源）=====================

// ExportApiDocSpec 返回接口变更文档规范全文（apidoc.SpecText 单一真相源：
// 界面导出 api-doc-spec.md 与 skill prompt 字节一致，AC9）。
func (s *Service) ExportApiDocSpec() (string, error) {
	return apidoc.SpecText, nil
}

// ===================== 导入预览（只读，绝不写库）=====================

// PreviewApiDocImport 解析 → 收集 Issue → 与库中现有定义算 diff → 返回预览。
// 只读：全程仅 Store 查询，不持事务、不写任何表。解析/校验失败不返回 Go error，
// 而是落在预览的 Issues 里（OK=false）——向导要展示结构化问题清单；
// 只有基础设施错误（库读失败、无法确定项目名）才走 error。
func (s *Service) PreviewApiDocImport(req ApiDocImportRequest) (ApiDocImportPreview, error) {
	pv := ApiDocImportPreview{
		Issues:  []apidoc.Issue{},
		Fixes:   []ApiDocFix{},
		Changes: []ApiDocChangePreview{},
		Domains: []ApiDomainCandidate{},
		Notes:   []string{},
	}
	if strings.TrimSpace(req.DocText) == "" {
		return pv, fmt.Errorf("文档内容不能为空")
	}
	// AutoFix（聊天附件通道）：先跑确定性修复器，再按修复稿重新完整校验（D3/D4）。
	// 修复清单进 pv.Fixes；修复稿全文进 pv.FixedText（下载修正稿用，D7/AC6）；
	// 剩余 E-*/W-* 照常进 pv.Issues。
	fixed, fixes := apiDocApplyAutoFix(req)
	req.DocText = fixed
	pv.Fixes = fixes
	if req.AutoFix {
		pv.FixedText = fixed
	}
	doc := apidoc.Parse(req.DocText, apiDocParseOptions(req))
	pv.Mode = doc.Mode
	pv.Meta = apiDocMetaPreview(doc) // 文档自己声明的 meta 原样回显（校验失败也要给，用户得知道文档写了啥）
	pv.ProjectName = apiDocProjectName(req, doc)
	pv.Issues = append(pv.Issues, doc.Issues...)
	pv.Domains = apiDomainCandidates(doc, req.DomainOverrides)
	if deny := apiDocInitAuthority(req, doc); deny != "" {
		// §13.26：解析档位以请求标志为唯一权威，双向冲突一律拒收（所有通道同一条闸）。
		// 拒绝时把文档声明的 scope 一并回显 + 指路怎么放行，不能只丢一句错误。
		pv.Notes = append(pv.Notes, deny)
		pv.Message = "解析档位与勾选冲突，已拒绝导入（基线导入 / 初始化模式需显式勾选）"
		return pv, nil
	}
	projectName := pv.ProjectName
	if doc.HasErrors() {
		pv.Message = fmt.Sprintf("文档校验未通过，共 %d 个阻断问题", len(doc.Errors()))
		return pv, nil
	}
	if projectName == "" {
		return pv, fmt.Errorf("无法确定项目名：请在 meta 的 project 字段或导入请求的 projectName 中提供")
	}
	plan, err := s.planApiDocImport(s.store, req, doc, projectName)
	if err != nil {
		return pv, err
	}
	pv.Issues = append(pv.Issues, plan.issues...)
	pv.Notes = append(pv.Notes, plan.notes...)
	pv.Stats = plan.stats
	pv.Breaking = plan.breaking
	pv.NeedConfirm = plan.needConfirm
	pv.InitBlocked = plan.initBlocked
	pv.Changes = plan.changePreviews()
	pv.OK = len(plan.issues) == 0 && !plan.initBlocked
	if plan.hasEvent {
		pv.NextSeq = 1
		if plan.projectID != "" {
			if evs, err := s.store.ListApiChangeEvents(plan.projectID, 1); err == nil && len(evs) > 0 {
				pv.NextSeq = evs[0].Seq + 1
			}
		}
	}
	pv.Message = plan.summaryMessage()
	return pv, nil
}

// ===================== 导入提交（单事务写入）=====================

// CommitApiDocImport 校验通过后在单事务里完成：项目 upsert → 领域 upsert（应用
// 改名/合并）→ 条目 upsert/软删/恢复 → 字段、示例、命名结构整组替换 → 事件
// （seq = MAX(seq)+1 同事务）→ 明细 → 导入批次。任一步失败整体回滚（AC3 零残留）。
//
// 强制闸门（全部在服务层，chat 路径也绕不过去）：
//  0. AutoFix（聊天附件通道）先跑确定性修复器再重新完整校验（D3/D4），修复稿全文随
//     结果卡的 fixedText 带回前端（D7/AC6 下载修正稿，不写回原文件）；
//  0.5 解析档位权威闸（§13.26，取代 §13.18）：req.InitMode 是档位的唯一权威——
//     文档自声明 scope: init 而未勾选 → 拒收；勾选了而文档声明 incremental/full →
//     同样拒收；文档无 meta.scope 时勾选才决定兜底档位。**所有通道同一条闸**，
//     界面向导与模型驱动技能都不再能被文档自声明绕过严格校验。
//     ⚠️ §13.12 的 init 闸门（下面第 3 条）语义一字未动：附件通道 AllowInitOnNonEmpty 恒
//     false；模型驱动的 apidoc_import 技能工具（apiDocSkillRequest）仍硬编码两个 init 键为
//     false 并保持「解析结果为 init 即拒执行」那道第二道闸——本闸让它「聊天拒绝 init」的意图
//     真正成立（此前只有附件通道守住），本次不放宽、不改错对象；
//  1. 任何 E-* issue → 拒收（错误信息带前若干条 code+行号+want），零写入；
//  2. needConfirm（聚合 breaking ∈ {1,2} 或明细含 removed）且未 confirmed → 拒写（D16）；
//  3. scope:init 仅当项目下无任何 active 条目或 AllowInitOnNonEmpty 时放行（§13.12）；
//  4. diff 载荷全空 → 不生成事件、不改定义，返回「无变更」（AC2 幂等，§13.1）；
//  5. init 模式不产生事件（R10）。
func (s *Service) CommitApiDocImport(req ApiDocImportRequest) (ApiDocImportResult, error) {
	if strings.TrimSpace(req.DocText) == "" {
		return ApiDocImportResult{}, fmt.Errorf("文档内容不能为空")
	}
	// AutoFix（聊天附件通道）：与 Preview 调用同一个 apidoc.Repair（确定性 → 两边修复
	// 结果必然一致），随后按修复稿重新完整校验；入库文本 = 修复稿本身，
	// doc_fingerprint / 事件 doc_text 全部对应修复稿（AC7：不存在第三份"AI 改写版"）。
	fixed, fixes := apiDocApplyAutoFix(req)
	req.DocText = fixed
	doc := apidoc.Parse(req.DocText, apiDocParseOptions(req))
	if deny := apiDocInitAuthority(req, doc); deny != "" {
		slog.Warn("接口文档导入被拒：解析档位与勾选冲突", "project", apiDocProjectName(req, doc),
			"declared", doc.Meta.Scope, "initMode", req.InitMode, "autoFix", req.AutoFix)
		return ApiDocImportResult{}, errors.New(deny)
	}
	if doc.HasErrors() {
		slog.Warn("接口文档导入被拒：解析校验未通过", "project", apiDocProjectName(req, doc),
			"mode", doc.Mode, "errors", len(doc.Errors()))
		return ApiDocImportResult{}, apiDocIssueError("接口文档校验未通过", doc.Errors())
	}
	projectName := apiDocProjectName(req, doc)
	if projectName == "" {
		return ApiDocImportResult{}, fmt.Errorf("无法确定项目名：请在 meta 的 project 字段或导入请求的 projectName 中提供")
	}
	label := apiDocFirstNonEmpty(strings.TrimSpace(req.Label), doc.Meta.VersionTag)
	kind := apiDocFirstNonEmpty(strings.TrimSpace(req.Kind), doc.Meta.ChangeType)
	if kind != "" && kind != store.ApiEventKindFeature && kind != store.ApiEventKindFix &&
		kind != store.ApiEventKindRefactor && kind != store.ApiEventKindDocs {
		return ApiDocImportResult{}, fmt.Errorf("变更类型不合法: %s（应为 feature/fix/refactor/docs）", kind)
	}
	title := apiDocFirstNonEmpty(strings.TrimSpace(req.Title), doc.Meta.ChangeTitle, doc.Title, "接口文档导入")
	reason := apiDocFirstNonEmpty(strings.TrimSpace(req.Reason), doc.Meta.ChangeNote)
	fingerprint := apiDocFingerprint(req.DocText)

	// 修复清单要能在 commit 之后仍然可见：序列化进事件 meta（api_change_events 的 JSON
	// 扩展槽——api_import_batches 没有 meta 列，而 §13 纪律是不给老表加列）。
	// init 导入不生成事件（R10），此时批次行的 doc_fingerprint 仍记录修复稿的 sha256，
	// 修复明细只随预览/结果卡在会话里呈现（AC7 断言面见 apiDocEventMeta）。
	res := ApiDocImportResult{Notes: []string{}, CreatedDomains: []string{}, FixCount: len(fixes)}
	// 修正稿全文随结果卡带回前端（D7/AC6「下载修正后的 md」）：入参已经是修复稿，
	// 所以这里给出去的就是真正落库的那份字节（与 doc_fingerprint / 事件 doc_text 同源）。
	if req.AutoFix {
		res.FixedText = fixed
	}
	err := s.store.WithTx(func(tx *store.Tx) error {
		// 规划（事务内读，读到自己未提交的写；绝不触碰 Store 方法，§13.14）
		plan, err := s.planApiDocImport(tx, req, doc, projectName)
		if err != nil {
			return err
		}
		// 闸门 1：与库状态矛盾的 E-*（声明交叉校验，D6）
		if len(plan.issues) > 0 {
			return apiDocIssueError("接口文档校验未通过", plan.issues)
		}
		// 闸门 2：破坏性/删除的二次确认（D16）
		if plan.needConfirm && !req.Confirmed {
			return fmt.Errorf("本次导入包含破坏性变更或接口删除（breaking=%d），请在预览确认后携带确认标记重新提交", plan.breaking)
		}
		// 闸门 3：scope:init 只允许空项目或显式放行（§13.12）
		if plan.initBlocked {
			return fmt.Errorf("项目已有 %d 个接口条目，初始化导入仅允许在空项目执行；确需重新初始化请勾选「允许非空项目初始化」", plan.activeCount)
		}
		res.Mode = plan.mode
		res.Breaking = plan.breaking
		res.Stats = plan.stats
		res.ProjectID = plan.projectID
		// 中文提示如实带回（幂等短路时这是用户唯一能看到的解释，结果卡要显示）
		if len(plan.notes) > 0 {
			res.Notes = append(res.Notes, plan.notes...)
		}
		// 闸门 4：diff 载荷全空 → 零写入（AC2；空事务提交，写库语句数为 0）
		if !plan.anyWrite {
			res.Message = "无变更：文档与库中接口定义一致，未写入任何数据"
			return nil
		}

		// 1. 项目 upsert（按 name）
		if plan.projectID == "" {
			np := store.ApiProject{Name: plan.projectName, Display: plan.display, BasePath: plan.basePath}
			if err := tx.CreateApiProject(&np); err != nil {
				return fmt.Errorf("创建接口项目失败: %w", err)
			}
			plan.projectID = np.ID
			res.CreatedProject = true
		}
		res.ProjectID = plan.projectID
		// 2. 领域 upsert（应用 DomainOverrides 的改名/合并；按终名去重）
		domainIDs := make(map[string]string, len(plan.domainWrites))
		for _, dw := range plan.domainWrites {
			d := store.ApiDomain{ProjectID: plan.projectID, Name: dw.name, Internal: dw.internal,
				NotesRaw: dw.notesRaw, Sort: dw.sort}
			if err := tx.SaveApiDomain(&d); err != nil {
				return fmt.Errorf("写入接口领域失败: %w", err)
			}
			if dw.created {
				// 新建领域名 = 最终落库名（override 改名/合并后的终名）
				res.CreatedDomains = append(res.CreatedDomains, dw.name)
			}
			domainIDs[dw.name] = d.ID
		}
		// 3. 条目 upsert / 软删 / 恢复 + 字段、示例、命名结构整组替换；顺带收集事件明细
		var items []*store.ApiChangeItem
		docSynced := 0
		for i := range plan.entries {
			p := &plan.entries[i]
			domainName := plan.domainFinalName(p.domainIdx)
			switch p.action {
			case apiActionAdd, apiActionUpdate, apiActionDocSync, apiActionRestore:
				entryID, err := s.writeApiEntry(tx, plan, p, domainIDs[domainName])
				if err != nil {
					return err
				}
				if p.action == apiActionDocSync {
					docSynced++
				}
				if p.changeType != "" {
					items = append(items, newChangeItem(plan, p, entryID, domainName))
				}
			case apiActionRemove:
				if err := tx.MarkApiEntryRemoved(p.existing.ID); err != nil {
					return fmt.Errorf("软删接口条目失败: %w", err)
				}
				items = append(items, newChangeItem(plan, p, p.existing.ID, domainName))
			}
		}
		// 4. 文档级命名结构（owner_scope=''）整组替换
		if len(plan.docSchemas) > 0 {
			if err := tx.DeleteApiSchemasByScope(plan.projectID, ""); err != nil {
				return fmt.Errorf("清理文档级命名结构失败: %w", err)
			}
			for _, sc := range plan.docSchemas {
				if err := writeApiSchema(tx, plan.projectID, "", sc); err != nil {
					return err
				}
			}
		}
		// 5. 事件（seq = MAX(seq)+1 同事务）+ 明细；init 不产生事件（R10）
		if plan.hasEvent {
			ev := store.ApiChangeEvent{
				ProjectID: plan.projectID, Label: label, Title: title, Reason: reason, Kind: kind,
				Breaking: plan.breaking, Source: store.ApiEventSourceImport, DocRef: strings.TrimSpace(req.DocRef),
				ScopeMode: plan.mode,
				Stats: store.ApiEventStats{Added: plan.stats.Added, Updated: plan.stats.Updated,
					Removed: plan.stats.Removed, Moved: plan.stats.Moved, Restored: plan.stats.Restored},
				Meta:    apiDocEventMeta(fingerprint, fixes),
				DocText: req.DocText, // D12：输入凭证，全文留档（AutoFix 通道 = 修复稿本身）
			}
			if err := tx.InsertApiChangeEvent(&ev); err != nil {
				return fmt.Errorf("写入变更事件失败: %w", err)
			}
			for _, it := range items {
				it.EventID = ev.ID
			}
			if err := tx.InsertApiChangeItems(items); err != nil {
				return fmt.Errorf("写入变更事件明细失败: %w", err)
			}
			res.EventID = ev.ID
			res.Seq = ev.Seq
		}
		// 6. 导入批次（keys_json 只记录本次声明的 key 集；full 模式逻辑二期，D17）
		batch := store.ApiImportBatch{
			ProjectID:      plan.projectID,
			ScopeIdentity:  plan.projectName + "|" + plan.basePath + "|" + doc.Title,
			DeclaredMode:   plan.mode,
			DocFingerprint: fingerprint,
			Keys:           apiDocDeclaredKeys(doc),
			SummaryCheck:   doc.Summary,
		}
		if err := tx.InsertApiImportBatch(&batch); err != nil {
			return fmt.Errorf("写入导入批次失败: %w", err)
		}
		switch {
		case plan.hasEvent:
			res.Message = fmt.Sprintf("导入成功：新增 %d、修改 %d、删除 %d、恢复 %d，已生成事件 v%d",
				plan.stats.Added, plan.stats.Updated, plan.stats.Removed, plan.stats.Restored, res.Seq)
		case plan.initMode:
			res.Message = fmt.Sprintf("初始化导入完成：新增 %d、覆盖 %d、恢复 %d；初始化不产生变更事件",
				plan.stats.Added, plan.stats.Updated, plan.stats.Restored)
		default:
			res.Message = fmt.Sprintf("无接口变更：仅同步了 %d 个条目的说明/示例文本，未生成事件", docSynced)
		}
		return nil
	})
	if err != nil {
		slog.Warn("接口文档导入被拒", "project", projectName, "mode", doc.Mode, "err", err)
		return ApiDocImportResult{}, err
	}
	slog.Info("接口文档导入完成", "project", projectName, "mode", res.Mode,
		"added", res.Stats.Added, "updated", res.Stats.Updated, "removed", res.Stats.Removed,
		"restored", res.Stats.Restored, "eventId", res.EventID, "seq", res.Seq,
		"createdProject", res.CreatedProject, "createdDomains", len(res.CreatedDomains),
		"notes", len(res.Notes))
	return res, nil
}

func newChangeItem(plan *apiDocPlan, p *apiEntryPlan, entryID, domainName string) *store.ApiChangeItem {
	name := p.ep.Name
	if name == "" {
		name = p.existing.Name
	}
	return &store.ApiChangeItem{
		ProjectID: plan.projectID, EntryID: entryID, DomainName: domainName, EntryName: name,
		Method: p.ep.Method, Path: p.ep.PathRaw, ChangeType: p.changeType, Breaking: p.breaking,
		Visibility: p.visibility, Diff: p.diff, DeclaredNote: p.ep.ChangePoint,
	}
}

// ===================== 导入规划（Preview 与 Commit 共用；只读）=====================

// apiDomainPlan 一个解析领域 + override 后的落库形态。
type apiDomainPlan struct {
	rawTitle   string
	name       string // 解析器归一名
	finalName  string // 应用 override 后的终名（落库名）
	mergedInto string
	internal   bool
	notesRaw   string
	sort       int
	entryCount int
	existingID string // 库里同名领域 id（'' = 写阶段新建）
}

// apiDomainWrite 去重后的领域写入行（合并场景多个解析领域落同一行）。
type apiDomainWrite struct {
	name     string
	internal bool
	notesRaw string
	sort     int
	created  bool // 库里查不到同名领域行 → 本次写库会新建（结果卡 CreatedDomains 判据）
}

// apiEntryPlan 一个接口条目的导入裁决。
type apiEntryPlan struct {
	ep          *apidoc.Endpoint
	domainIdx   int
	existing    store.ApiEntry
	hasExisting bool
	action      string // apiAction*
	changeType  string // ''（不进明细）| added|updated|removed|restored
	diff        []apidoc.DiffItem
	breaking    int
	visibility  string
	sort        int
	stale       string // stale*：陈旧声明，批次级裁决转提示或转 E-*
}

// apiDocPlan 一份文档的完整导入计划。
type apiDocPlan struct {
	projectID   string // '' = 项目尚不存在（提交时创建）
	projectName string
	display     string
	basePath    string
	mode        string
	initMode    bool

	domains      []apiDomainPlan
	domainWrites []apiDomainWrite
	entries      []apiEntryPlan
	docSchemas   []apidoc.SchemaDef // 文档级命名结构（OwnerIdx == -1）

	stats       ApiDocImportStats
	breaking    int
	hasEvent    bool
	needConfirm bool
	initBlocked bool
	activeCount int
	anyWrite    bool

	issues []apidoc.Issue // 编排层 E-*（对照库状态产生，report.go 词表）
	notes  []string       // 中文提示（不新增错误码，§13.10）
}

// planApiDocImport 解析结果 × 库中现状 → 导入计划。rdr 是 Store（预览）或 Tx（提交）。
func (s *Service) planApiDocImport(rdr apiImportReader, req ApiDocImportRequest, doc *apidoc.Document, projectName string) (*apiDocPlan, error) {
	// base_path：解析器已归一到 doc.BasePath（meta.base_path 优先、散文「**基础路径**」
	// 兜底，冲突出 W，§13.17）——这里只认这一个来源，不再另做 meta 兜底。
	basePath := doc.BasePath
	plan := &apiDocPlan{
		projectName: projectName,
		mode:        doc.Mode,
		initMode:    doc.Mode == apidoc.ModeInit,
		display:     strings.TrimSpace(doc.Meta.Service),
		basePath:    basePath,
	}
	if plan.display == "" {
		plan.display = projectName
	}
	proj, found, err := rdr.FindApiProjectByName(projectName)
	if err != nil {
		return nil, fmt.Errorf("读取接口项目失败: %w", err)
	}
	if found {
		plan.projectID = proj.ID
		if plan.basePath == "" {
			plan.basePath = proj.BasePath
		}
	}
	// 领域：解析 + override 归并 + 库里同名定位
	plan.domains = resolveApiDomainPlans(doc, req.DomainOverrides)
	if plan.projectID != "" {
		existing, err := rdr.ListApiDomains(plan.projectID)
		if err != nil {
			return nil, fmt.Errorf("读取接口领域失败: %w", err)
		}
		byName := make(map[string]string, len(existing))
		for _, d := range existing {
			byName[d.Name] = d.ID
		}
		for i := range plan.domains {
			plan.domains[i].existingID = byName[plan.domains[i].finalName]
		}
	}
	seenDomain := map[string]int{}
	for _, d := range plan.domains {
		if idx, ok := seenDomain[d.finalName]; ok {
			w := &plan.domainWrites[idx]
			w.internal = w.internal || d.internal
			if d.sort < w.sort {
				w.sort = d.sort
			}
			if w.notesRaw == "" {
				w.notesRaw = d.notesRaw
			}
			continue
		}
		seenDomain[d.finalName] = len(plan.domainWrites)
		plan.domainWrites = append(plan.domainWrites, apiDomainWrite{
			name: d.finalName, internal: d.internal, notesRaw: d.notesRaw, sort: d.sort,
			created: d.existingID == ""})
	}
	// init 闸门预检（§13.12；提交时在事务内以同一计划强制）
	if plan.initMode && !req.AllowInitOnNonEmpty && plan.projectID != "" {
		n, err := rdr.CountActiveApiEntries(plan.projectID)
		if err != nil {
			return nil, fmt.Errorf("统计接口条目失败: %w", err)
		}
		plan.activeCount = n
		if n > 0 {
			plan.initBlocked = true
			plan.notes = append(plan.notes,
				fmt.Sprintf("项目已有 %d 个接口条目，初始化导入将被拒绝；确需重新初始化请勾选「允许非空项目初始化」", n))
		}
	}
	// 文档级命名结构（OwnerIdx == -1）
	for _, sc := range doc.Schemas {
		if sc.OwnerIdx < 0 {
			plan.docSchemas = append(plan.docSchemas, sc)
		}
	}
	// 逐接口裁决
	for i, ep := range doc.Endpoints {
		p := apiEntryPlan{ep: ep, domainIdx: ep.DomainIdx, sort: i + 1, visibility: doc.VisibilityOf(ep)}
		if plan.projectID != "" {
			e, f, err := rdr.FindApiEntryByIdentity(plan.projectID, ep.IdentityKey)
			if err != nil {
				return nil, fmt.Errorf("读取接口条目失败: %w", err)
			}
			p.existing, p.hasExisting = e, f
		}
		if plan.initMode {
			// init：库里没有 → 新增；已有 → 覆盖定义并记「未变更」；软删 → 恢复（R10）
			switch {
			case !p.hasExisting:
				p.action = apiActionAdd
				plan.stats.Added++
			case p.existing.Status == store.ApiEntryStatusRemoved:
				p.action = apiActionRestore
				plan.stats.Restored++
			default:
				if s.entryNeedsWrite(plan, &p) {
					p.action = apiActionUpdate
					plan.stats.Updated++
				} else {
					plan.stats.Unchanged++
				}
			}
		} else if err := s.planDeclaredEntry(rdr, plan, &p); err != nil {
			return nil, err
		}
		plan.entries = append(plan.entries, p)
	}
	// 批次级裁决：有真实明细 → 陈旧声明升级 E-*（D6 交叉校验）；
	// 全文档零明细 → 幂等重导（AC2），陈旧声明降级中文提示，仅允许文本同步。
	hasItems := false
	for i := range plan.entries {
		if plan.entries[i].changeType != "" {
			hasItems = true
			break
		}
	}
	for i := range plan.entries {
		p := &plan.entries[i]
		if p.stale == staleNone {
			continue
		}
		key := p.ep.Method + " " + p.ep.PathRaw
		if hasItems {
			switch p.stale {
			case staleAddedExists:
				plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrAddedExists,
					Line: p.ep.Line, Snippet: key, Want: "「变更：新增」仅用于库中不存在的接口",
					Hint: "库中已存在该接口且与文档一致；若确有修改请声明「变更：修改」并写变更点"})
			case staleUpdateNoDiff:
				plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrUpdatedNoDiff,
					Line: p.ep.Line, Snippet: key, Want: "声明「修改」必须存在程序可识别的字段级变化",
					Hint: "程序对照库中定义未算出任何差异；只改说明/示例请声明「未变更」"})
			case staleDeleteGone:
				plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrDeleteNotFound,
					Line: p.ep.Line, Snippet: key, Want: "「变更：删除」要求接口在库中处于未删除状态",
					Hint: "该接口已处于删除状态，无需重复删除"})
			}
			continue
		}
		switch p.stale {
		case staleAddedExists:
			plan.notes = append(plan.notes, "声明「新增」但库中已存在且一致，按未变更处理："+key)
		case staleUpdateNoDiff:
			plan.notes = append(plan.notes, "声明「修改」但程序未算出变化，按未变更处理："+key)
		case staleDeleteGone:
			plan.notes = append(plan.notes, "声明「删除」但已处于删除状态，跳过："+key)
		}
		if p.hasExisting && p.existing.Status == store.ApiEntryStatusActive {
			if s.entryNeedsWrite(plan, p) {
				p.action = apiActionDocSync
			}
		}
		plan.stats.Unchanged++
	}
	// 事件与确认闸（判据 = 明细非空，不是 hash 不等，§13.1；init 不产生事件，R10）
	plan.hasEvent = hasItems && !plan.initMode
	if plan.hasEvent {
		hasRemoved := false
		for i := range plan.entries {
			b := plan.entries[i].breaking
			if b == store.ApiBreakingYes {
				plan.breaking = store.ApiBreakingYes
			} else if b == store.ApiBreakingConfirm && plan.breaking != store.ApiBreakingYes {
				plan.breaking = store.ApiBreakingConfirm
			}
			if plan.entries[i].changeType == store.ApiChangeTypeRemoved {
				hasRemoved = true
			}
		}
		plan.needConfirm = plan.breaking == store.ApiBreakingYes || plan.breaking == store.ApiBreakingConfirm || hasRemoved
	}
	for i := range plan.entries {
		if plan.entries[i].action != apiActionNone {
			plan.anyWrite = true
			break
		}
	}
	return plan, nil
}

// planDeclaredEntry 规范模式下按 **变更** 声明做「声明 × 程序 diff」双向交叉校验（D6）。
// 结果写回 p（action/changeType/diff/breaking/stale）或 plan（issues/stats）；
// 只有库读取失败才返回 error（文档矛盾一律落 plan.issues，不中断规划）。
func (s *Service) planDeclaredEntry(rdr apiImportReader, plan *apiDocPlan, p *apiEntryPlan) error {
	ep := p.ep
	key := ep.Method + " " + ep.PathRaw
	switch ep.Change {
	case apidoc.ChangeAdded:
		switch {
		case !p.hasExisting:
			p.action = apiActionAdd
			p.changeType = store.ApiChangeTypeAdded
			if ep.BreakingDecl == 1 {
				p.breaking = store.ApiBreakingYes // 人工声明破坏而启发式非破坏 → 接受（人比规则懂业务）
			}
			plan.stats.Added++
		case p.existing.Status == store.ApiEntryStatusRemoved:
			p.action = apiActionRestore
			p.changeType = store.ApiChangeTypeRestored // 软删条目重新出现 → restored（§8.4）
			plan.stats.Restored++
		default:
			before, err := s.rebuildEndpoint(rdr, plan.projectID, p.existing)
			if err != nil {
				return err
			}
			if diff := apidoc.DiffEndpoints(before, ep); len(diff) > 0 {
				// 声明新增但库里已有且内容不同 → 立即拒收（不论其他接口有无变更）
				plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrAddedExists,
					Line: ep.Line, Snippet: key, Want: "「变更：新增」仅用于库中不存在的接口",
					Hint: "库中已存在该接口且内容与文档不同；若为修改请声明「变更：修改」并写变更点"})
			} else {
				p.stale = staleAddedExists // 完全一致 → 交给批次级裁决（AC2 幂等 vs D6 交叉校验）
			}
		}
	case apidoc.ChangeUpdated:
		if !p.hasExisting || p.existing.Status == store.ApiEntryStatusRemoved {
			hint := "库中没有该接口；若为新接口请声明「变更：新增」"
			if p.hasExisting {
				hint = "该接口已处于删除状态；请先恢复接口，或声明「变更：新增」重新入库"
			}
			plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrUpdatedNotFound,
				Line: ep.Line, Snippet: key, Want: "「变更：修改」要求接口已存在于库中", Hint: hint})
			return nil
		}
		before, err := s.rebuildEndpoint(rdr, plan.projectID, p.existing)
		if err != nil {
			return err
		}
		diff := apidoc.DiffEndpoints(before, ep)
		if len(diff) == 0 {
			p.stale = staleUpdateNoDiff // 声明修改但程序算出无变化 → 批次级裁决
			return nil
		}
		p.action = apiActionUpdate
		p.changeType = store.ApiChangeTypeUpdated
		p.diff = diff
		p.breaking = apidoc.AggregateBreaking(diff)
		noteStructAmbiguous(plan, key, diff)
		if ep.BreakingDecl == 1 && p.breaking == apidoc.BreakingNo {
			p.breaking = store.ApiBreakingYes
		}
		if ep.BreakingDecl == 0 && !apidoc.ValidateBreakingOverride(0, diff) {
			plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrBreakingDenied,
				Line: ep.Line, Snippet: key, Want: "breaking: false 仅在程序判定非破坏时允许",
				Hint: "文档声明非破坏，但字段级 diff 判定为破坏（breaking=" + strconv.Itoa(p.breaking) + "）；请去掉该声明或修正变更内容"})
		}
		plan.stats.Updated++
	case apidoc.ChangeRemoved:
		if !p.hasExisting {
			plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrDeleteNotFound,
				Line: ep.Line, Snippet: key, Want: "「变更：删除」要求接口存在于库中", Hint: "库中找不到该接口，无法删除"})
			return nil
		}
		if p.existing.Status == store.ApiEntryStatusRemoved {
			p.stale = staleDeleteGone
			return nil
		}
		before, err := s.rebuildEndpoint(rdr, plan.projectID, p.existing)
		if err != nil {
			return err
		}
		p.action = apiActionRemove
		p.changeType = store.ApiChangeTypeRemoved
		p.breaking = store.ApiBreakingConfirm // D16：明细含 removed 必须二次确认
		// §7 removed 行例外：载荷存被删接口「请求必填参数 + 响应字段清单」最后一次最小片段
		p.diff = removedDiffPayload(before)
		plan.stats.Removed++
	case apidoc.ChangeUnchanged:
		if !p.hasExisting || p.existing.Status == store.ApiEntryStatusRemoved {
			hint := "库中没有该接口；「未变更」的全量重述要求接口已入库"
			if p.hasExisting {
				hint = "该接口已处于删除状态，与「未变更」声明矛盾"
			}
			plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrUpdatedNotFound,
				Line: ep.Line, Snippet: key, Want: "「变更：未变更」要求接口已存在且未删除", Hint: hint})
			return nil
		}
		before, err := s.rebuildEndpoint(rdr, plan.projectID, p.existing)
		if err != nil {
			return err
		}
		diff := apidoc.DiffEndpoints(before, ep)
		// ambiguous 命名结构的降级项不参与「作者违约」裁决（D2）：程序自己都认不出该用
		// 哪套结构定义时，没资格说「声明未变更却存在字段级差异」。真实差异照旧拒收。
		noteStructAmbiguous(plan, key, diff)
		if real := apidoc.AuthoritativeItems(diff); len(real) > 0 {
			// 声明集合与程序 diff 集合不一致（逐项列两边结果，spec-draft §9）
			ops := make([]string, 0, len(real))
			for _, d := range real {
				ops = append(ops, d.Section+" "+d.Key+" "+d.Op)
			}
			plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrDiffMismatchUnc,
				Line: ep.Line, Snippet: key, Want: "声明「未变更」的接口不得存在字段级差异",
				Hint: "文档声明：未变更；程序 diff：" + strings.Join(ops, "、") + "。请声明「变更：修改」并写变更点"})
			return nil
		}
		if s.entryNeedsWrite(plan, p) {
			p.action = apiActionDocSync // 仅措辞/示例变化：更新条目原文、不发事件（AC4）
		}
		plan.stats.Unchanged++
	default:
		// strict 解析已保证声明合法（E-CHANGE-MISSING/E-CHANGE-ENUM 挡在前面），防御性兜底
		plan.issues = append(plan.issues, apidoc.Issue{Level: apidoc.LevelError, Code: apidoc.ErrChangeEnum,
			Line: ep.Line, Snippet: key, Want: "变更 ∈ 新增 / 修改 / 删除 / 未变更", Hint: ""})
	}
	return nil
}

// noteStructAmbiguous 命名结构歧义降级项的中文提示（D2：预览/结果卡要说清为什么没按破坏处理）。
// 不新增错误码（W/E 码表封闭）——这类差异仍随明细落库、仍需人工确认，只是不拿它断言作者违约。
func noteStructAmbiguous(plan *apiDocPlan, key string, diff []apidoc.DiffItem) {
	names := map[string]bool{}
	n := 0
	for _, d := range diff {
		if !apidoc.IsStructAmbiguous(d) {
			continue
		}
		n++
		// key 形如 <Schema>.<field> 或 <Schema>，取结构名供人去文档里定位
		if i := strings.Index(d.Key, "."); i > 0 {
			names[d.Key[:i]] = true
		} else {
			names[d.Key] = true
		}
	}
	if n == 0 {
		return
	}
	list := make([]string, 0, len(names))
	for _, s := range sortedAmbiguousSchemaNames(names) {
		list = append(list, "「"+s+"」")
	}
	plan.notes = append(plan.notes, key+"：命名结构 "+strings.Join(list, "、")+
		" 存在同名不同构的多套定义（歧义），"+strconv.Itoa(n)+
		" 条字段差异无法按名对齐，已降级为需人工确认，不参与破坏性判定")
}

// sortedAmbiguousSchemaNames 结构名排序，保证同一份文档每次导入的 notes 逐字一致（确定性）。
func sortedAmbiguousSchemaNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// entryNeedsWrite 判断既有条目是否需要重写（定义列逐一对照；hash 只是快速通道，
// 占位符改名等 cosmetic 也要能触发 path_raw 同步）。
func (s *Service) entryNeedsWrite(plan *apiDocPlan, p *apiEntryPlan) bool {
	e, ep := p.existing, p.ep
	if e.SemanticHash != ep.SemanticHash || e.DocHash != ep.DocHash {
		return true
	}
	if e.Name != ep.Name || e.PathRaw != ep.PathRaw || e.ContentType != ep.ContentType ||
		e.Auth != ep.Auth || e.Visibility != p.visibility || e.Description != ep.Description ||
		e.ParseState != ep.ParseState || e.RawMarkdown != ep.RawMarkdown ||
		e.BasePathAssert != plan.basePath ||
		e.Status != store.ApiEntryStatusActive || e.DeletedAt != 0 {
		return true
	}
	domainID := ""
	if p.domainIdx >= 0 && p.domainIdx < len(plan.domains) {
		domainID = plan.domains[p.domainIdx].existingID
	}
	if domainID == "" {
		return true // 终名领域行尚不存在（新建/改名/合并）→ 条目必须重挂
	}
	return e.DomainID != domainID
}

// rebuildEndpoint 从库中现状回建 before 端点（diff 的对照侧）。
// 只回建 SemanticHash/DiffEndpoints 用到的槽位（字段/命名结构/meta 列）；
// Examples 与 TableHeaders 只进 doc_hash，而 doc_hash 直接比对库中存量列，无需回建。
func (s *Service) rebuildEndpoint(rdr apiImportReader, projectID string, e store.ApiEntry) (*apidoc.Endpoint, error) {
	ep := &apidoc.Endpoint{
		Name: e.Name, Method: e.Method, PathRaw: e.PathRaw, IdentityKey: e.IdentityKey,
		ContentType: e.ContentType, Auth: e.Auth, Description: e.Description,
		ParseState: e.ParseState, SemanticHash: e.SemanticHash, DocHash: e.DocHash,
	}
	fields, err := rdr.ListApiFields(store.ApiOwnerEndpoint, e.ID)
	if err != nil {
		return nil, fmt.Errorf("读取接口字段失败: %w", err)
	}
	for _, f := range fields {
		fld := apidoc.Field{Name: f.Name, TypeRaw: f.TypeRaw, TypeKey: f.TypeKey, Required: f.Required,
			Position: f.Position, Default: f.DefaultValue, Desc: f.Description}
		switch f.Kind {
		case store.ApiFieldKindPath:
			if fld.Position == "" {
				fld.Position = apidoc.PosPath
			}
			ep.PathFields = append(ep.PathFields, fld)
		case store.ApiFieldKindResponse:
			ep.ResponseFields = append(ep.ResponseFields, fld)
		default: // request | body | header → 请求侧（Position 以存量列为准，空则按 kind 兜底）
			if fld.Position == "" {
				switch f.Kind {
				case store.ApiFieldKindBody:
					fld.Position = apidoc.PosBody
				case store.ApiFieldKindHeader:
					fld.Position = apidoc.PosHeader
				default:
					fld.Position = apidoc.PosQuery
				}
			}
			ep.RequestFields = append(ep.RequestFields, fld)
		}
	}
	schemas, err := rdr.ListApiSchemasByOwner(projectID, e.ID)
	if err != nil {
		return nil, fmt.Errorf("读取命名结构失败: %w", err)
	}
	for _, sc := range schemas {
		sd := apidoc.SchemaDef{Name: sc.Name, OwnerScope: e.IdentityKey, BodyHash: sc.BodyHash,
			Ambiguous: sc.Ambiguous, RawMarkdown: sc.RawMarkdown}
		sfs, err := rdr.ListApiFields(store.ApiOwnerSchema, sc.ID)
		if err != nil {
			return nil, fmt.Errorf("读取命名结构字段失败: %w", err)
		}
		for _, f := range sfs {
			sd.Fields = append(sd.Fields, apidoc.Field{Name: f.Name, TypeRaw: f.TypeRaw, TypeKey: f.TypeKey,
				Required: f.Required, Position: f.Position, Default: f.DefaultValue, Desc: f.Description})
		}
		ep.Schemas = append(ep.Schemas, sd)
	}
	return ep, nil
}

// ===================== 写库辅助（只在 Commit 大事务内调用）=====================

// writeApiEntry 条目 upsert + 字段/示例/命名结构整组替换。返回条目 id。
func (s *Service) writeApiEntry(tx *store.Tx, plan *apiDocPlan, p *apiEntryPlan, domainID string) (string, error) {
	ep := p.ep
	e := store.ApiEntry{
		ID: p.existing.ID, ProjectID: plan.projectID, DomainID: domainID,
		Name: ep.Name, Method: ep.Method, PathRaw: ep.PathRaw, IdentityKey: ep.IdentityKey,
		BasePathAssert: plan.basePath, ContentType: ep.ContentType, Auth: ep.Auth,
		Visibility: p.visibility, SemanticHash: ep.SemanticHash, DocHash: ep.DocHash,
		Description: ep.Description, ParseState: ep.ParseState, RawMarkdown: ep.RawMarkdown,
		Status: store.ApiEntryStatusActive, DeletedAt: 0, Sort: p.sort, CreatedAt: p.existing.CreatedAt,
	}
	if err := tx.SaveApiEntry(&e); err != nil {
		return "", fmt.Errorf("写入接口条目失败: %w", err)
	}
	if err := tx.ReplaceApiFields(store.ApiOwnerEndpoint, e.ID, endpointApiFields(ep)); err != nil {
		return "", fmt.Errorf("写入接口字段失败: %w", err)
	}
	if err := tx.ReplaceApiExamples(store.ApiOwnerEndpoint, e.ID, endpointApiExamples(ep)); err != nil {
		return "", fmt.Errorf("写入接口示例失败: %w", err)
	}
	// 命名结构整组替换：先清空该条目作用域再重建。
	// §13.13：解析层 OwnerScope 是 identity_key，落库必须是 entry id。
	if err := tx.DeleteApiSchemasByScope(plan.projectID, e.ID); err != nil {
		return "", fmt.Errorf("清理命名结构失败: %w", err)
	}
	for _, sc := range ep.Schemas {
		if err := writeApiSchema(tx, plan.projectID, e.ID, sc); err != nil {
			return "", err
		}
	}
	return e.ID, nil
}

// writeApiSchema 落一个命名结构（ownerScope = entry id 或 ” 文档级）+ 其字段。
func writeApiSchema(tx *store.Tx, projectID, ownerScope string, sc apidoc.SchemaDef) error {
	row := store.ApiSchema{ProjectID: projectID, Name: sc.Name, OwnerScope: ownerScope,
		RawMarkdown: sc.RawMarkdown, BodyHash: sc.BodyHash, Ambiguous: sc.Ambiguous}
	if err := tx.SaveApiSchema(&row); err != nil {
		return fmt.Errorf("写入命名结构失败: %w", err)
	}
	fields := make([]store.ApiField, 0, len(sc.Fields))
	seen := map[string]bool{}
	for _, f := range sc.Fields {
		if f.Name == "" || seen[f.Name] {
			continue // UNIQUE(owner_kind,owner_id,kind,name) 去重保护
		}
		seen[f.Name] = true
		fields = append(fields, store.ApiField{Kind: store.ApiFieldKindBody, Name: f.Name,
			TypeRaw: f.TypeRaw, TypeKey: f.TypeKey, Required: f.Required, Position: f.Position,
			DefaultValue: f.Default, Description: f.Desc})
	}
	if err := tx.ReplaceApiFields(store.ApiOwnerSchema, row.ID, fields); err != nil {
		return fmt.Errorf("写入命名结构字段失败: %w", err)
	}
	return nil
}

// endpointApiFields 解析字段 → 库行。kind 取「位置」：Path→path、Query→request、
// Body→body、Header→header、响应→response（与 UNIQUE(owner_kind,owner_id,kind,name)
// 配合，跨位置同名字段不冲突；回建时按 kind+position 精确还原分组）。
func endpointApiFields(ep *apidoc.Endpoint) []store.ApiField {
	out := make([]store.ApiField, 0, len(ep.PathFields)+len(ep.RequestFields)+len(ep.ResponseFields))
	seen := map[string]bool{}
	push := func(kind string, f apidoc.Field) {
		k := kind + "\x00" + f.Name
		if f.Name == "" || seen[k] {
			return // 同表重复解析器已报 E-PARAM-DUP；这里只兜 UNIQUE 索引的最后一道
		}
		seen[k] = true
		out = append(out, store.ApiField{Kind: kind, Name: f.Name, TypeRaw: f.TypeRaw, TypeKey: f.TypeKey,
			Required: f.Required, Position: f.Position, DefaultValue: f.Default, Description: f.Desc})
	}
	for _, f := range ep.PathFields {
		if f.Position == "" {
			f.Position = apidoc.PosPath
		}
		push(store.ApiFieldKindPath, f)
	}
	for _, f := range ep.RequestFields {
		push(apiRequestFieldKind(f.Position), f)
	}
	for _, f := range ep.ResponseFields {
		push(store.ApiFieldKindResponse, f)
	}
	return out
}

func apiRequestFieldKind(position string) string {
	switch position {
	case apidoc.PosBody:
		return store.ApiFieldKindBody
	case apidoc.PosHeader:
		return store.ApiFieldKindHeader
	case apidoc.PosPath:
		return store.ApiFieldKindPath
	}
	return store.ApiFieldKindRequest // Query 与未知值
}

// endpointApiExamples 示例与 curl 原文整组落库（永不结构化、不参与 diff，D9）。
func endpointApiExamples(ep *apidoc.Endpoint) []store.ApiExample {
	out := make([]store.ApiExample, 0, len(ep.Examples))
	for i, ex := range ep.Examples {
		out = append(out, store.ApiExample{Kind: ex.Kind, Language: ex.Language, Content: ex.Content, Sort: i + 1})
	}
	return out
}

// removedDiffPayload 被删接口的「请求必填参数 + 响应字段清单」最后一次最小片段
// （spec-draft §7 removed 行例外——「不存快照」的唯一局部例外，否则删除事件在 UI 上是空壳）。
// 必填未知的请求参数按保守口径收录（宁多勿漏）。
func removedDiffPayload(before *apidoc.Endpoint) []apidoc.DiffItem {
	items := make([]apidoc.DiffItem, 0, 8)
	frag := func(f apidoc.Field) apidoc.FieldFragment {
		return apidoc.FieldFragment{Name: f.Name, Type: f.TypeRaw, Required: f.Required,
			Position: f.Position, Default: f.Default, Desc: f.Desc}
	}
	for _, f := range before.PathFields {
		if f.Required == apidoc.RequiredNo {
			continue
		}
		items = append(items, apidoc.DiffItem{Section: apidoc.SectionPathParams, Key: f.Name,
			Op: apidoc.OpRemoved, Before: frag(f), Breaking: true, Rule: "path-parameter-removed"})
	}
	for _, f := range before.RequestFields {
		if f.Required == apidoc.RequiredNo {
			continue
		}
		items = append(items, apidoc.DiffItem{Section: apiSectionOfPosition(f.Position), Key: f.Name,
			Op: apidoc.OpRemoved, Before: frag(f), NeedsConfirm: true, Rule: "request-parameter-removed"})
	}
	for _, f := range before.ResponseFields {
		items = append(items, apidoc.DiffItem{Section: apidoc.SectionResponseBody, Key: f.Name,
			Op: apidoc.OpRemoved, Before: frag(f), Breaking: true, Rule: "response-property-removed"})
	}
	return items
}

func apiSectionOfPosition(pos string) string {
	switch pos {
	case apidoc.PosPath:
		return apidoc.SectionPathParams
	case apidoc.PosQuery:
		return apidoc.SectionRequestQuery
	case apidoc.PosHeader:
		return apidoc.SectionRequestHeader
	}
	return apidoc.SectionRequestBody
}

// ===================== 计划 → 预览/文案 =====================

func (plan *apiDocPlan) domainFinalName(domainIdx int) string {
	if domainIdx < 0 || domainIdx >= len(plan.domains) {
		return ""
	}
	return plan.domains[domainIdx].finalName
}

func (plan *apiDocPlan) changePreviews() []ApiDocChangePreview {
	out := make([]ApiDocChangePreview, 0, len(plan.entries))
	for i := range plan.entries {
		p := &plan.entries[i]
		ct := p.changeType
		if ct == "" {
			ct = "unchanged"
		}
		diff := p.diff
		if diff == nil {
			diff = []apidoc.DiffItem{}
		}
		out = append(out, ApiDocChangePreview{
			Method: p.ep.Method, Path: p.ep.PathRaw, Name: p.ep.Name,
			Domain: plan.domainFinalName(p.domainIdx), Declared: p.ep.Change,
			ChangeType: ct, Breaking: p.breaking, Diff: diff, DeclaredNote: p.ep.ChangePoint,
			ParseState: p.ep.ParseState, DocOnly: p.action == apiActionDocSync,
		})
	}
	return out
}

func (plan *apiDocPlan) summaryMessage() string {
	switch {
	case len(plan.issues) > 0:
		return fmt.Sprintf("文档校验未通过，共 %d 个阻断问题，不能导入", len(plan.issues))
	case plan.initBlocked:
		return fmt.Sprintf("初始化导入被拒绝：项目已有 %d 个接口条目", plan.activeCount)
	case !plan.anyWrite:
		return "无变更：文档与库中接口定义一致，提交也不会写入任何数据"
	case plan.initMode:
		return fmt.Sprintf("初始化导入预览：将写入 %d 个接口（新增 %d、覆盖 %d、恢复 %d），不产生变更事件",
			plan.stats.Added+plan.stats.Updated+plan.stats.Restored, plan.stats.Added, plan.stats.Updated, plan.stats.Restored)
	case plan.hasEvent:
		msg := fmt.Sprintf("将生成变更事件：新增 %d、修改 %d、删除 %d、恢复 %d",
			plan.stats.Added, plan.stats.Updated, plan.stats.Removed, plan.stats.Restored)
		if plan.needConfirm {
			msg += fmt.Sprintf("；包含破坏性变更或删除（breaking=%d），提交需二次确认", plan.breaking)
		}
		return msg
	default:
		return "无接口变更：仅同步说明/示例文本，不生成变更事件"
	}
}

// resolveApiDomainPlans 解析领域 + 应用 override（改名/合并；单层解析，不做递归链）。
func resolveApiDomainPlans(doc *apidoc.Document, overrides []ApiDomainOverride) []apiDomainPlan {
	plans := make([]apiDomainPlan, 0, len(doc.Domains))
	for _, d := range doc.Domains {
		plans = append(plans, apiDomainPlan{rawTitle: d.RawTitle, name: d.Name, finalName: d.Name,
			internal: d.Internal, notesRaw: d.NotesRaw, sort: d.Sort, entryCount: len(d.EndpointIdx)})
	}
	byRaw := make(map[string]int, len(plans))
	for i := range plans {
		if _, dup := byRaw[plans[i].rawTitle]; !dup {
			byRaw[plans[i].rawTitle] = i
		}
	}
	for _, ov := range overrides {
		i, ok := byRaw[strings.TrimSpace(ov.RawTitle)]
		if !ok {
			continue
		}
		if m := strings.TrimSpace(ov.MergeInto); m != "" {
			plans[i].mergedInto = m
			continue
		}
		if n := strings.TrimSpace(ov.Name); n != "" {
			plans[i].finalName = n
		}
	}
	// 合并目标解析：MergeInto 可写目标领域的原始标题或归一名；都匹配不上按字面值使用
	for i := range plans {
		t := plans[i].mergedInto
		if t == "" {
			continue
		}
		resolved := t
		if j, ok := byRaw[t]; ok && j != i {
			resolved = plans[j].finalName
			if plans[j].mergedInto != "" {
				resolved = plans[j].name
			}
		} else {
			for j := range plans {
				if j != i && plans[j].mergedInto == "" && (plans[j].name == t || plans[j].finalName == t) {
					resolved = plans[j].finalName
					break
				}
			}
		}
		plans[i].finalName = resolved
	}
	return plans
}

// apiDomainCandidates 预览的领域候选区（不依赖库状态）。
func apiDomainCandidates(doc *apidoc.Document, overrides []ApiDomainOverride) []ApiDomainCandidate {
	plans := resolveApiDomainPlans(doc, overrides)
	out := make([]ApiDomainCandidate, 0, len(plans))
	for _, p := range plans {
		out = append(out, ApiDomainCandidate{RawTitle: p.rawTitle, Name: p.finalName,
			MergedInto: p.mergedInto, Internal: p.internal, EntryCount: p.entryCount})
	}
	return out
}

// apiDocMetaPreview 原样回显文档声明的 meta（不做 req 覆盖合并、不做 init 兜底）。
// doc.Meta.Present 为 false（散文文档）时各字段本就是空串——零指针约定，前端按空串显示「文档未声明」。
func apiDocMetaPreview(doc *apidoc.Document) ApiDocMetaPreview {
	m := doc.Meta
	return ApiDocMetaPreview{
		Project:     m.Project,
		Service:     m.Service,
		BasePath:    m.BasePath,
		Scope:       m.Scope,
		ChangeType:  m.ChangeType,
		ChangeTitle: m.ChangeTitle,
		ChangeNote:  m.ChangeNote,
		VersionTag:  m.VersionTag,
	}
}

// apiDocParseOptions 请求标志 → 解析兜底档位：勾选初始化 → init，否则规范严格模式。
// ⚠️ 解析器内部仍是「meta.scope 优先」（apidoc.resolveMode，本次不动解析器），
// 所以这里的 opts.Mode 只是「文档没写 scope 时」的兜底；一旦 meta.scope 与请求标志
// 冲突，由 apiDocInitAuthority 在请求层拒收（§13.26），不做静默取舍。
func apiDocParseOptions(req ApiDocImportRequest) apidoc.Options {
	if req.InitMode {
		return apidoc.Options{Mode: apidoc.ModeInit}
	}
	return apidoc.Options{Mode: apidoc.ModeIncremental}
}

// apiDocApplyAutoFix 聊天附件通道的确定性修复前置（任务 10-20，D3/D4/D5）：
// AutoFix 为真时调用 apidoc.Repair —— Preview 与 Commit 调的是同一个纯函数、同一份输入，
// 确定性保证两边结果必然一致（不存在「预览修 A、提交修 B」的第二份实现）。
// 修复后必须重新完整校验（D4）：调用方拿着返回的文本照常 Parse，修完仍有 E-* 一律拒收。
// 未勾选 AutoFix 时原样返回，界面向导与技能通道的行为一个字节都不变。
func apiDocApplyAutoFix(req ApiDocImportRequest) (string, []ApiDocFix) {
	if !req.AutoFix {
		return req.DocText, []ApiDocFix{}
	}
	fixed, fixes, _ := apidoc.Repair(req.DocText)
	out := make([]ApiDocFix, 0, len(fixes))
	for _, f := range fixes {
		out = append(out, ApiDocFix{Kind: f.Kind, Line: f.Line, Before: f.Before,
			After: f.After, NeedsReview: f.NeedsReview, Note: f.Note})
	}
	return fixed, out
}

// apiDocInitAuthority 解析档位的权威闸（spec-draft §13.26，取代 §13.18 的分工说法）：
// **请求标志 req.InitMode 是档位的唯一权威**，文档 meta 的 scope 不许覆盖它。
// 解析器 apidoc.resolveMode 仍然「meta 优先」（它不该知道调用方是谁，本次不动解析器），
// 冲突判定落在请求层（本函数），Preview 与 Commit 两条路径共用同一判据。
//
// 为什么必须有这道闸（实测过的漏洞）：`resolveMode` 里 meta.scope 覆盖请求标志，于是
// 只要文档自声明 `scope: init`，即使界面的「初始化模式」勾选框根本没勾，也照样拿到宽松
// 解析、跳过 `位置` 列与变更声明等严格校验——界面向导与模型驱动技能都漏着。
//
// 三条规则（双向对称，否则勾选形同虚设）：
//   - 文档声明 init 而 req.InitMode == false → 拒收；
//   - req.InitMode == true 而文档声明 incremental/full → 同样拒收；
//   - 文档**没有** meta.scope（现存散文文档）→ 由 req.InitMode 决定档位，不拒。
//
// 效果：模型驱动的 apidoc_import 技能 InitMode 恒 false（apiDocSkillRequest），于是
// 「聊天拒绝 init」由本闸自动成立，不再依赖技能层那第二道闸（§13.18 的意图由此兑现）。
// 校验只判「声明 vs 勾选」的冲突，档位合法性与 §13.12 的 init 空项目闸门各自独立不动。
// 返回非空字符串 = 拒绝理由（中文短句，前端原样展示）。
func apiDocInitAuthority(req ApiDocImportRequest, doc *apidoc.Document) string {
	declared := strings.TrimSpace(doc.Meta.Scope)
	if declared != apidoc.ModeInit && declared != apidoc.ModeIncremental && declared != apidoc.ModeFull {
		return "" // 文档没声明合法 scope（散文文档 / 非法值）：档位就是请求标志给的档位
	}
	if declared == apidoc.ModeInit && !req.InitMode {
		return "解析档位冲突：文档声明 scope: init，但界面未勾选初始化/基线导入模式，已拒绝导入。" +
			"档位以请求标志为唯一权威（spec-draft §13.26，文档自声明不再能绕过严格校验）——" +
			"要按基线导入请勾选界面向导的「初始化模式」或聊天附件条的「基线导入」（仍受「init 只允许空项目」闸门约束）；" +
			"若这份文档本应是增量变更，请把 meta 的 scope 改为 incremental 或删掉该行"
	}
	if req.InitMode && declared != apidoc.ModeInit {
		return "解析档位冲突：界面已勾选初始化/基线导入模式，但文档声明 scope: " + declared + "，已拒绝导入。" +
			"档位以请求标志为唯一权威（spec-draft §13.26），两者不一致时不做静默取舍——" +
			"请取消勾选以按文档声明导入，或让作者把 meta 的 scope 改为 init（无 meta 的存量散文文档才只由勾选决定档位）"
	}
	return ""
}

// apiDocProjectName 项目名：请求覆盖 > meta.project（D13）。
func apiDocProjectName(req ApiDocImportRequest, doc *apidoc.Document) string {
	return apiDocFirstNonEmpty(strings.TrimSpace(req.ProjectName), strings.TrimSpace(doc.Meta.Project))
}

func apiDocFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// apiDocFingerprint 文档 sha256（批次表 doc_fingerprint，幂等第二道保险）。
// AutoFix 通道下入参已是修复稿 → 指纹/事件 doc_text/落库内容三者同源（AC7：
// 测试比对 sha256(修复稿) == api_import_batches.doc_fingerprint == 事件 meta.docSha256，
// 证明入库的就是文件原文经确定性修复后的文本本身，不存在第三份"AI 改写版"）。
func apiDocFingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// apiDocEventMeta 事件 meta JSON 扩展槽（api_change_events.meta，§13 不给老表加列）：
// docSha256 恒有；AutoFix 通道实际应用过修复时追加 autoFix/fixes/fixCount，
// 让修复清单在 commit 之后仍可从事件详情回读（init 导入不生成事件，修复明细
// 只随预览/结果卡呈现——批次行的 doc_fingerprint 仍对应修复稿）。
func apiDocEventMeta(fingerprint string, fixes []ApiDocFix) map[string]any {
	meta := map[string]any{"docSha256": fingerprint}
	if len(fixes) > 0 {
		meta["autoFix"] = true
		meta["fixes"] = fixes
		meta["fixCount"] = len(fixes)
	}
	return meta
}

func apiDocDeclaredKeys(doc *apidoc.Document) []string {
	keys := make([]string, 0, len(doc.Endpoints))
	for _, ep := range doc.Endpoints {
		keys = append(keys, ep.IdentityKey)
	}
	return keys
}

// apiDocIssueError 把结构化 Issue 列表压成一条中文错误（带前若干条 code+行号+want）。
func apiDocIssueError(prefix string, issues []apidoc.Issue) error {
	const maxShow = 5
	parts := make([]string, 0, maxShow)
	for i, is := range issues {
		if i >= maxShow {
			break
		}
		part := fmt.Sprintf("%s(第%d行)", is.Code, is.Line)
		if is.Want != "" {
			part += "：期望 " + is.Want
		}
		parts = append(parts, part)
	}
	msg := fmt.Sprintf("%s，共 %d 个问题：%s", prefix, len(issues), strings.Join(parts, "；"))
	if len(issues) > maxShow {
		msg += "；…"
	}
	return errors.New(msg)
}

// ===================== 浏览与检索（R9）=====================

// ListApiProjects 全部接口项目（按名称排序）。
func (s *Service) ListApiProjects() ([]store.ApiProject, error) {
	list, err := s.store.ListApiProjects()
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []store.ApiProject{}
	}
	return list, nil
}

// ListApiDomains 项目下全部领域（按 sort）。
func (s *Service) ListApiDomains(projectID string) ([]store.ApiDomain, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("请先选择接口项目")
	}
	list, err := s.store.ListApiDomains(projectID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []store.ApiDomain{}
	}
	return list, nil
}

// SaveApiDomain 手工保存领域（PRD D19：领域支持改名与排序，不支持手工新建、
// 不支持改归属项目、不支持改 internal/notesRaw——两者由文档章节决定）。
//
// 与 SaveApiEntry 同一类防护（store.UpdateApiDomain 也是全列写，漏传即清零）：
// 先按 id 取库里既有行，只覆盖白名单列，其余一律沿用既有值、不接受入参。
//
//	白名单列：name（传空 = 不改名）/ sort（<= 0 = 不改排序）
//	服务端保留列：id / projectId / internal / notesRaw / createdAt / updatedAt
//
// 空值一律按「漏传即不改」处理：领域重排的调用只带 id + sort，改名只带 id + name，
// 服务端不允许把「没传」解释成「清空」。想把领域改名成空串是无效操作（保留原名）。
//
// 领域的身份是「项目内的名称」，所以改名会让下次导入按文档章节标题另建新行
// （D19：不提供改名之外的身份操作）。
func (s *Service) SaveApiDomain(d store.ApiDomain) error {
	id := strings.TrimSpace(d.ID)
	if id == "" {
		return fmt.Errorf("接口领域 id 不能为空（领域由文档章节产生，不支持手工新建）")
	}
	name := strings.TrimSpace(d.Name)
	old, err := s.store.GetApiDomain(id)
	if err != nil {
		return err
	}
	if projectID := strings.TrimSpace(d.ProjectID); projectID != "" && projectID != old.ProjectID {
		return fmt.Errorf("不支持把领域移动到其他项目（领域的身份是项目内的名称）")
	}
	// 读-改-写：internal / notesRaw 由文档章节决定，手工编辑一律沿用库里既有值
	row := old
	if name != "" {
		row.Name = name
	}
	if d.Sort > 0 {
		row.Sort = d.Sort
	}
	if err := s.store.UpdateApiDomain(row); err != nil {
		return err // 撞 UNIQUE(project_id, name) 时 store 已先查再给中文错误（不裸捕约束冲突）
	}
	slog.Info("接口领域已手工保存", "domain", row.ID, "project", row.ProjectID,
		"name", row.Name, "sort", row.Sort)
	return nil
}

// DeleteApiDomain 删除领域（PRD D19：只允许删空领域）。
//
// 领域下还有 active 条目时拒收，中文错误带上条数并给出出路（先删接口，或在导入预览里
// 把本领域合并到其他领域）：静默删掉一个还有接口的领域会让接口从层级里消失，
// 而文档下次导入又会把领域行按章节标题建回来。
//
// 为什么物理删而不是软删：api_domains 没有 status 列（表结构照 PRD「不留状态机」），
// 且历史事件读的是 api_change_items.domain_name 冗余快照（R7），删行不会让既有事件
// 读不出领域名；留个空行反而会让「已删领域」继续出现在列表里。
// 领域里残余的软删条目会被保留（事件要能按 entry_id 跳转，硬删会让引用悬空），
// 只把它们的 domain_id 置空 = 未归领域。
func (s *Service) DeleteApiDomain(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("接口领域 id 不能为空")
	}
	d, err := s.store.GetApiDomain(id)
	if err != nil {
		return err
	}
	active, total, err := s.store.CountApiEntriesInDomain(id)
	if err != nil {
		return fmt.Errorf("统计领域下的接口条目失败: %w", err)
	}
	if active > 0 {
		return fmt.Errorf("该领域下还有 %d 个接口，无法删除；请先删除这些接口，或在导入预览里把本领域合并到其他领域", active)
	}
	if err := s.store.DeleteApiDomainRow(id); err != nil {
		return err
	}
	slog.Info("接口领域已删除", "domain", id, "project", d.ProjectID, "name", d.Name,
		"detachedEntries", total)
	return nil
}

// ListApiEntries 接口条目列表：默认过滤 internal 与 removed；keyword 对名称/路径
// 做大小写不敏感的模糊匹配。
func (s *Service) ListApiEntries(q ApiEntryQuery) ([]store.ApiEntry, error) {
	if strings.TrimSpace(q.ProjectID) == "" {
		return nil, fmt.Errorf("请先选择接口项目")
	}
	list, err := s.store.ListApiEntries(q.ProjectID, q.DomainID, q.IncludeRemoved, q.IncludeInternal)
	if err != nil {
		return nil, err
	}
	out := make([]store.ApiEntry, 0, len(list))
	kw := strings.ToLower(strings.TrimSpace(q.Keyword))
	for _, e := range list {
		if kw == "" || strings.Contains(strings.ToLower(e.Name), kw) ||
			strings.Contains(strings.ToLower(e.PathRaw), kw) {
			out = append(out, e)
		}
	}
	return out, nil
}

// GetApiEntry 接口条目详情：条目 + 分组字段 + 命名结构 + 示例与 curl + 历史事件（反向关联）。
func (s *Service) GetApiEntry(id string) (ApiEntryDetail, error) {
	detail := ApiEntryDetail{
		PathFields:     []store.ApiField{},
		RequestFields:  []store.ApiField{},
		ResponseFields: []store.ApiField{},
		Schemas:        []ApiSchemaDetail{},
		Examples:       []store.ApiExample{},
		Events:         []store.ApiEntryEventRef{},
	}
	if strings.TrimSpace(id) == "" {
		return detail, fmt.Errorf("接口条目 id 不能为空")
	}
	entry, err := s.store.GetApiEntry(id)
	if err != nil {
		return detail, err
	}
	detail.Entry = entry
	fields, err := s.store.ListApiFields(store.ApiOwnerEndpoint, id)
	if err != nil {
		return detail, err
	}
	for _, f := range fields {
		switch f.Kind {
		case store.ApiFieldKindPath:
			detail.PathFields = append(detail.PathFields, f)
		case store.ApiFieldKindResponse:
			detail.ResponseFields = append(detail.ResponseFields, f)
		default: // request | body | header
			detail.RequestFields = append(detail.RequestFields, f)
		}
	}
	schemas, err := s.store.ListApiSchemasByOwner(entry.ProjectID, id)
	if err != nil {
		return detail, err
	}
	for _, sc := range schemas {
		sf, err := s.store.ListApiFields(store.ApiOwnerSchema, sc.ID)
		if err != nil {
			return detail, err
		}
		if sf == nil {
			sf = []store.ApiField{}
		}
		detail.Schemas = append(detail.Schemas, ApiSchemaDetail{Schema: sc, Fields: sf})
	}
	examples, err := s.store.ListApiExamples(store.ApiOwnerEndpoint, id)
	if err != nil {
		return detail, err
	}
	if examples != nil {
		detail.Examples = examples
	}
	events, err := s.store.ListApiEntryEvents(id)
	if err != nil {
		return detail, err
	}
	if events != nil {
		detail.Events = events
	}
	return detail, nil
}

// SaveApiEntry 手工保存接口条目（编辑已有条目，或手工新建；PRD D19）。
//
// ⚠️ 手工编辑不产生变更事件（PRD 已裁决：事件流完整性属路线 C，本期不做；
// 界面必须明示「手工修改不会记入变更事件」）。
//
// 为什么要在这里做读-改-写：store.SaveApiEntry 是全列 upsert（21 列一次写满，漏传即清零）。
// 让调用方自己补齐全列（PR4a 前端的做法：深拷贝整个 entry 再改白名单字段）只是权宜之计——
// 少带一个字段就会静默抹掉 raw_markdown、抹掉 semantic_hash/doc_hash（违反 spec-draft §13.19
// 「手工编辑不重算双 hash」），而 store 的空值兜底还会把 parse_state 从 partial 悄悄改成 ok
// （等于把「这个接口有未解析项」这个事实抹掉）。所以补齐责任收在服务端：
//
//	白名单列（唯一允许入参覆盖的列）：
//	  name / description / contentType / auth / visibility / sort / domainId（限同项目内移动）
//	服务端保留列（一律沿用库里既有值，不接受入参）：
//	  id / projectId / identityKey / method / pathRaw / basePathAssert / semanticHash /
//	  docHash / rawMarkdown / parseState / status / deletedAt / createdAt
//
// status 与 deletedAt 只能通过 DeleteApiEntry / RestoreApiEntry 改——软删/恢复是独立语义，
// 混进手工编辑就等于让「删除」变成一个可能被漏传的字段。
//
// 白名单里的空值语义（尽量不把「漏传」变成「清空」）：
//   - visibility 传空串 = 不改；domainId 传空串 = 不挪归属；sort <= 0 = 不改排序；
//   - method / pathRaw 双双传空 = 不改（传了就必须与库里身份键一致，否则中文错误拒收）；
//   - description / contentType / auth 是纯文本列，入参为准（传空即清空）——表单必须回填当前值再提交。
//
// 新建路径（入参 id 为空）没有既有行可沿用：projectId / method / pathRaw 必填，
// 服务端保留列由服务端定值（status=active、deletedAt=0、parse_state 走 store 兜底 ok，
// 双 hash 与 raw_markdown 一律留空——手工创建的条目没有导入指纹，等下次文档导入来写）。
func (s *Service) SaveApiEntry(e store.ApiEntry) error {
	name := strings.TrimSpace(e.Name)
	if name == "" {
		return fmt.Errorf("接口名称不能为空")
	}
	switch e.Visibility {
	case "", store.ApiVisibilityPublic, store.ApiVisibilityInternal:
	default:
		return fmt.Errorf("可见性不合法: %s（应为 public/internal）", e.Visibility)
	}
	if strings.TrimSpace(e.ID) == "" {
		return s.createApiEntryManually(e, name)
	}
	old, err := s.store.GetApiEntry(e.ID)
	if err != nil {
		return err
	}
	if projectID := strings.TrimSpace(e.ProjectID); projectID != "" && projectID != old.ProjectID {
		return fmt.Errorf("不支持把接口条目移动到其他项目")
	}
	method := strings.ToUpper(strings.TrimSpace(e.Method))
	path := strings.TrimSpace(e.PathRaw)
	if method != "" || path != "" { // 传了才校验（表单不显示方法/路径时留空即不改）
		if !apidoc.ValidMethod(method) {
			return fmt.Errorf("HTTP 方法不合法: %s（应为 GET/POST/PUT/DELETE/PATCH 大写）", e.Method)
		}
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("接口路径必须以 / 开头")
		}
		if apidoc.IdentityKey(method, path) != old.IdentityKey {
			return fmt.Errorf("手工编辑不支持修改方法或路径（会改变接口身份），请走文档导入流程")
		}
	}
	domainID := strings.TrimSpace(e.DomainID)
	if domainID != "" && domainID != old.DomainID {
		if err := s.checkApiDomainInProject(domainID, old.ProjectID); err != nil {
			return err
		}
	}
	// 读-改-写：从库里既有行出发，白名单之外的列一个都不碰。
	row := old
	row.Name = name
	row.Description = e.Description
	row.ContentType = e.ContentType
	row.Auth = e.Auth
	if e.Visibility != "" {
		row.Visibility = e.Visibility
	}
	if domainID != "" {
		row.DomainID = domainID
	}
	if e.Sort > 0 {
		row.Sort = e.Sort
	}
	if err := s.store.SaveApiEntry(&row); err != nil {
		return err
	}
	slog.Info("接口条目已手工保存（不产生变更事件）", "entry", row.ID, "key", row.IdentityKey,
		"project", row.ProjectID, "domainChanged", domainID != "" && domainID != old.DomainID)
	return nil
}

// createApiEntryManually 手工新建条目（id 为空路径）。只收白名单列 + 身份三列，
// 服务端保留列不接受入参（见 SaveApiEntry 注释）。
func (s *Service) createApiEntryManually(e store.ApiEntry, name string) error {
	projectID := strings.TrimSpace(e.ProjectID)
	if projectID == "" {
		return fmt.Errorf("接口条目缺少所属项目")
	}
	method := strings.ToUpper(strings.TrimSpace(e.Method))
	if !apidoc.ValidMethod(method) {
		return fmt.Errorf("HTTP 方法不合法: %s（应为 GET/POST/PUT/DELETE/PATCH 大写）", e.Method)
	}
	path := strings.TrimSpace(e.PathRaw)
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("接口路径必须以 / 开头")
	}
	domainID := strings.TrimSpace(e.DomainID)
	if domainID != "" {
		if err := s.checkApiDomainInProject(domainID, projectID); err != nil {
			return err
		}
	}
	key := apidoc.IdentityKey(method, path)
	if _, found, err := s.store.FindApiEntryByIdentity(projectID, key); err != nil {
		return err
	} else if found {
		return fmt.Errorf("接口已存在: %s %s", method, path)
	}
	visibility := e.Visibility
	if visibility == "" {
		visibility = store.ApiVisibilityPublic
	}
	row := store.ApiEntry{
		ProjectID: projectID, DomainID: domainID, Name: name,
		Method: method, PathRaw: path, IdentityKey: key,
		ContentType: e.ContentType, Auth: e.Auth, Visibility: visibility,
		Description: e.Description, Sort: e.Sort,
		// Status/DeletedAt/ParseState/SemanticHash/DocHash/RawMarkdown/BasePathAssert 全部留空
		// 由服务端与 store 兜底定值：手工条目没有导入指纹，parse_state 也只能是 ok（全手工写的定义）。
	}
	if err := s.store.SaveApiEntry(&row); err != nil {
		return err
	}
	slog.Info("接口条目已手工新建（不产生变更事件）", "entry", row.ID, "key", row.IdentityKey, "project", projectID)
	return nil
}

// checkApiDomainInProject 领域行必须存在且属于该项目（条目在同项目内移动归属用）。
func (s *Service) checkApiDomainInProject(domainID, projectID string) error {
	d, err := s.store.GetApiDomain(domainID)
	if err != nil {
		return err
	}
	if d.ProjectID != projectID {
		return fmt.Errorf("接口领域「%s」不属于该项目，不能把条目移过去", d.Name)
	}
	return nil
}

// DeleteApiEntry 删除接口条目（软删 status=removed；事件要能长期引用「当时那个接口」，
// 硬删会让引用悬空，R7）。
func (s *Service) DeleteApiEntry(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("接口条目 id 不能为空")
	}
	if _, err := s.store.GetApiEntry(id); err != nil {
		return err
	}
	if err := s.store.MarkApiEntryRemoved(id); err != nil {
		return err
	}
	slog.Info("接口条目已删除（软删）", "entry", id)
	return nil
}

// RestoreApiEntry 恢复软删的接口条目（幂等：已是 active 直接成功）。
func (s *Service) RestoreApiEntry(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("接口条目 id 不能为空")
	}
	e, err := s.store.GetApiEntry(id)
	if err != nil {
		return err
	}
	if e.Status == store.ApiEntryStatusActive {
		return nil
	}
	e.Status = store.ApiEntryStatusActive
	e.DeletedAt = 0
	if err := s.store.SaveApiEntry(&e); err != nil {
		return err
	}
	slog.Info("接口条目已恢复", "entry", id)
	return nil
}

// DeleteApiProject 删除项目并手写级联清理 9 张 api_* 表（无 FK，store 单事务多条 DELETE）。
func (s *Service) DeleteApiProject(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("接口项目 id 不能为空")
	}
	if _, err := s.store.GetApiProject(id); err != nil {
		return err
	}
	if err := s.store.DeleteApiProject(id); err != nil {
		return err
	}
	slog.Info("接口项目已级联删除", "project", id)
	return nil
}

// ===================== 变更事件（R6/AC7）=====================

// ListApiChangeEvents 事件列表：seq DESC；可按 label 过滤（「按需求标签聚合」视图，D10）。
// 返回结构里 doc_text 恒为空（列表 SQL 明确列名，不带大文本；原文单独取）。
func (s *Service) ListApiChangeEvents(q ApiEventQuery) ([]store.ApiChangeEvent, error) {
	if strings.TrimSpace(q.ProjectID) == "" {
		return nil, fmt.Errorf("请先选择接口项目")
	}
	list, err := s.store.ListApiChangeEvents(q.ProjectID, 0)
	if err != nil {
		return nil, err
	}
	out := make([]store.ApiChangeEvent, 0, len(list))
	label := strings.TrimSpace(q.Label)
	for _, e := range list {
		if label != "" && e.Label != label {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// ListApiChangeLabels 项目下出现过的事件 label（去重，按最近事件的 seq 序）：
// 供「按需求标签聚合」视图的筛选器使用。
func (s *Service) ListApiChangeLabels(projectID string) ([]string, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("请先选择接口项目")
	}
	list, err := s.store.ListApiChangeEvents(projectID, 0)
	if err != nil {
		return nil, err
	}
	out := []string{}
	seen := map[string]bool{}
	for _, e := range list {
		if e.Label == "" || seen[e.Label] {
			continue
		}
		seen[e.Label] = true
		out = append(out, e.Label)
	}
	return out, nil
}

// GetApiChangeEvent 事件详情 = 事件头 + 明细（含字段级 diff 载荷）；不含 doc_text。
func (s *Service) GetApiChangeEvent(id int64) (ApiChangeEventDetail, error) {
	detail := ApiChangeEventDetail{Items: []store.ApiChangeItem{}}
	if id <= 0 {
		return detail, fmt.Errorf("变更事件 id 不合法")
	}
	ev, err := s.store.GetApiChangeEvent(id)
	if err != nil {
		return detail, err
	}
	items, err := s.store.ListApiChangeItems(id)
	if err != nil {
		return detail, err
	}
	for i := range items {
		if items[i].Diff == nil {
			items[i].Diff = []apidoc.DiffItem{} // JS 侧拿 [] 而不是 null
		}
	}
	detail.Event = ev
	detail.Items = items
	return detail, nil
}

// GetApiChangeEventDocText 单独取事件保存的导入文档全文（D12 输入凭证，展开核对用）。
func (s *Service) GetApiChangeEventDocText(id int64) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("变更事件 id 不合法")
	}
	return s.store.GetApiChangeEventDocText(id)
}
