// query.go 在「交付三件套」（apidoc_spec / apidoc_check / apidoc_import）之外
// 给同包补上第四个技能 apidoc_query——只读的接口文档库查询（任务 10-21）。
//
// 为什么必须有它（PRD 现象段）：此前智能体对 api_* 表完全读不到——三个老技能只做
// 「规范交付 / 格式体检 / 导入」，没有任何读库工具。用户要在聊天里讨论接口、
// 分析影响面，必须先能「看见」库里的东西。
//
// 设计要点（逐条对应 PRD 决策，改动前请先读 prd.md，勿凭喜好改）：
//  1. D3 单工具 + action 六枚举（projects / domains / entries / entry / events / event），
//     不拆成 6 个工具：工具越少模型选错概率越低，常驻 schema 体积越小；
//  2. D2 分层预算（本文件的预算规格，逐字照抄）：摘要层（entries）绝不带字段表，
//     详情层（entry）才给；超限必须显式 truncated=true + total + 缩小指引（R2——
//     模型把「看到的」当「全部」，比不查更糟）；
//  3. D4 只读 + 出网前脱敏：QuerySource 缝上没有任何写方法（结构上就写不了，AC6）；
//     所有出口文本统一走 render()（MaskSecrets + CapChars 12000）——接口示例天然带
//     Bearer token 与内网 host，没有豁免通道；
//  4. R3 raw_markdown 默认不返回：EntryView / EventView 在类型层面就没有 RawMarkdown /
//     DocText 字段——「不返回原文」是类型闸门而不是渲染纪律，实现想手滑都做不到；
//  5. D5 复用 appsvc 的六个既有查询方法（界面向导在用）：本层只做投影与聚合
//     （接口数 / 事件数 / 每领域条数用现有 List 的结果在技能层数出来），零新 SQL、
//     零新业务逻辑。本包不 import appsvc（循环依赖），缝由 appsvc/apidocquery.go
//     实现、app.go 注入——形状照 deploy 的 FlowSource（接口 + svc 实现）。
//
// R6：Description 写清「何时用哪个 action」，并要求先查 projects 而不是猜项目名。
package apidoc

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	docspec "DevCraft/internal/apidoc" // DiffItem 词表（复用既有导入，不新增依赖面）
	"DevCraft/internal/sanitize"
	"DevCraft/internal/skill"
	"DevCraft/internal/store"
)

// SkillQuery 常量在 apidoc.go 的技能名块中声明（与交付三件套并列，装配迁移走独立标记键）。

// 分层预算（PRD D2 的表就是规格；改数字 = 改契约）。
const (
	queryDefaultEntriesLimit = 50   // entries 默认条数（D2）
	queryMaxEntriesLimit     = 200  // entries 硬上限（防御模型乱传大 limit，照 pg.MaxRowLimit 范式）
	queryEventsCap           = 20   // events 返回上限（D2）
	queryEventItemsCap       = 40   // event 明细上限（D2：超出截断并说明）
	queryItemDiffCap         = 10   // 单条明细的字段级 diff 行数上限（兜底，超出显式「从略」）
	queryEntryEventsCap      = 10   // entry 详情里「历史事件摘要」行数上限
	queryExampleCap          = 1000 // 单个示例/curl 的字符上限（防一条 curl 挤爆整份详情）
	queryDiffValueCap        = 200  // diff 的 before/after 序列化后的字符上限
)

// ===================== 只读注入缝（appsvc 实现，app.go 注入）=====================

// QuerySource 查询技能可见的编排层只读能力（D5）：与 appsvc 既有的六个查询方法
// 一一对应（ListApiProjects / ListApiDomains / ListApiEntries / GetApiEntry /
// ListApiChangeEvents / GetApiChangeEvent），由 appsvc/apidocquery.go 做薄投影实现。
// ⚠️ 本接口没有任何 Save/Commit/Delete 方法——「只读」由接口形状在编译期保证（AC6），
// 不依赖实现或模型的自觉。
type QuerySource interface {
	ApiDocSkillProjects(ctx context.Context) ([]store.ApiProject, error)
	ApiDocSkillDomains(ctx context.Context, projectID string) ([]store.ApiDomain, error)
	ApiDocSkillEntries(ctx context.Context, projectID, domainID, keyword string, includeInternal, includeRemoved bool) ([]store.ApiEntry, error)
	ApiDocSkillEntry(ctx context.Context, entryID string) (EntryView, error)
	ApiDocSkillEvents(ctx context.Context, projectID string, limit int) ([]store.ApiChangeEvent, error)
	ApiDocSkillEvent(ctx context.Context, eventID int64) (EventView, error)
}

// EntryView 单接口详情的技能侧投影（R3：类型里没有 RawMarkdown——原文永不进模型；
// 双 hash / identityKey 等机器列同样不投影，模型不需要也不该被它们占上下文）。
type EntryView struct {
	ID             string
	ProjectID      string
	DomainID       string
	Name           string
	Method         string
	Path           string
	Description    string
	Auth           string
	ContentType    string
	Visibility     string
	Status         string
	ParseState     string
	BasePathAssert string
	PathFields     []store.ApiField
	RequestFields  []store.ApiField
	ResponseFields []store.ApiField
	Schemas        []EntrySchemaView
	Examples       []store.ApiExample
	Events         []store.ApiEntryEventRef
}

// EntrySchemaView 命名结构投影（歧义标记必带——§13.31：ambiguous 时按名展开不可信）。
type EntrySchemaView struct {
	Name      string
	Ambiguous bool
	Fields    []store.ApiField
}

// EventView 变更事件详情的技能侧投影（R3/D4：没有 DocText 字段——导入原文
// 永远只走界面事件详情页的 GetApiChangeEventDocText，不进模型）。
type EventView struct {
	ID        int64
	ProjectID string
	Seq       int
	Label     string
	Title     string
	Reason    string
	Kind      string
	Breaking  int
	Source    string
	DocRef    string
	ScopeMode string
	Stats     store.ApiEventStats
	CreatedAt int64
	Items     []EventItemView
}

// EventItemView 事件明细行（逐字段 diff；before/after 是对象——spec-draft §13.20）。
type EventItemView struct {
	EntryID      string
	DomainName   string
	EntryName    string
	Method       string
	Path         string
	ChangeType   string
	Breaking     int
	DeclaredNote string
	Diff         []docspec.DiffItem
}

// ===================== apidoc_query =====================

// queryDoc 只读的接口文档库查询技能（分层摘要，防爆上下文）。
type queryDoc struct {
	base
	q QuerySource
}

func (s *queryDoc) Name() string { return SkillQuery }

// Description 是路由说明书（R6）：模型第一轮就该先 projects，而不是猜项目名。
func (s *queryDoc) Description() string {
	return "只读查询 DevCraft 接口文档库（api_* 表），不写入、不导入任何东西。" +
		"按「逐层缩小」选 action：" +
		"① 先弄清库里有哪些接口项目、各自多少接口/事件 → action=projects（无需其他参数）；" +
		"② 某项目有哪些领域、每领域接口数 → action=domains + project；" +
		"③ 接口清单摘要行（METHOD path｜名称｜领域｜parseState，不含字段表；默认 50 条，超出会明确告知 truncated 与 total）→ action=entries + project，可选 domain/keyword/includeInternal/includeRemoved/limit；" +
		"④ 单个接口完整详情（分组参数、返回字段、命名结构、示例与 curl、该接口历史事件；接口原文 raw_markdown 不返回）→ action=entry + id（id 取自 entries 摘要行的 [id=…]）；" +
		"⑤ 某项目最近变更事件（最多 20 条）→ action=events + project；" +
		"⑥ 单个事件的「为什么改」与字段级 diff（明细最多 40 项，超出截断并说明）→ action=event + id（id 取自 events 行的 [id=…]，数字）。" +
		"除 projects 外都必须带 project；不确定项目名时先 action=projects 列举，禁止猜测。" +
		"返回文本已做凭据掩码与字符上限；一旦出现「truncated」或「已截断」提示，按提示用 domain/keyword 缩小后再查，不要把看到的部分当成全部来回答。"
}

func (s *queryDoc) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["projects","domains","entries","entry","events","event"],"description":"查询动作（层级导航 projects→domains→entries→entry；变更史 events→event），用法见工具描述"},
			"project":{"type":"string","description":"项目标识（除 projects 外必带；不确定先用 action=projects 列举）"},
			"domain":{"type":"string","description":"仅 action=entries：按领域名过滤"},
			"keyword":{"type":"string","description":"仅 action=entries：接口名称或路径模糊匹配（大小写不敏感）"},
			"id":{"type":"string","description":"仅 action=entry（entries 行 [id=…] 里的接口 id）或 action=event（events 行 [id=…] 里的数字事件 id）"},
			"includeInternal":{"type":"boolean","description":"仅 action=entries：包含内部接口（默认 false）"},
			"includeRemoved":{"type":"boolean","description":"仅 action=entries：包含已删除接口（默认 false）"},
			"limit":{"type":"integer","description":"仅 action=entries（默认 50，上限 200）与 action=events（上限 20）"}
		},
		"required":["action"],
		"additionalProperties":false
	}`)
}

// queryArgs 参数白名单（decodeArgs 的真闸门，schema 只是提示）。
var queryArgs = []string{"action", "project", "domain", "keyword", "id", "includeInternal", "includeRemoved", "limit"}

// queryIn 技能入参。id 收 RawMessage 再手工解析：事件 id 是数字、条目 id 是字符串，
// 模型两种形态都可能传，在这里宽容、在白名单外一律拒绝。
type queryIn struct {
	Action          string          `json:"action"`
	Project         string          `json:"project"`
	Domain          string          `json:"domain"`
	Keyword         string          `json:"keyword"`
	ID              json.RawMessage `json:"id"`
	IncludeInternal bool            `json:"includeInternal"`
	IncludeRemoved  bool            `json:"includeRemoved"`
	Limit           int             `json:"limit"`
}

// idText 把 id 参数解析成字符串（容忍 number 形态）。
func (in queryIn) idText() string {
	raw := in.ID
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil && f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

// Execute 只做「解 action → 调只读缝 → 渲染文本」。
// 全部输出经 render()（掩码 + 12000 字符封顶，D4/R4）；本技能不存在任何写入路径。
func (s *queryDoc) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in queryIn
	if err := decodeArgs(args, queryArgs, &in); err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "projects":
		return s.qProjects(ctx)
	case "domains":
		return s.qDomains(ctx, in.Project)
	case "entries":
		return s.qEntries(ctx, in)
	case "entry":
		return s.qEntry(ctx, in.idText())
	case "events":
		return s.qEvents(ctx, in.Project, in.Limit)
	case "event":
		return s.qEvent(ctx, in.idText())
	default:
		return "", fmt.Errorf("参数错误: 未知 action %q（可用: projects / domains / entries / entry / events / event；不确定库里有什么项目时先调 action=projects）", in.Action)
	}
}

// ===================== action: projects =====================

// qProjects 项目目录（D2：项目名 + 显示名 + 接口数 + 事件数，先让模型知道规模）。
// 计数用现有 List 在技能层聚合（D5，不新增 appsvc 统计方法）：
// 接口数 = 未删除接口数（含 internal，规模口径）；事件数 = 全部变更事件数。
func (s *queryDoc) qProjects(ctx context.Context) (string, error) {
	projects, err := s.q.ApiDocSkillProjects(ctx)
	if err != nil {
		return "", fmt.Errorf("查询接口项目失败: %w", err)
	}
	if len(projects) == 0 {
		return render(
			[]string{"接口文档库还没有任何项目（从未导入过接口文档）。"},
			[]string{"如需建库：先用 apidoc_spec 了解《接口变更文档规范》，再用 apidoc_import 或界面导入向导灌入文档。"},
		), nil
	}
	lines := make([]string, 0, len(projects))
	for _, p := range projects {
		entries, err := s.q.ApiDocSkillEntries(ctx, p.ID, "", "", true, false)
		if err != nil {
			return "", fmt.Errorf("统计项目 %s 的接口数失败: %w", p.Name, err)
		}
		events, err := s.q.ApiDocSkillEvents(ctx, p.ID, 0)
		if err != nil {
			return "", fmt.Errorf("统计项目 %s 的事件数失败: %w", p.Name, err)
		}
		name := p.Name
		if p.Display != "" && p.Display != p.Name {
			name = fmt.Sprintf("%s（显示名 %s）", p.Name, p.Display)
		}
		lines = append(lines, fmt.Sprintf("- %s ｜ 接口 %d ｜ 事件 %d", name, len(entries), len(events)))
	}
	return render(
		[]string{fmt.Sprintf("接口文档库共 %d 个项目（接口数=未删除接口，含 internal）：", len(projects))},
		lines,
		[]string{"下一步：action=domains 看领域、action=entries 看接口清单（project=上面的项目标识）。"},
	), nil
}

// ===================== 项目名 → 项目行（D1：目录逐层导航，不做会话绑定）=====================

// needProject 把模型给的 project 标识解析成项目行；缺失与不存在都给中文短句并指路
// action=projects（R6：模型不该猜项目名）。除 projects 外的 action 都经这里。
func (s *queryDoc) needProject(ctx context.Context, name string) (store.ApiProject, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.ApiProject{}, fmt.Errorf("参数错误: 本 action 需要 project（项目标识）；不确定库里有哪些项目时先调 action=projects（无需参数）")
	}
	projects, err := s.q.ApiDocSkillProjects(ctx)
	if err != nil {
		return store.ApiProject{}, fmt.Errorf("查询接口项目失败: %w", err)
	}
	avail := make([]string, 0, len(projects))
	for _, p := range projects {
		if p.Name == name || strings.EqualFold(p.Name, name) || (p.Display != "" && p.Display == name) {
			return p, nil
		}
		avail = append(avail, p.Name)
	}
	return store.ApiProject{}, fmt.Errorf("接口项目 %q 不存在（可用: %s）；不要猜测项目名，可先调 action=projects 列举",
		name, strings.Join(avail, ", "))
}

// ===================== action: domains =====================

// qDomains 领域清单 + 每领域接口数（D2 预算：全部领域；计数用现有 List 聚合）。
func (s *queryDoc) qDomains(ctx context.Context, projectName string) (string, error) {
	p, err := s.needProject(ctx, projectName)
	if err != nil {
		return "", err
	}
	domains, err := s.q.ApiDocSkillDomains(ctx, p.ID)
	if err != nil {
		return "", fmt.Errorf("查询领域失败: %w", err)
	}
	entries, err := s.q.ApiDocSkillEntries(ctx, p.ID, "", "", true, false)
	if err != nil {
		return "", fmt.Errorf("统计领域接口数失败: %w", err)
	}
	count := make(map[string]int, len(domains))
	unassigned := 0
	for _, e := range entries {
		if e.DomainID == "" {
			unassigned++
			continue
		}
		count[e.DomainID]++
	}
	if len(domains) == 0 {
		return render(
			[]string{fmt.Sprintf("项目 %s 还没有领域（active 接口 %d 个）。", p.Name, len(entries))},
			[]string{"领域由文档一级 ## 章节在导入时产生；散落在领域之外的接口会显示为「未归领域」。"},
		), nil
	}
	lines := make([]string, 0, len(domains)+1)
	for _, d := range domains {
		line := fmt.Sprintf("- %s ｜ 接口 %d", d.Name, count[d.ID])
		if d.Internal {
			line += "（内部领域，默认不出现在 entries 结果里，需 includeInternal=true）"
		}
		lines = append(lines, line)
	}
	if unassigned > 0 {
		lines = append(lines, fmt.Sprintf("- 未归领域 ｜ 接口 %d", unassigned))
	}
	return render(
		[]string{fmt.Sprintf("项目 %s 共 %d 个领域（接口数=未删除，含 internal）：", p.Name, len(domains))},
		lines,
		[]string{fmt.Sprintf("下一步：action=entries + project=%s，可加 domain=领域名 缩小范围。", p.Name)},
	), nil
}

// ===================== action: entries =====================

// qEntries 接口摘要清单（D2/R2 的硬规则：摘要行绝不带字段表；超出必须说清
// truncated + total + 怎么缩小，防止模型把「看到的 50 条」当成「全部」）。
func (s *queryDoc) qEntries(ctx context.Context, in queryIn) (string, error) {
	p, err := s.needProject(ctx, in.Project)
	if err != nil {
		return "", err
	}
	domains, err := s.q.ApiDocSkillDomains(ctx, p.ID)
	if err != nil {
		return "", fmt.Errorf("查询领域失败: %w", err)
	}
	byName := make(map[string]store.ApiDomain, len(domains))
	nameByID := make(map[string]string, len(domains))
	avail := make([]string, 0, len(domains))
	for _, d := range domains {
		byName[strings.ToLower(d.Name)] = d
		nameByID[d.ID] = d.Name
		avail = append(avail, d.Name)
	}
	domainID := ""
	if dm := strings.TrimSpace(in.Domain); dm != "" {
		d, ok := byName[strings.ToLower(dm)]
		if !ok {
			return "", fmt.Errorf("领域 %q 不存在（项目 %s 可用: %s）；不要猜测领域名，可先调 action=domains",
				dm, p.Name, strings.Join(avail, ", "))
		}
		domainID = d.ID
	}
	limit := in.Limit
	if limit <= 0 {
		limit = queryDefaultEntriesLimit
	}
	if limit > queryMaxEntriesLimit {
		limit = queryMaxEntriesLimit
	}
	entries, err := s.q.ApiDocSkillEntries(ctx, p.ID, domainID, strings.TrimSpace(in.Keyword), in.IncludeInternal, in.IncludeRemoved)
	if err != nil {
		return "", fmt.Errorf("查询接口清单失败: %w", err)
	}
	total := len(entries)
	scope := fmt.Sprintf("筛选: 项目 %s｜领域 %s｜关键词 %q｜含内部=%t｜含已删除=%t",
		p.Name, orDefault(strings.TrimSpace(in.Domain), "全部"), strings.TrimSpace(in.Keyword), in.IncludeInternal, in.IncludeRemoved)
	if total == 0 {
		return render(
			[]string{fmt.Sprintf("接口清单为空（%s）。", scope)},
			[]string{"放宽办法：换 keyword、去掉 domain，或把 includeInternal / includeRemoved 置 true；也可先 action=domains 看领域分布。"},
		), nil
	}
	shown := entries
	if len(shown) > limit {
		shown = shown[:limit]
	}
	lines := make([]string, 0, len(shown))
	for _, e := range shown {
		// D2 摘要行格式逐字：METHOD path ｜ 名称 ｜ 领域 ｜ parseState（[id=…] 是给
		// action=entry 的把手，不是字段表的一部分）
		domain := nameByID[e.DomainID]
		if e.DomainID == "" || domain == "" {
			domain = "未归领域"
		}
		line := fmt.Sprintf("- [id=%s] %s %s ｜ %s ｜ %s ｜ %s",
			e.ID, e.Method, e.PathRaw, orDefault(e.Name, "(未命名)"), domain, entryStateOf(e.ParseState))
		if e.Status == store.ApiEntryStatusRemoved {
			line += " ｜已删除"
		}
		if e.Visibility == store.ApiVisibilityInternal {
			line += " ｜internal"
		}
		lines = append(lines, line)
	}
	head := []string{fmt.Sprintf("命中 %d 个接口（%s）：", total, scope)}
	tail := []string{"摘要行不含字段表；要看某个接口的参数与返回字段，用 action=entry + 上面行内 [id=…]。"}
	if total > len(shown) {
		tail = append([]string{fmt.Sprintf(
			"truncated=true：共 %d 条，本次只返回前 %d 条摘要（还有 %d 条没显示）；请用 domain 或 keyword 缩小范围，或加大 limit（默认 %d，上限 %d）。不要把这里看到的当成全部接口。",
			total, len(shown), total-len(shown), queryDefaultEntriesLimit, queryMaxEntriesLimit)}, tail...)
	}
	return render(head, lines, tail), nil
}

// ===================== action: entry =====================

// qEntry 单接口完整详情（分组参数、返回字段、命名结构、示例与 curl、历史事件摘要）。
// R3：raw_markdown 不返回（EntryView 类型里根本没有该字段）；出网仍统一 render 兜底。
func (s *queryDoc) qEntry(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("参数错误: action=entry 需要 id（接口条目 id，取自 action=entries 摘要行里的 [id=…]）")
	}
	d, err := s.q.ApiDocSkillEntry(ctx, id)
	if err != nil {
		return "", fmt.Errorf("查询接口详情失败: %w", err)
	}
	if d.ID == "" {
		return "", fmt.Errorf("接口条目不存在（id=%s）；先用 action=entries 拿到真实 id", id)
	}
	// 名称装饰（项目/领域）走既有 List 在技能层拼（D5）；失败降级为显示 id，不阻断详情。
	projName := d.ProjectID
	if projects, perr := s.q.ApiDocSkillProjects(ctx); perr == nil {
		for _, p := range projects {
			if p.ID == d.ProjectID {
				projName = p.Name
			}
		}
	}
	domainName := "未归领域"
	if d.DomainID != "" {
		if ds, derr := s.q.ApiDocSkillDomains(ctx, d.ProjectID); derr == nil {
			for _, x := range ds {
				if x.ID == d.DomainID {
					domainName = x.Name
				}
			}
		}
	}
	head := []string{
		fmt.Sprintf("%s %s ｜ %s", d.Method, d.Path, orDefault(d.Name, "(未命名)")),
		fmt.Sprintf("项目 %s ｜ 领域 %s ｜ 可见性 %s ｜ 状态 %s ｜ 解析状态 %s",
			projName, domainName, orDefault(d.Visibility, "public"), statusText(d.Status), entryStateOf(d.ParseState)),
	}
	var meta []string
	if d.Description != "" {
		meta = append(meta, "说明: "+oneLine(d.Description))
	}
	if d.Auth != "" {
		meta = append(meta, "认证: "+oneLine(d.Auth))
	}
	if d.ContentType != "" {
		meta = append(meta, "Content-Type: "+d.ContentType)
	}
	if d.BasePathAssert != "" {
		meta = append(meta, "基础路径断言: "+d.BasePathAssert)
	}
	schemaLines := make([]string, 0, len(d.Schemas))
	for _, sc := range d.Schemas {
		part := fmt.Sprintf("- 结构 %s（%d 个字段）:", sc.Name, len(sc.Fields))
		if sc.Ambiguous {
			part += "（歧义: 库里存在同名不同构的多套定义，按名展开不可信，请对照原文核对）"
		}
		schemaLines = append(schemaLines, part)
		for _, f := range sc.Fields {
			schemaLines = append(schemaLines, "  "+fieldLine(f))
		}
	}
	exampleLines := make([]string, 0, len(d.Examples)*2)
	for _, ex := range d.Examples {
		exampleLines = append(exampleLines, fmt.Sprintf("- 示例[%s %s]:", orDefault(ex.Kind, "example"), orDefault(ex.Language, "text")))
		exampleLines = append(exampleLines, sanitize.CapChars(ex.Content, queryExampleCap))
	}
	eventLines := make([]string, 0, queryEntryEventsCap+1)
	for i, ev := range d.Events {
		if i >= queryEntryEventsCap {
			eventLines = append(eventLines, fmt.Sprintf("- …其余 %d 个事件从略（最新全貌看 action=events，单条明细看 action=event）", len(d.Events)-i))
			break
		}
		eventLines = append(eventLines, fmt.Sprintf("- v%d [id=%d] %s ｜ label=%s ｜ %s ｜ breaking=%d ｜ %s",
			ev.Seq, ev.EventID, orDefault(ev.Title, "(无标题)"), orDefault(ev.Label, "(无标签)"), ev.ChangeType, ev.Breaking, fmtDate(ev.CreatedAt)))
	}
	note := []string{fmt.Sprintf("注: 接口原文（raw_markdown）与导入文档全文不随本工具返回（防整段原文灌进模型）；需要原文请让用户在界面接口详情页/事件详情查看。返回文本已做凭据掩码并受 %d 字符上限约束。", maxReportChars)}
	return render(
		head, meta,
		prefixed("路径参数:", fieldLines(d.PathFields)),
		prefixed("请求参数:", fieldLines(d.RequestFields)),
		prefixed("返回字段:", fieldLines(d.ResponseFields)),
		prefixed("命名结构:", schemaLines),
		prefixed("示例与 curl:", exampleLines),
		prefixed("该接口历史事件（新→旧）:", eventLines),
		note,
	), nil
}

// ===================== action: events =====================

// qEvents 变更事件列表（D2 预算：最多 20 条，seq 新→旧）。
// 取全量再截断：total 必须真实，模型才知道还有多少没看到（R2 同理）。
func (s *queryDoc) qEvents(ctx context.Context, projectName string, limit int) (string, error) {
	p, err := s.needProject(ctx, projectName)
	if err != nil {
		return "", err
	}
	events, err := s.q.ApiDocSkillEvents(ctx, p.ID, 0)
	if err != nil {
		return "", fmt.Errorf("查询变更事件失败: %w", err)
	}
	if limit <= 0 || limit > queryEventsCap {
		limit = queryEventsCap
	}
	if len(events) == 0 {
		return render(
			[]string{fmt.Sprintf("项目 %s 还没有变更事件（事件由增量导入产生；初始化导入不生成事件）。", p.Name)},
			[]string{"要看接口现状用 action=entries。"},
		), nil
	}
	shown := events
	if len(shown) > limit {
		shown = shown[:limit]
	}
	lines := make([]string, 0, len(shown))
	for _, e := range shown {
		lines = append(lines, fmt.Sprintf(
			"- v%d [id=%d] %s ｜ label=%s ｜ %s ｜ breaking=%d ｜ 新增%d 修改%d 删除%d 恢复%d ｜ %s",
			e.Seq, e.ID, orDefault(e.Title, "(无标题)"), orDefault(e.Label, "(无标签)"),
			orDefault(e.Kind, "-"), e.Breaking,
			e.Stats.Added, e.Stats.Updated, e.Stats.Removed, e.Stats.Restored, fmtDate(e.CreatedAt)))
	}
	tail := []string{"看单个事件改了什么（字段级 diff 与「为什么改」）用 action=event + 上面行内 [id=…]。"}
	if len(events) > len(shown) {
		tail = append([]string{fmt.Sprintf("truncated=true：共 %d 个事件，本次只返回最近 %d 个；可按 label 用界面筛选，或逐条用 action=event 看明细。",
			len(events), len(shown))}, tail...)
	}
	return render(
		[]string{fmt.Sprintf("项目 %s 的变更事件（新→旧，共 %d 个）：", p.Name, len(events))},
		lines, tail,
	), nil
}

// ===================== action: event =====================

// qEvent 事件详情：事件头（含 reason——本模块的核心价值「为什么改」）+ 明细逐条字段级 diff。
// D2：明细上限 40 项，超出截断并说明；单条明细的 diff 也限 10 行（超出「从略」）。
func (s *queryDoc) qEvent(ctx context.Context, rawID string) (string, error) {
	if strings.TrimSpace(rawID) == "" {
		return "", fmt.Errorf("参数错误: action=event 需要 id（数字事件 id，取自 action=events 行的 [id=…]）")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("参数错误: 事件 id 必须是数字（收到 %q，取自 events 行的 [id=…]）", rawID)
	}
	v, err := s.q.ApiDocSkillEvent(ctx, id)
	if err != nil {
		return "", fmt.Errorf("查询事件详情失败: %w", err)
	}
	head := []string{
		fmt.Sprintf("变更事件 v%d（id %d）: %s", v.Seq, v.ID, orDefault(v.Title, "(无标题)")),
		fmt.Sprintf("label=%s ｜ kind=%s ｜ breaking=%d ｜ 模式 %s ｜ 来源 %s ｜ 时间 %s",
			orDefault(v.Label, "(无标签)"), orDefault(v.Kind, "-"), v.Breaking,
			orDefault(v.ScopeMode, "-"), orDefault(v.Source, "-"), fmtDate(v.CreatedAt)),
		fmt.Sprintf("统计: 新增 %d、修改 %d、删除 %d、移动 %d、恢复 %d",
			v.Stats.Added, v.Stats.Updated, v.Stats.Removed, v.Stats.Moved, v.Stats.Restored),
		"为什么改(reason): " + orDefault(oneLine(v.Reason), "（文档 meta.change_note 未声明）"),
	}
	if v.DocRef != "" {
		head = append(head, "来源文档: "+v.DocRef)
	}
	lines := make([]string, 0, queryEventItemsCap+1)
	for i, it := range v.Items {
		if i >= queryEventItemsCap {
			lines = append(lines, fmt.Sprintf("- …其余 %d 条明细未展开（本事件共 %d 条；完整明细请让用户看界面事件详情页）",
				len(v.Items)-queryEventItemsCap, len(v.Items)))
			break
		}
		line := fmt.Sprintf("- %s %s ｜ %s ｜ 领域 %s ｜ %s ｜ breaking=%d",
			it.Method, it.Path, orDefault(it.EntryName, "(未命名)"), orDefault(it.DomainName, "未归领域"), it.ChangeType, it.Breaking)
		lines = append(lines, line)
		if it.DeclaredNote != "" {
			lines = append(lines, "  文档声明变更点: "+oneLine(it.DeclaredNote))
		}
		for j, diff := range it.Diff {
			if j >= queryItemDiffCap {
				lines = append(lines, fmt.Sprintf("  - …该接口其余 %d 条差异从略", len(it.Diff)-queryItemDiffCap))
				break
			}
			part := fmt.Sprintf("  - [%s] %s %s ｜ %s → %s", diff.Section, diff.Key, diff.Op, diffSideText(diff.Before), diffSideText(diff.After))
			if diff.Breaking {
				part += "（破坏）"
			} else if diff.NeedsConfirm {
				part += "（需人工确认）"
			}
			if diff.Rule != "" {
				part += " ｜ rule=" + diff.Rule
			}
			lines = append(lines, part)
		}
	}
	if len(v.Items) == 0 {
		lines = append(lines, "- （本次事件没有字段级明细：可能仅同步了说明/示例文本）")
	}
	return render(
		head,
		prefixed("明细（每条接口一行，缩进为其字段级 diff）:", lines),
	), nil
}

// ===================== 渲染小工具（只在本文件用）=====================

// fieldLines 字段表渲染（仅 entry 详情层出现——R2：摘要层绝不调用它）。
func fieldLines(fields []store.ApiField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, fieldLine(f))
	}
	return out
}

// fieldLine 单个字段一行：名称 ｜ 类型 ｜ 必填 ｜ 位置 ｜ 默认 ｜ 说明。
func fieldLine(f store.ApiField) string {
	line := fmt.Sprintf("- %s ｜ %s ｜ %s", f.Name, orDefault(f.TypeRaw, "?"), reqText(f.Required))
	if f.Position != "" {
		line += " ｜ 位置 " + f.Position
	}
	if f.DefaultValue != "" {
		line += " ｜ 默认 " + f.DefaultValue
	}
	if f.Description != "" {
		line += " ｜ " + oneLine(f.Description)
	}
	return line
}

// reqText 必填三态（store.ApiField.Required：-1 未知 | 0 否 | 1 是，零指针哨兵约定）。
func reqText(r int) string {
	switch r {
	case 1:
		return "必填"
	case 0:
		return "可选"
	default:
		return "必填未知"
	}
}

// diffSideText diff 的 before/after（any，多为 FieldFragment/SchemaFragment）压成一行短文本。
// §13.20 契约：载荷是对象不是字符串——这里 json 序列化后压平、限长（超长显式截断标记）。
func diffSideText(v any) string {
	if v == nil {
		return "—"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return sanitize.CapChars(oneLine(fmt.Sprint(v)), queryDiffValueCap)
	}
	return sanitize.CapChars(oneLine(string(b)), queryDiffValueCap)
}

// fmtDate 毫秒时间戳 → 日期时间（0 = 未知，零指针哨兵）。
func fmtDate(ms int64) string {
	if ms <= 0 {
		return "时间未知"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func statusText(status string) string {
	switch status {
	case store.ApiEntryStatusRemoved:
		return "已删除(软删)"
	case store.ApiEntryStatusActive, "":
		return "在用"
	default:
		return status
	}
}

func entryStateOf(state string) string {
	if strings.TrimSpace(state) == "" {
		return "ok"
	}
	return state
}

// 编译期核对：queryDoc 满足 skill.Skill（注册形状与 spec/check/import 三件套一致）。
var _ skill.Skill = (*queryDoc)(nil)
