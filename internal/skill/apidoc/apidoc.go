// Package apidoc 实现接口文档域的 3 个内置技能：
// apidoc_spec（交付规范全文）/ apidoc_check（格式体检）/ apidoc_import（触发导入）。
//
// 为什么拆成 3 个技能而不是 1 个多工具技能（对照仓库既有形状）：
//   - pg 域就是「一个包三个技能」（pg_list_tables / pg_describe_table / pg_query，
//     pg.go:32-39），ops 域同理（ops.go 三个）；一个技能 = 一个 function-calling 工具，
//     工具名越具体，模型路由越准（skill.go:27-28「Description 的质量直接决定路由准确性」）；
//   - 三个能力的副作用层级完全不同：spec 纯读常量、check 只读解析（绝不写库）、
//     import 写库且可能升级为待审批单。混在一个工具里就没法用 Parameters 把
//     「只传路径、正文永不经过模型」这条保真红线（D5.1）钉死在 schema 上。
//
// 保真红线（D5.1 / spec-draft §13.18，本包的立身之本）：
//  1. 文档正文永远由 Go 侧 os.ReadFile 读磁盘，技能参数里只有「文件路径 + 元信息」——
//     LLM 经手的任何正文字节都进不了写入通道（长 Markdown 会被模型截断/重排）；
//  2. 解析/diff/校验/写入全部复用 appsvc 的同一套方法（D8「skill 不另写解析」），
//     本包零解析逻辑；
//  3. chat 路径一律拒绝 scope: init（§13.18：InitMode 与 AllowInitOnNonEmpty 恒 false，
//     且解析出的模式若为 init 直接拒收）——初始化灌库是界面向导的职责；
//  4. 破坏性/删除的确认闸复用 deploy 的审批单骨架（D16），本包不自造确认机制：
//     needConfirm 为真时只创建待审批单并推事件，未批准绝不写入；
//  5. 文档正文衍生物（Issue 原文片段、变更摘要）进 LLM 前先过 internal/sanitize
//     （掩 Token/密码类字段）——规范常量本身是 DevCraft 自己的文本，不掩码，
//     否则 AC9 的「与界面导出字节一致」就破了。
//
// 依赖方向（对照 deploy.go:11-14 / pg.go:15-17）：本包只定义一个小接口 DocService，
// 由 app.go 注入 appsvc 的实现，避免 skill → appsvc 的循环依赖；会话 id 也不在本包
// 取——把 ctx 原样交给 appsvc，由它用 deploy.SessionIDFromCtx 解（ctx 键的唯一持有者
// 仍是 deploy，本包不复制第二个键）。
//
// 注意包名冲突：本技能包也叫 apidoc，故解析器包 internal/apidoc 以别名 docspec 引入
// （只取 SpecText 常量与 Issue 词表，不取解析函数）。
package apidoc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	docspec "DevCraft/internal/apidoc" // SpecText 单一真相源 + Issue/Mode 词表
	"DevCraft/internal/sanitize"       // 正文衍生物入 LLM 前的掩码与体量封顶
	"DevCraft/internal/skill"          // Skill 接口与注册表
)

// 技能名常量（带 apidoc_ 命名空间；Registry.Register 强制名字含下划线，skill.go:56-58，
// 且 function-calling 工具名必须匹配 ^[a-zA-Z0-9_-]+$）。
const (
	SkillSpec   = "apidoc_spec"
	SkillCheck  = "apidoc_check"
	SkillImport = "apidoc_import"
)

// SkillNames 全部 apidoc 技能名（用户手工装配 / 将来一次性补装迁移都用它）。
var SkillNames = []string{SkillSpec, SkillCheck, SkillImport}

// 体量与路径防护（对照 pg 的 DefaultRowLimit/MaxOutputChars 三件套范式）。
const (
	// maxDocBytes 允许读取的文档上限（2 MiB）。规范文档实测几十 KB，
	// 这个闸只挡「模型乱传一个巨型文件」，不误伤正常文档。
	maxDocBytes = 2 << 20
	// maxReportChars 回给模型的报告文本上限（控制 token 成本的兜底）。
	maxReportChars = 12000
)

// ===================== 依赖注入的缝（appsvc 实现，app.go 注入）=====================

// ImportInput 一次校验/导入的输入。字段里刻意没有 docText 的对应物「initMode」
// 之类的开关：chat 路径不允许初始化导入（§13.18），把闸门留在类型上而不是靠调用方自觉。
// DocText 由技能自己读文件填入，永远不取自模型参数。
type ImportInput struct {
	DocText     string // 文档全文（Go 侧读文件得到，非模型传参）
	DocRef      string // 来源路径（仅溯源，落事件的 doc_ref 列）
	ProjectName string // 覆盖 meta.project（空则用文档声明）
	Title       string // 覆盖 meta.change_title
	Reason      string // 覆盖 meta.change_note
	Label       string // 覆盖 meta.version_tag
	Kind        string // 覆盖 meta.change_type（feature/fix/refactor/docs）
	Confirmed   bool   // 破坏性二次确认标记（仅在纯新增/非破坏的直发路径置真）
}

// ImportStats 导入统计（appsvc.ApiDocImportStats 的技能侧镜像，只留展示需要的列）。
type ImportStats struct {
	Added     int
	Updated   int
	Removed   int
	Restored  int
	Unchanged int
}

// ImportChange 逐接口变更摘要（技能只报身份与结论，不回传正文）。
type ImportChange struct {
	Method     string
	Path       string
	Name       string
	Domain     string
	ChangeType string // added|updated|removed|restored|unchanged
	Breaking   int    // 0 无 | 1 破坏 | 2 需人工确认
	ParseState string // ok|partial|unparsed
}

// ImportCheck 确定性校验结论（编排层算出来的，本包零解析）。
type ImportCheck struct {
	OK          bool
	NeedConfirm bool
	InitBlocked bool
	Mode        string // incremental|init（init 在 chat 路径一律被拒收）
	ProjectName string
	Breaking    int
	NextSeq     int
	Message     string
	Notes       []string
	Issues      []docspec.Issue // 结构化清单（含行号），模型据此翻译成人话
	Stats       ImportStats
	Changes     []ImportChange
}

// ImportReceipt 提交回执。Seq==0 = 本次未生成事件（哨兵，零指针约定）。
type ImportReceipt struct {
	EventID        int64
	Seq            int
	Mode           string
	Breaking       int
	Stats          ImportStats
	Message        string
	Notes          []string
	CreatedProject bool
	CreatedDomains []string
}

// DocService 接口文档技能可见的编排层能力（appsvc 实现，方法与既有导出方法一一对应：
// ApiDocSkillCheck → PreviewApiDocImport、ApiDocSkillCommit → CommitApiDocImport，
// 保证 D8「两个入口共用同一套 Go 校验/导入方法」）。
type DocService interface {
	// ApiDocSkillSpec 返回规范全文（与界面导出 api-doc-spec.md 同一个 Go 常量，AC9/D15）。
	ApiDocSkillSpec(ctx context.Context) (string, error)
	// ApiDocSkillCheck 跑确定性解析 + 与库中现状的 diff 预览（只读，绝不写库）。
	ApiDocSkillCheck(ctx context.Context, in ImportInput) (ImportCheck, error)
	// ApiDocSkillCommit 单事务写入（E-* 拒收 / 二次确认闸 / init 闸都在服务层强制）。
	ApiDocSkillCommit(ctx context.Context, in ImportInput) (ImportReceipt, error)
	// ApiDocSkillSubmitApproval 创建「接口文档导入」待审批单并推送审批卡片帧，
	// 返回审批单 id（未批准绝不执行）。复用 deploy 的审批单骨架（D16）。
	ApiDocSkillSubmitApproval(ctx context.Context, in ImportInput) (string, ImportCheck, error)
}

// Register 把 3 个接口文档技能挂载到注册表（注册范式同 ops/deploy/pg）。
// docs 由 app.go 注入（同一个 *appsvc.Service 同时实现四个方法）。
func Register(reg *skill.Registry, docs DocService) error {
	b := base{docs: docs}
	for _, s := range []skill.Skill{
		&specDoc{base: b},
		&checkDoc{base: b},
		&importDoc{base: b},
	} {
		if err := reg.Register(s); err != nil {
			return err
		}
	}
	return nil
}

// base 三个技能共有的依赖与读文件/渲染逻辑（组合复用，对照 pg.base）。
type base struct {
	docs DocService
}

// ===================== 入参解析与文件读取（保真红线的落地位）=====================

// decodeArgs 解技能入参：必须是 JSON 对象，且键全部在白名单内。
// 白名单里刻意没有 docText/content——正文只能来自技能自己读的文件（D5.1）；
// 模型硬塞正文进来时当场拒绝，绝不让它混进任何链路（对照 deploy.go:194-198
// 「未声明的参数拒绝（防误用）」；schema 的 additionalProperties:false 只是提示，
// 真正的闸门在这里）。
func decodeArgs(args json.RawMessage, allow []string, target any) error {
	raw := args
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("参数错误: 调用参数不是合法 JSON 对象")
	}
	for k := range obj {
		if !apiDocArgAllowed(allow, k) {
			return fmt.Errorf("参数错误: 不接受参数 %q（本技能只接受 %s；"+
				"文档正文不作为参数传递，技能会自己读 path 指向的文件）",
				k, strings.Join(allow, "/"))
		}
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("参数错误: 调用参数无法解析")
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("参数错误: 调用参数不符合 schema: %v", err)
	}
	return nil
}

// apiDocArgAllowed 参数键是否在白名单内。
func apiDocArgAllowed(allow []string, key string) bool {
	for _, a := range allow {
		if a == key {
			return true
		}
	}
	return false
}

// readDoc 读磁盘上的文档全文。路径由用户/模型给出，本包只做「读」，
// 绝不改写一个字节（读到的原文直接进 ImportInput.DocText → 解析器）。
// 三道防御：空路径拒绝 / 大小封顶（maxDocBytes）/ 空白文档拒绝。
// 错误文案里带上解析后的绝对路径——模型要靠它告诉用户「到底读的是哪个文件」。
func (b base) readDoc(path string) (text, ref string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", fmt.Errorf("参数错误: 需要提供 path（接口变更文档的文件路径）")
	}
	abs, absErr := filepath.Abs(path)
	if absErr == nil {
		ref = filepath.ToSlash(abs)
	} else {
		ref = filepath.ToSlash(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("读不到文档 %s: %v（请向用户确认文件路径是否存在）", ref, err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("%s 是目录不是文档文件，已拒绝读取", ref)
	}
	if info.Size() > maxDocBytes {
		return "", "", fmt.Errorf("文档 %s 超过 %d MB，技能不处理（请改用界面导入向导）",
			ref, maxDocBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("读取文档失败 %s: %v", ref, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", "", fmt.Errorf("文档内容为空: %s", ref)
	}
	return string(data), ref, nil
}

// issueList 把结构化 Issue 压成给模型看的行（E/W 分栏由各调用点决定；
// 逐条都是 code + 行号 + 期望 + 原文片段，行号必给——spec-draft §9「AI 能把错误码
// 翻译成人话的前提」）。原文片段是文档正文衍生物 → 入 LLM 前掩码。
func issueList(issues []docspec.Issue, level string) []string {
	out := make([]string, 0, len(issues))
	n := 0
	for _, is := range issues {
		if is.Level != level {
			continue
		}
		n++
		part := fmt.Sprintf("- 第%d行 %s", is.Line, is.Code)
		if is.Want != "" {
			part += "：期望 " + is.Want
		}
		if is.Hint != "" {
			part += "；" + is.Hint
		}
		if is.Snippet != "" {
			part += "｜原文: " + sanitize.MaskSecrets(oneLine(is.Snippet))
		}
		out = append(out, part)
	}
	if n == 0 {
		return nil
	}
	return out
}

// changeLines 变更分栏的逐行摘要（身份 + 结论 + 破坏档，不含正文）。
func changeLines(changes []ImportChange, max int) []string {
	out := make([]string, 0, len(changes))
	for i, c := range changes {
		if i >= max {
			out = append(out, fmt.Sprintf("- …其余 %d 条从略", len(changes)-max))
			return out
		}
		line := fmt.Sprintf("- %s %s（%s / 领域 %s）→ %s", c.Method, c.Path, c.Name, c.Domain, c.ChangeType)
		if c.Breaking > 0 {
			line += fmt.Sprintf("，breaking=%d", c.Breaking)
		}
		if c.ParseState != "" && c.ParseState != "ok" {
			line += "，解析状态 " + c.ParseState + "（需人工补全）"
		}
		out = append(out, line)
	}
	return out
}

// statsLine 统计一行话。
func statsLine(st ImportStats) string {
	return fmt.Sprintf("新增 %d、修改 %d、删除 %d、恢复 %d、未变更 %d",
		st.Added, st.Updated, st.Removed, st.Restored, st.Unchanged)
}

// noteLines 中文提示（编排层的降级提示，不新增错误码，§13.10）。
func noteLines(notes []string) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, "- "+sanitize.MaskSecrets(oneLine(n)))
	}
	return out
}

// oneLine 把多行文本压成单行（Issue 原文片段可能带换行，压平后清单才可读）。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// render 把若干段落拼成最终回给模型的文本。
// 出网前统一「先掩码再截断」（与 deploy.sanitizeStepOutput 同一套防护顺序）：
// 每个调用点已经对正文字段掩过一次，这里再整体兜一遍，防止将来新增字段漏掩。
func render(paragraphs ...[]string) string {
	var b strings.Builder
	for _, p := range paragraphs {
		if len(p) == 0 {
			continue
		}
		for _, line := range p {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return sanitize.CapChars(sanitize.MaskSecrets(strings.TrimRight(b.String(), "\n")), maxReportChars)
}

// ===================== apidoc_spec =====================

// specDoc 交付规范全文（D15/R12：与界面导出的 api-doc-spec.md 是同一个 Go 常量）。
type specDoc struct{ base }

func (s *specDoc) Name() string { return SkillSpec }

func (s *specDoc) Description() string {
	return "获取 DevCraft《接口变更文档规范 v1》全文（与界面导出的 api-doc-spec.md 字节一致的同一份文本）。" +
		"当用户要求「把这次业务改动写成接口变更文档 / 接口文档怎么写 / 按规范生成变更文档」时，先调用本技能拿到规范，" +
		"然后严格按规范里的三段式、meta 字段字典、#### 小节 kind、参数表列字典与错误码表来写文档。" +
		"硬规则：URL 行必须写成 - **URL**：`METHOD /path`（全角冒号 + 反引号）；每个接口必须写 **变更**，" +
		"修改/删除必须写 **变更点**；禁止「同新增订阅。」这类引用式写法；请求/路径参数表必须带「位置」列。" +
		"不要为了「让校验通过」而删字段或改类型——校验器只读不改，写什么就入库什么。" +
		"本技能只输出规范文本，不读取任何文件、不写库。"
}

func (s *specDoc) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{},
		"additionalProperties":false
	}`)
}

// Execute 原样返回规范常量——不掩码、不加包装文案（AC9 要求与界面导出逐字节相等）。
func (s *specDoc) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if err := decodeArgs(args, nil, &struct{}{}); err != nil {
		return "", err
	}
	text, err := s.docs.ApiDocSkillSpec(ctx)
	if err != nil {
		return "", fmt.Errorf("读取接口变更文档规范失败: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("内部错误: 规范文本为空（单一真相源常量不应为空）")
	}
	return text, nil
}

// ===================== apidoc_check =====================

// checkDoc 格式体检：Go 侧读文件跑确定性解析，把结构化 Issue 交给模型转述成人话。
// 这是本模块唯一允许把文档正文衍生物喂给 LLM 的入口（D5.1），且只诊断不写库。
type checkDoc struct{ base }

func (s *checkDoc) Name() string { return SkillCheck }

func (s *checkDoc) Description() string {
	return "对一份「接口变更文档」做格式体检：按给定文件路径由程序读取原文，跑 DevCraft 的确定性解析器，" +
		"返回结构化问题清单（错误码 + 行号 + 期望写法 + 原文片段）以及导入会产生的变更统计（只读预览，绝不写库）。" +
		"当用户想知道某份文档写得对不对、为什么导入会失败、导入前想先确认格式时使用。" +
		"参数只需要文件路径，绝不要把文档正文塞进参数里（正文由程序自己读磁盘，防止内容被改写）。" +
		"拿到结果后用中文逐条向用户解释怎么改；不要替用户「顺手修复」后声称已导入——本技能什么都不写。" +
		"需要真正导入请调用 apidoc_import。"
}

func (s *checkDoc) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"接口变更文档的文件路径（绝对路径最稳妥；技能自己读文件，正文不经过模型）"},
			"project":{"type":"string","description":"可选：覆盖文档 meta 里的项目标识（一个后端服务/仓库，如 paper-manager）"}
		},
		"required":["path"],
		"additionalProperties":false
	}`)
}

// checkArgs 体检技能的参数白名单（没有 docText：正文只从文件读，D5.1）。
var checkArgs = []string{"path", "project"}

// importArgs 导入技能的参数白名单（同上：只有路径与元信息）。
var importArgs = []string{"path", "project", "title", "reason", "label", "kind"}

// Execute 读文件 → 复用 appsvc 预览（与界面向导同一个方法）→ 渲染结构化清单。
// 全程只读：本技能永不触碰 CommitApiDocImport。
func (s *checkDoc) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Project string `json:"project"`
	}
	if err := decodeArgs(args, checkArgs, &in); err != nil {
		return "", err
	}
	text, ref, err := s.readDoc(in.Path)
	if err != nil {
		return "", err
	}
	ck, err := s.docs.ApiDocSkillCheck(ctx, ImportInput{
		DocText: text, DocRef: ref, ProjectName: strings.TrimSpace(in.Project),
	})
	if err != nil {
		return "", fmt.Errorf("接口文档体检失败: %w", err)
	}
	return render(
		[]string{fmt.Sprintf("体检文件: %s（项目 %s，模式 %s）", ref, ck.ProjectName, ck.Mode)},
		[]string{fmt.Sprintf("结论: %s", sanitize.MaskSecrets(ck.Message))},
		[]string{fmt.Sprintf("统计: %s；将生成事件 v%d；破坏档 %d（需确认=%t）",
			statsLine(ck.Stats), ck.NextSeq, ck.Breaking, ck.NeedConfirm)},
		prefixed("阻断问题（E-*，导入会被整批拒收）:", issueList(ck.Issues, docspec.LevelError)),
		prefixed("警告（W-*，预览需人工核对）:", issueList(ck.Issues, docspec.LevelWarning)),
		prefixed("变更分栏:", changeLines(ck.Changes, 40)),
		prefixed("提示:", noteLines(ck.Notes)),
		checkHint(ck),
	), nil
}

// checkHint 体检结论的行动建议（模型照着转述；把「只诊断不写库」讲死）。
func checkHint(ck ImportCheck) []string {
	out := []string{"以上仅为诊断：本技能没有写入任何数据。"}
	if !ck.OK {
		out = append(out, "文档存在阻断问题，请用中文逐条告诉用户怎么改（错误码 + 第几行 + 应该写成什么）。")
		return out
	}
	if ck.Mode == docspec.ModeInit {
		out = append(out, "该文档声明 scope: init（初始化灌库），chat 路径一律拒绝执行，请让用户在界面导入向导里做初始化。")
		return out
	}
	out = append(out, "格式校验通过，可用 apidoc_import（同一个 path）触发导入。")
	if ck.NeedConfirm {
		out = append(out, "注意：本文档含接口删除或破坏性变更，导入会生成待审批单，必须用户在卡片里批准后才落库。")
	}
	return out
}

// prefixed 给清单加一个小标题（空清单整段省略，避免输出「标题:」孤儿行）。
func prefixed(title string, lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	return append([]string{title}, lines...)
}

// ===================== apidoc_import =====================

// importDoc 导入触发器：正文由 Go 侧读文件，校验/diff/写入全走 appsvc。
type importDoc struct{ base }

func (s *importDoc) Name() string { return SkillImport }

func (s *importDoc) Description() string {
	return "导入一份「接口变更文档」：按文件路径由程序读取原文（正文绝不经过模型），用 DevCraft 的确定性解析器校验、" +
		"与库中现状算字段级 diff，并在单个事务里更新接口定义 + 生成一条变更事件。" +
		"当用户要求把某份接口变更文档导入 DevCraft / 更新接口库 / 登记这次接口变更时使用。" +
		"参数只给文件路径与可选的元信息（项目名/标题/原因/标签/类型），绝不要把文档正文粘进参数里。" +
		"规则：① 纯新增或非破坏修改会直接写入并回报事件版本与统计；② 含接口删除或破坏性变更（breaking=1/2）" +
		"会生成待审批单并推送审批卡片，必须用户点击批准后才落库——此时如实告诉用户已生成审批单、等待批准，" +
		"绝不能声称导入已完成；③ 校验未通过会被整批拒收，把返回的错误码清单逐条翻译给用户；" +
		"④ 初始化导入（scope: init）在聊天里一律被拒绝，请引导用户走界面导入向导。"
}

func (s *importDoc) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"接口变更文档的文件路径（技能自己读文件，正文不经过模型）"},
			"project":{"type":"string","description":"可选：覆盖文档 meta 里的项目标识（一个后端服务/仓库）"},
			"title":{"type":"string","description":"可选：覆盖文档 meta.change_title（变更事件标题）"},
			"reason":{"type":"string","description":"可选：覆盖文档 meta.change_note（为什么改）"},
			"label":{"type":"string","description":"可选：覆盖文档 meta.version_tag（需求单号/发布标签）"},
			"kind":{"type":"string","enum":["feature","fix","refactor","docs"],"description":"可选：覆盖文档 meta.change_type"}
		},
		"required":["path"],
		"additionalProperties":false
	}`)
}

// Execute 读文件 → 预览 → 按 D16 分流：
//   - 非破坏/纯新增 → 直接 CommitApiDocImport(confirmed=true) 并回报事件与统计；
//   - 含删除或破坏（needConfirm）→ 创建待审批单（复用 deploy 审批闸骨架），未批准绝不执行；
//   - 任何 E-* → 整批拒收（后端已保证零写入），把 code + 行号 + 期望交回模型说明。
//
// init 模式在这里被硬性拒收（§13.18：chat 路径 InitMode/AllowInitOnNonEmpty 恒 false，
// 且解析结果为 init 时不提交）——这是服务层闸门之外的第二道「入口身份」闸。
func (s *importDoc) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Project string `json:"project"`
		Title   string `json:"title"`
		Reason  string `json:"reason"`
		Label   string `json:"label"`
		Kind    string `json:"kind"`
	}
	if err := decodeArgs(args, importArgs, &in); err != nil {
		return "", err
	}
	switch strings.TrimSpace(in.Kind) {
	case "", "feature", "fix", "refactor", "docs":
	default:
		return "", fmt.Errorf("参数错误: kind 只能是 feature/fix/refactor/docs（收到 %q）", in.Kind)
	}

	text, ref, err := s.readDoc(in.Path)
	if err != nil {
		return "", err
	}
	req := ImportInput{
		DocText:     text,
		DocRef:      ref,
		ProjectName: strings.TrimSpace(in.Project),
		Title:       strings.TrimSpace(in.Title),
		Reason:      strings.TrimSpace(in.Reason),
		Label:       strings.TrimSpace(in.Label),
		Kind:        strings.TrimSpace(in.Kind),
	}

	// 1. 先跑与界面向导完全相同的确定性校验（同一套 appsvc 方法，D8）。
	ck, err := s.docs.ApiDocSkillCheck(ctx, req)
	if err != nil {
		return "", fmt.Errorf("接口文档校验失败: %w", err)
	}
	if !ck.OK || len(issueList(ck.Issues, docspec.LevelError)) > 0 {
		return "", fmt.Errorf("接口文档校验未通过，已整批拒收（数据库零变更）：%s\n%s",
			sanitize.MaskSecrets(ck.Message),
			strings.Join(issueList(ck.Issues, docspec.LevelError), "\n"))
	}
	// 2. chat 路径的 init 闸（§13.18）：文档自声明 scope: init 也一律拒执行。
	if ck.Mode == docspec.ModeInit {
		return "", fmt.Errorf("文档声明了 scope: init（初始化灌库），聊天里一律拒绝执行初始化导入；"+
			"请让用户在界面导入向导里勾选初始化模式（项目 %s）", ck.ProjectName)
	}
	if ck.InitBlocked {
		return "", fmt.Errorf("初始化导入被服务层闸门拦截（项目 %s 已有接口条目），已拒绝执行", ck.ProjectName)
	}

	// 3. 破坏性/删除 → 审批闸（复用 deploy 的待审批单形状，D16；本包不自造确认机制）。
	if ck.NeedConfirm {
		approvalID, _, err := s.docs.ApiDocSkillSubmitApproval(ctx, req)
		if err != nil {
			return "", fmt.Errorf("创建接口文档审批单失败: %w", err)
		}
		return render(
			[]string{fmt.Sprintf("已生成接口文档导入审批单（编号 %s），文件 %s，项目 %s", approvalID, ref, ck.ProjectName)},
			[]string{fmt.Sprintf("原因：破坏档 %d，%s", ck.Breaking, statsLine(ck.Stats))},
			prefixed("变更分栏:", changeLines(ck.Changes, 40)),
			[]string{"在用户于审批卡片点击「批准」之前绝不会写入任何数据。"},
			[]string{"请转告用户：批准后本次导入才会落库并生成事件（预计 v" + fmt.Sprint(ck.NextSeq) +
				"）；拒绝或不操作则一行不写。不要声称导入已经完成。"},
		), nil
	}

	// 4. 纯新增 / 非破坏修改 → 直接写入（D16：校验通过即生效，无需二次确认）。
	req.Confirmed = true
	rc, err := s.docs.ApiDocSkillCommit(ctx, req)
	if err != nil {
		return "", fmt.Errorf("接口文档导入失败（事务已回滚，数据库零变更）: %w", err)
	}
	head := fmt.Sprintf("导入成功：文件 %s，项目 %s，模式 %s，%s", ref, ck.ProjectName, rc.Mode, statsLine(rc.Stats))
	if rc.EventID > 0 {
		head = fmt.Sprintf("导入成功：文件 %s，项目 %s，模式 %s，%s；已生成变更事件 v%d（事件 id %d）",
			ref, ck.ProjectName, rc.Mode, statsLine(rc.Stats), rc.Seq, rc.EventID)
	} else {
		head += "；本次未生成事件（diff 为空或初始化模式）"
	}
	created := make([]string, 0, len(rc.CreatedDomains)+1)
	if rc.CreatedProject {
		created = append(created, "- 本次新建了项目")
	}
	for _, d := range rc.CreatedDomains {
		created = append(created, "- 本次新建领域: "+sanitize.MaskSecrets(d))
	}
	return render(
		[]string{head},
		[]string{"结论: " + sanitize.MaskSecrets(rc.Message)},
		prefixed("变更分栏:", changeLines(ck.Changes, 40)),
		prefixed("本次新建:", created),
		prefixed("提示:", noteLines(rc.Notes)),
	), nil
}
