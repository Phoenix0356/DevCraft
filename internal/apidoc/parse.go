package apidoc

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ===================== 模式与枚举（字符串常量对，不用 Go enum）=====================

// 解析模式（= meta 的 scope 取值）。
const (
	ModeIncremental = "incremental" // 规范模式：严格校验
	ModeInit        = "init"        // 初始化模式：放宽 + 推断 + 逐条警告，不产生事件
	ModeFull        = "full"        // 可识别但未实现（D17）：E-SCOPE-FULL-NOT-SUPPORTED
)

// **变更** bullet 取值。
const (
	ChangeAdded     = "新增"
	ChangeUpdated   = "修改"
	ChangeRemoved   = "删除"
	ChangeUnchanged = "未变更"
)

// **认证** bullet 枚举（参与 breaking 判定，见 diff.go）。
const (
	AuthLogin    = "需要登录"
	AuthNoLogin  = "无需登录"
	AuthInternal = "内部Token"
)

// 位置列枚举。
const (
	PosPath   = "Path"
	PosQuery  = "Query"
	PosBody   = "Body"
	PosHeader = "Header"
)

// parse_state 取值（存 api_entries.parse_state）。
const (
	ParseOK       = "ok"
	ParsePartial  = "partial"
	ParseUnparsed = "unparsed"
)

// 小节 kind（#### 标题前缀匹配归一，7 种）。
const (
	KindRequestParams   = "request_params"
	KindPathParams      = "path_params"
	KindResponseParams  = "response_params"
	KindRequestExample  = "request_example"
	KindResponseExample = "response_example"
	KindErrorExample    = "error_example"
	KindCURL            = "curl"
)

// 示例块 kind（存 api_examples.kind）。
const (
	ExampleRequest   = "req_example"
	ExampleResponse  = "res_example"
	ExampleCURL      = "curl"
	ExampleMultipart = "multipart_raw"
	ExampleError     = "error_example"
)

// Required 哨兵值（与 api_fields.required 一致）：列缺失/未知不是「否」。
const (
	RequiredUnknown = -1
	RequiredNo      = 0
	RequiredYes     = 1
)

// Options 解析选项。
type Options struct {
	// Mode 是兜底解析模式：文档没有 apidoc-meta 块（或块里缺 scope）时生效。
	// 取值 ModeIncremental（默认，规范严格模式）/ ModeInit。
	// meta 块存在且 scope 合法时以 meta.scope 为准（文档自己声明档位）。
	Mode string
}

// ===================== 解析产物 =====================

// Meta 文档级元信息（```apidoc-meta 围栏块，只允许一层 key: value）。
type Meta struct {
	Present     bool   `json:"present"`
	Line        int    `json:"line"` // 围栏起始行（1-based）
	Spec        string `json:"spec"`
	Project     string `json:"project"`
	Service     string `json:"service"`
	BasePath    string `json:"basePath"`
	Scope       string `json:"scope"`
	ChangeType  string `json:"changeType"`
	ChangeTitle string `json:"changeTitle"`
	ChangeNote  string `json:"changeNote"`
	VersionTag  string `json:"versionTag"`
}

// meta 键字典（字典外的键 → E-META-UNKNOWN-KEY）。
var metaKeys = map[string]bool{
	"spec": true, "project": true, "service": true, "base_path": true,
	"scope": true, "change_type": true, "change_title": true,
	"change_note": true, "version_tag": true,
}

// Field 参数/字段表的一行。Required 用 -1/0/1 哨兵（-1 = 表里根本没有必填列，未知≠否）。
type Field struct {
	Name     string `json:"name"`
	TypeRaw  string `json:"typeRaw"` // 显示保真：List\<Long\> / boolean 原样
	TypeKey  string `json:"typeKey"` // 比较归一（identity.go TypeKey）
	Required int    `json:"required"`
	Position string `json:"position"` // Path|Query|Body|Header|''（响应字段）
	Default  string `json:"default"`
	Desc     string `json:"desc"`
	Line     int    `json:"line"` // 1-based
}

// Example 示例与 curl：整块原文，永不结构化、不参与 semantic diff（D9）。
type Example struct {
	Kind     string `json:"kind"` // req_example|res_example|curl|multipart_raw|error_example
	Language string `json:"language"`
	Content  string `json:"content"`
	Line     int    `json:"line"`
	Sort     int    `json:"sort"`
}

// SectionState 参数小节的存在状态。
// 散文归类必须区分「写了 无。」与「根本没有这个小节」——前者是明确无参数，后者是信息缺失。
type SectionState string

const (
	SectionAbsent   SectionState = "absent"    // 根本没有该 #### 小节 = 信息缺失（Unresolved）
	SectionEmpty    SectionState = "empty"     // 明确声明无参数（「无。」「无请求体。」等）
	SectionTable    SectionState = "table"     // 有参数表
	SectionCrossRef SectionState = "cross_ref" // 引用式写法（「同新增订阅。」），规范模式一律拒收
	SectionProse    SectionState = "prose"     // 其他无法识别的散文
)

// TableHeader 表头列集合（只参与 doc_hash，不进 semantic_hash）。
type TableHeader struct {
	Section string   `json:"section"` // request_params|path_params|response_params|schema:<Name>
	Cols    []string `json:"cols"`
	Line    int      `json:"line"`
}

// SchemaDef 命名结构（**Xxx 结构**： + 表）。主键 (name, owner_scope)；
// 同名不同构 → Ambiguous=1 并拒绝按名自动展开，只报 W-STRUCT-DIVERGED。
type SchemaDef struct {
	Name        string  `json:"name"`
	OwnerScope  string  `json:"ownerScope"` // '' = 文档级；否则 = 宿主接口 identity_key
	OwnerIdx    int     `json:"ownerIdx"`   // Document.Endpoints 索引；-1 = 文档级
	Fields      []Field `json:"fields"`
	BodyHash    string  `json:"bodyHash"`
	Ambiguous   bool    `json:"ambiguous"`
	Line        int     `json:"line"`
	RawMarkdown string  `json:"rawMarkdown"`
}

// Endpoint 接口条目（### 块）。只存当前态语义；身份 = IdentityKey。
type Endpoint struct {
	Seq       string `json:"seq"` // "8.2" —— 不进身份、不参与任何逻辑，仅一致性断言
	Name      string `json:"name"`
	Line      int    `json:"line"` // ### 标题行（1-based）
	EndLine   int    `json:"endLine"`
	DomainIdx int    `json:"domainIdx"` // Document.Domains 索引；-1 = 不在任何 ## 下

	Method      string `json:"method"`
	PathRaw     string `json:"pathRaw"` // 原样（含 {id}），渲染与复制 curl 用
	IdentityKey string `json:"identityKey"`

	ContentType   string `json:"contentType"`
	Auth          string `json:"auth"`
	AuthInherited bool   `json:"authInherited"` // 由章级 preamble 下渗而来
	Header        string `json:"header"`        // **Header** bullet 原文（如 X-Internal-Token）

	Change       string `json:"change"` // 新增|修改|删除|未变更|''（未声明，init 模式允许）
	ChangePoint  string `json:"changePoint"`
	BreakingDecl int    `json:"breakingDecl"` // -1 未声明 | 0 false | 1 true

	Description string `json:"description"`

	PathFields     []Field       `json:"pathFields"`
	RequestFields  []Field       `json:"requestFields"` // Position ∈ Query|Body|Header
	ResponseFields []Field       `json:"responseFields"`
	Schemas        []SchemaDef   `json:"schemas"` // 本接口拥有的命名结构（同名冲突标记已同步）
	Examples       []Example     `json:"examples"`
	TableHeaders   []TableHeader `json:"tableHeaders"`

	ReqParamsState  SectionState `json:"reqParamsState"`
	PathParamsState SectionState `json:"pathParamsState"`
	RespParamsState SectionState `json:"respParamsState"`

	Unresolved  []string `json:"unresolved"` // 没解析出来的槽位（不是整条丢掉）
	ParseState  string   `json:"parseState"` // ok | partial
	RawMarkdown string   `json:"rawMarkdown"`

	SemanticHash string `json:"semanticHash"`
	DocHash      string `json:"docHash"`
}

// Domain 领域 = 文档一级 ## 章节（结构规则判定：该 ## 下存在带 **URL** bullet 的 ###）。
type Domain struct {
	Name        string `json:"name"` // 归一后的领域名
	RawTitle    string `json:"rawTitle"`
	Internal    bool   `json:"internal"` // 由「（内部）」限定语降级而来
	NotesRaw    string `json:"notesRaw"` // 章级 preamble（认证方式/调用方），下渗给子接口
	Sort        int    `json:"sort"`
	Line        int    `json:"line"`
	EndpointIdx []int  `json:"endpointIdx"` // Document.Endpoints 索引（文档序）
}

// UnparsedBlock 无法解析的块（无合法 URL 的 ### / 孤儿代码块）：原文切片保真兜底。
type UnparsedBlock struct {
	Line        int    `json:"line"`
	Heading     string `json:"heading"`
	RawMarkdown string `json:"rawMarkdown"`
}

// SummaryRow 「接口汇总」表的一行。⚠️ 只能校验到 (method, path)：
// 实测 12 处「接口」列名称与 H3 标题不一致，绝不可用名称做 join key。
type SummaryRow struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Desc   string `json:"desc"`
	Key    string `json:"key"` // upper(method)|normPath
	Line   int    `json:"line"`
}

// SummaryCheck C1 汇总闭合自检（报告用，不作删除依据）。
type SummaryCheck struct {
	Present     bool         `json:"present"`
	Line        int          `json:"line"`
	Rows        []SummaryRow `json:"rows"`
	BodyOnly    []string     `json:"bodyOnly"`    // 正文有、汇总表没有（identity_key）
	SummaryOnly []string     `json:"summaryOnly"` // 汇总表有、正文没有
	Closed      bool         `json:"closed"`      // 双向差集 = 0
}

// Document 一份文档的完整解析结果。所有切片保持文档序（确定性：同文档解析两次字节一致）。
type Document struct {
	Title     string          `json:"title"`
	Mode      string          `json:"mode"` // 生效模式 incremental|init|full
	Meta      Meta            `json:"meta"`
	BasePath  string          `json:"basePath"` // meta.base_path 优先；概述「**基础路径**」散文行兜底（§13.17）
	Domains   []Domain        `json:"domains"`
	Endpoints []*Endpoint     `json:"endpoints"`
	Unparsed  []UnparsedBlock `json:"unparsed"`
	Schemas   []SchemaDef     `json:"schemas"` // 命名结构 registry（endpoint 级 + 文档级）
	Summary   SummaryCheck    `json:"summary"`
	Issues    []Issue         `json:"issues"`
}

// DomainOf 返回接口所属领域（无则 nil）。
func (d *Document) DomainOf(ep *Endpoint) *Domain {
	if ep == nil || ep.DomainIdx < 0 || ep.DomainIdx >= len(d.Domains) {
		return nil
	}
	return &d.Domains[ep.DomainIdx]
}

// VisibilityOf 接口可见性：内部领域的接口 → internal（入库并默认过滤，不能跳过，否则计数对不上）。
func (d *Document) VisibilityOf(ep *Endpoint) string {
	if dom := d.DomainOf(ep); dom != nil && dom.Internal {
		return "internal"
	}
	return "public"
}

// FindEndpoint 按 identity_key 查接口（nil = 没有）。
func (d *Document) FindEndpoint(identityKey string) *Endpoint {
	for _, ep := range d.Endpoints {
		if ep.IdentityKey == identityKey {
			return ep
		}
	}
	return nil
}

// ===================== 正则（全部包级编译一次）=====================

var (
	// 严格 bullet：- **Label**：value（全角冒号，标签用 ** 包裹，冒号在粗体外）
	reBulletStrict = regexp.MustCompile(`^-\s+\*\*([^*]+)\*\*：\s*(.*)$`)
	// 近似 bullet（方言探测，只为报 Issue 不追方言）：**URL:** / **URL**: 这类半角变体
	reBulletNearMiss = regexp.MustCompile(`^-\s+\*\*(URL|Content-Type|变更点|变更|breaking|认证|Header)`)
	reH3Seq          = regexp.MustCompile(`^(\d+(?:\.\d+)*)\s+(.*)$`)
	// 领域名归一：1) 剥序号前缀 2) 剥尾部括号限定语
	reNumPrefix  = regexp.MustCompile(`^(?:[一二三四五六七八九十百零]+、|\d+[、.．])\s*`)
	reTailParen  = regexp.MustCompile(`[（(]([^（）()]*)[）)]\s*$`)
	reBoldTitle  = regexp.MustCompile(`^\*\*(.+)\*\*[:：]?\s*$`)
	reBasePath   = regexp.MustCompile(`^\*\*基础路径\*\*[:：]\s*(.*)$`)
	reSeeRef     = regexp.MustCompile(`见\s*[0-9]+(?:\.[0-9]+)*`)
	reCURLMethod = regexp.MustCompile(`(?:-X|--request)\s+([A-Za-z]+)`)
	reCURLURL    = regexp.MustCompile(`https?://[^"'\s\\]+`)
)

// ===================== 解析器内部结构 =====================

type fenceBlock struct {
	start0  int // 开栏行（0-based，含）
	end0    int // 闭栏行（0-based，含；未闭合 = 末行）
	info    string
	content string // 栏内内容（不含围栏行）
}

type h4Section struct {
	title    string
	kind     string // 7 种之一；'' = 未知（W-UNKNOWN-SECTION）
	heading0 int
	start0   int
	end0     int // 独占
}

type h3Block struct {
	chapIdx  int // -1 = 顶层
	title    string
	heading0 int
	start0   int
	end0     int // 独占
	sections []h4Section
}

type chapter struct {
	title    string
	heading0 int
	start0   int
	end0     int // 独占
	h3s      []int
}

type mdRow struct {
	line0 int
	cells []string
}

type mdTable struct {
	header0   int
	cols      []string
	rows      []mdRow
	boldTitle string // 前置 **Xxx 结构**： 标题（'' = 无）
	bold0     int    // 标题行（-1 = 无）
	end0      int    // 最后一数据行（含）
	summary   bool   // 表头含 方法+URL = 接口汇总表
}

// sectionContent 一个参数小节路由后的内容。
type sectionContent struct {
	present    bool
	table      *mdTable // 主表（第一张无粗体标题的表）
	prose      string
	proseLine0 int
}

// blockState 与 blocks 平行的解析状态。
type blockState struct {
	urlish bool // 存在 URL bullet（含近似变体）——领域结构判定的依据
	ep     *Endpoint
	req    sectionContent
	path   sectionContent
	resp   sectionContent
}

type parser struct {
	lines   []string
	inFence []bool
	fences  []fenceBlock

	chapters []chapter
	blocks   []h3Block
	states   []blockState
	tables   []*mdTable

	doc    *Document
	mode   string
	strict bool

	metaFound   bool
	metaFence   *fenceBlock
	metaKeyLine map[string]int
}

// Parse 确定性解析一份接口文档（9 趟顺序不可颠倒）：
// fence 区间 → 标题栈/章节分类 → bullet 元信息 → 表格（表头驱动）→ #### 分桶
// → 散文归类 → 命名结构 registry → 汇总表 join 自检 → identity + 双 hash。
//
// fence 区间必须最先跑：样例里有无语言标记的 multipart 原始报文块，
// 块内含 Content-Type:/Content-Disposition:，不排除区间就会污染元数据与参数名集合。
func Parse(text string, opts Options) *Document {
	p := &parser{doc: &Document{}, metaKeyLine: map[string]int{}}
	p.lines = splitLines(text)
	p.inFence = make([]bool, len(p.lines))

	p.passFences()   // 趟 1：fence 区间（必须最先）
	p.passHeadings() // 趟 2a：标题栈
	p.parseMetaBlock()
	p.resolveMode(opts)
	p.validateMeta()
	p.passBullets()   // 趟 3：bullet 元信息
	p.buildDomains()  // 趟 2b：章节分类（结构规则依赖趟 3 的 URL bullet 探测结果）
	p.passTables()    // 趟 4：表格收集（表头驱动，绝不按列序）
	p.routeSections() // 趟 5：#### 分桶（7 种 kind，前缀匹配）+ 内容路由
	p.classifyProse() // 趟 6：散文归类
	p.buildSchemas()  // 趟 7：命名结构 registry
	p.checkSummary()  // 趟 8：汇总表 join 自检（C1）
	p.finalize()      // 趟 9：identity + 双 hash
	return p.doc
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}

// ===================== 趟 1：fence 区间 =====================

func (p *parser) passFences() {
	open := -1
	var info string
	for i, line := range p.lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "```") && !strings.HasPrefix(t, "~~~") {
			continue
		}
		if open < 0 {
			open = i
			info = strings.TrimSpace(t[3:])
		} else {
			content := strings.Join(p.lines[open+1:i], "\n")
			p.fences = append(p.fences, fenceBlock{start0: open, end0: i, info: info, content: content})
			for k := open; k <= i; k++ {
				p.inFence[k] = true
			}
			open = -1
		}
	}
	if open >= 0 { // 未闭合围栏：闭到文末，报 W 保真兜底
		content := strings.Join(p.lines[open+1:], "\n")
		p.fences = append(p.fences, fenceBlock{start0: open, end0: len(p.lines) - 1, info: info, content: content})
		for k := open; k < len(p.lines); k++ {
			p.inFence[k] = true
		}
		p.issue(LevelWarning, WarnUnparsedBlock, open+1, "未闭合的代码围栏", "``` 成对出现", "围栏未闭合，块内内容已全部按代码块处理（不参与元数据/表格解析）")
	}
}

// ===================== 趟 2a：标题栈 =====================

func (p *parser) passHeadings() {
	curChap := -1
	curBlock := -1
	curSec := -1
	for i, line := range p.lines {
		if p.inFence[i] {
			continue
		}
		level, title := headingLevel(line)
		if level == 0 {
			continue
		}
		switch {
		case level == 1:
			if curBlock >= 0 {
				p.blocks[curBlock].end0 = i // 顶层块也要在 H1/H2 处收口
			}
			curSec, curBlock, curChap = -1, -1, -1
			if p.doc.Title == "" {
				p.doc.Title = strings.TrimSpace(title)
			}
		case level == 2:
			if curBlock >= 0 {
				p.blocks[curBlock].end0 = i
			}
			curSec, curBlock = -1, -1
			if curChap >= 0 {
				p.chapters[curChap].end0 = i
			}
			p.chapters = append(p.chapters, chapter{title: strings.TrimSpace(title), heading0: i, start0: i + 1, end0: len(p.lines)})
			curChap = len(p.chapters) - 1
		case level == 3:
			curSec = -1
			if curBlock >= 0 {
				p.blocks[curBlock].end0 = i
			}
			p.blocks = append(p.blocks, h3Block{chapIdx: curChap, title: strings.TrimSpace(title), heading0: i, start0: i + 1, end0: blockEndDefault(p, curChap, i)})
			curBlock = len(p.blocks) - 1
			if curChap >= 0 {
				p.chapters[curChap].h3s = append(p.chapters[curChap].h3s, curBlock)
			}
		case level == 4:
			if curBlock >= 0 {
				if curSec >= 0 {
					p.blocks[curBlock].sections[curSec].end0 = i
				}
				p.blocks[curBlock].sections = append(p.blocks[curBlock].sections,
					h4Section{title: strings.TrimSpace(title), heading0: i, start0: i + 1, end0: p.blocks[curBlock].end0})
				curSec = len(p.blocks[curBlock].sections) - 1
			}
			// 章节直挂的 ####（如「## 十三、错误响应」下的「#### 错误响应示例」）不进接口块，忽略
		}
	}
	// H4 的 end0 先按块尾兜底，这里再收紧到下一个 H4/块尾（append 时已处理前一个）
	for bi := range p.blocks {
		blk := &p.blocks[bi]
		for si := range blk.sections {
			if si+1 < len(blk.sections) {
				blk.sections[si].end0 = blk.sections[si+1].heading0
			} else {
				blk.sections[si].end0 = blk.end0
			}
		}
	}
}

func blockEndDefault(p *parser, curChap, i int) int {
	if curChap >= 0 {
		return p.chapters[curChap].end0
	}
	return len(p.lines)
}

// headingLevel 返回标题级别与文本（非标题返回 0）。只认 1–4 级；# 后必须有空格。
func headingLevel(line string) (int, string) {
	t := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 4 || n >= len(t) || (t[n] != ' ' && t[n] != '\t') {
		return 0, ""
	}
	return n, strings.TrimSpace(t[n+1:])
}

// ===================== meta 块 =====================

func (p *parser) parseMetaBlock() {
	for i := range p.fences {
		if p.fences[i].info != "apidoc-meta" {
			continue
		}
		if p.metaFound { // 第二个 meta 块：不认，报 W
			p.issue(LevelWarning, WarnUnparsedBlock, p.fences[i].start0+1, "```apidoc-meta", "一份文档只有一个 meta 块", "出现多个 apidoc-meta 围栏块，只认第一个")
			continue
		}
		p.metaFound = true
		p.metaFence = &p.fences[i]
		p.doc.Meta.Present = true
		p.doc.Meta.Line = p.fences[i].start0 + 1
		for off, line := range strings.Split(p.fences[i].content, "\n") {
			t := strings.TrimSpace(line)
			if t == "" {
				continue
			}
			lineNo := p.fences[i].start0 + 1 + off + 1
			key, val, ok := cutMetaLine(t)
			if !ok {
				p.issue(LevelError, ErrMetaUnknownKey, lineNo, "meta 行必须是单层 key: value："+t, "key: value（全角或半角冒号均可，只允许一层）", "")
				continue
			}
			key = strings.TrimSpace(key)
			val = strings.TrimSpace(val)
			if !metaKeys[key] {
				p.issueAt(LevelError, ErrMetaUnknownKey, lineNo, t, "meta 只允许 spec/project/service/base_path/scope/change_type/change_title/change_note/version_tag", "未知键 "+key)
				continue
			}
			if _, dup := p.metaKeyLine[key]; dup {
				p.issueAt(LevelWarning, WarnUnparsedBlock, lineNo, t, "meta 键不应重复", "键 "+key+" 重复，取最后一次赋值")
			}
			p.metaKeyLine[key] = lineNo
			switch key {
			case "spec":
				p.doc.Meta.Spec = val
			case "project":
				p.doc.Meta.Project = val
			case "service":
				p.doc.Meta.Service = val
			case "base_path":
				p.doc.Meta.BasePath = val
			case "scope":
				p.doc.Meta.Scope = val
			case "change_type":
				p.doc.Meta.ChangeType = val
			case "change_title":
				p.doc.Meta.ChangeTitle = val
			case "change_note":
				p.doc.Meta.ChangeNote = val
			case "version_tag":
				p.doc.Meta.VersionTag = val
			}
		}
	}
}

// resolveMode 生效模式：meta.scope（合法时）> opts.Mode > incremental。
func (p *parser) resolveMode(opts Options) {
	mode := opts.Mode
	if mode == "" {
		mode = ModeIncremental
	}
	if sc := p.doc.Meta.Scope; sc == ModeIncremental || sc == ModeInit || sc == ModeFull {
		mode = sc
	}
	p.mode = mode
	p.doc.Mode = mode
	p.strict = mode != ModeInit
	if mode == ModeFull {
		// D17：full 解析可识别，但导入未实现（表已建好，逻辑后补）
		p.issueAt(LevelError, ErrScopeFullUnsupported, p.metaLine("scope"), "",
			"scope: incremental 或 init",
			"scope: full（全量重出 + 完整性证书）推到第二阶段，MVP 只做 incremental + 显式删除")
	}
}

func (p *parser) metaLine(key string) int {
	if l, ok := p.metaKeyLine[key]; ok {
		return l
	}
	if p.metaFence != nil {
		return p.metaFence.start0 + 1
	}
	return 1
}

// cutMetaLine meta 行手写 split（不引入 YAML 依赖）：按最先出现的冒号切（全角/半角均可，
// 规范骨架示例用半角「key: value」，散文文档常见全角）。
func cutMetaLine(t string) (key, val string, ok bool) {
	fi := strings.Index(t, "：")
	hi := strings.Index(t, ":")
	switch {
	case fi < 0 && hi < 0:
		return "", "", false
	case fi >= 0 && (hi < 0 || fi <= hi):
		return strings.TrimSpace(t[:fi]), strings.TrimSpace(t[fi+len("："):]), true
	default:
		return strings.TrimSpace(t[:hi]), strings.TrimSpace(t[hi+1:]), true
	}
}

func (p *parser) validateMeta() {
	m := &p.doc.Meta
	if !m.Present {
		if p.strict {
			p.issueAt(LevelError, ErrSpecBlockMissing, 1, p.snippet1(1),
				"文档开头必须有 ```apidoc-meta 围栏块", "meta 块声明 project/scope/变更说明，是导入的唯一入口（init 模式可缺省）")
		}
		return
	}
	if m.Spec != "" && m.Spec != "1" {
		p.issueAt(LevelError, ErrSpecVersion, p.metaLine("spec"), "spec: "+m.Spec, "spec: 1", "规范版本不支持")
	}
	if !p.strict {
		return // init 模式放宽：meta 存在也不强制必填/枚举
	}
	if m.Spec == "" {
		p.issueAt(LevelError, ErrMetaFieldMissing, p.metaFence.start0+1, "spec", "spec: 1", "meta 缺必填字段 spec")
	}
	if m.Project == "" {
		p.issueAt(LevelError, ErrMetaFieldMissing, p.metaFence.start0+1, "project", "project: <服务标识>", "meta 缺必填字段 project（一个后端服务/仓库）")
	}
	if m.Scope == "" {
		p.issueAt(LevelError, ErrMetaFieldMissing, p.metaFence.start0+1, "scope", "scope: incremental | init", "meta 缺必填字段 scope")
	} else if m.Scope != ModeIncremental && m.Scope != ModeInit && m.Scope != ModeFull {
		p.issueAt(LevelError, ErrMetaEnum, p.metaLine("scope"), "scope: "+m.Scope, "scope ∈ incremental / init / full", "")
	}
	if m.Scope == ModeIncremental || m.Scope == ModeFull {
		for _, f := range []struct{ key, val, want string }{
			{"change_type", m.ChangeType, "feature | fix | refactor | docs"},
			{"change_title", m.ChangeTitle, "一行事件标题"},
			{"change_note", m.ChangeNote, "一行「为什么改」（本功能的核心价值，必填）"},
		} {
			if f.val == "" {
				p.issueAt(LevelError, ErrMetaFieldMissing, p.metaFence.start0+1, f.key, f.key+": "+f.want, "scope≠init 时必填")
			}
		}
		if m.ChangeType != "" {
			switch m.ChangeType {
			case "feature", "fix", "refactor", "docs":
			default:
				p.issueAt(LevelError, ErrMetaEnum, p.metaLine("change_type"), "change_type: "+m.ChangeType, "change_type ∈ feature / fix / refactor / docs", "")
			}
		}
	}
}

// ===================== 趟 3：bullet 元信息 =====================

func (p *parser) passBullets() {
	p.states = make([]blockState, len(p.blocks))
	for bi := range p.blocks {
		blk := &p.blocks[bi]
		st := &p.states[bi]
		metaEnd := blk.end0
		if len(blk.sections) > 0 {
			metaEnd = blk.sections[0].heading0
		}
		// 说明散文 = 标题行到第一个 bullet/表格/围栏/--- 之间的非空行
		var desc []string
		// URL 行之前出现的其他 bullet 先暂存，URL 合法后补应用
		var pendingKV []struct {
			i            int
			label, value string
		}
		bulletAreaDone := false
		var ep *Endpoint
		for i := blk.start0; i < metaEnd; i++ {
			if p.inFence[i] {
				desc = nil // 说明区出现围栏，保守丢弃已收集的（不合常规，不猜）
				continue
			}
			line := p.lines[i]
			t := strings.TrimSpace(line)
			if t == "" || t == "---" {
				continue
			}
			if strings.HasPrefix(t, "|") {
				break
			}
			// 先严格匹配（全角冒号 + 冒号在 ** 之外），失败才做方言近似探测——顺序不能反，
			// 否则合法的 `- **URL**：` 也会命中近似正则被误报
			if m := reBulletStrict.FindStringSubmatch(line); m != nil {
				bulletAreaDone = true
				label := strings.TrimSpace(m[1])
				value := strings.TrimSpace(m[2])
				if label == "URL" {
					st.urlish = true
					ep = p.applyURLBullet(blk, i, value, ep)
					continue
				}
				if ep == nil {
					pendingKV = append(pendingKV, struct {
						i            int
						label, value string
					}{i, label, value})
					continue
				}
				p.applyMetaBullet(ep, i, label, value)
				continue
			}
			if reBulletNearMiss.MatchString(line) {
				bulletAreaDone = true
				if strings.Contains(line, "**URL") {
					st.urlish = true
				}
				p.parseBulletNearMiss(blk, i, line)
				continue
			}
			if !bulletAreaDone {
				desc = append(desc, t)
			}
			// bullet 区之后的散文行不进说明（如变更声明后的补充文字），忽略
		}
		for _, pb := range pendingKV {
			if ep != nil {
				p.applyMetaBullet(ep, pb.i, pb.label, pb.value)
			}
		}
		if ep != nil {
			ep.Description = UnescapeText(strings.Join(desc, "\n"))
			ep.RawMarkdown = strings.Join(p.lines[blk.heading0:blk.end0], "\n")
			ep.EndLine = blk.end0 // 1-based 独占端点 = 最后内容行 +1，语义上即块尾
			st.ep = ep
			p.doc.Endpoints = append(p.doc.Endpoints, ep)
			p.validateDeclarations(ep, blk)
		} else {
			raw := strings.Join(p.lines[blk.heading0:blk.end0], "\n")
			p.doc.Unparsed = append(p.doc.Unparsed, UnparsedBlock{Line: blk.heading0 + 1, Heading: blk.title, RawMarkdown: raw})
			if !st.urlish {
				// 没有任何 URL 形态的 bullet：规范模式下 ### 必须是接口块 → E；init 模式只提示
				want := "接口块必须含 - **URL**：" + q + "METHOD /path" + q + "（全角冒号 + backtick 包裹）"
				if p.strict {
					p.issueAt(LevelError, ErrURLFormat, blk.heading0+1, "### "+blk.title,
						want, "该 ### 块没有可识别的 URL 行，无法入库")
				} else {
					p.issueAt(LevelWarning, WarnUnparsedBlock, blk.heading0+1, "### "+blk.title,
						want, "该 ### 块没有 URL 行，按未解析块保留原文")
				}
			}
		}
	}
}

// parseBulletNearMiss 处理方言 bullet（**URL:** 半角冒号 / 冒号在粗体内等）：
// 报 Issue 但不追方言（字面约定窄是已知风险，靠 parse_state + Issue + 预览关卡兜底）。
func (p *parser) parseBulletNearMiss(blk *h3Block, i int, line string) {
	m := reBulletNearMiss.FindStringSubmatch(line)
	if m == nil {
		return
	}
	label := m[1]
	if label == "URL" {
		p.issueAt(LevelError, ErrURLFormat, i+1, strings.TrimSpace(line),
			"- **URL**：`METHOD /path`（全角冒号，冒号在 ** 之外，路径用 backtick 包裹）",
			"URL 行是唯一硬规则；半角冒号或 **URL:** 写法不被识别，该接口块不会入库")
		return
	}
	p.issueAt(LevelWarning, WarnUnparsedBlock, i+1, strings.TrimSpace(line),
		"- **"+label+"**：值（全角冒号，冒号在 ** 之外）",
		"元信息行 "+label+" 写法不合规范，该行内容被忽略（槽位记入 Unresolved）")
}

// applyURLBullet 解析 - **URL**：`METHOD /path`；合格才建 Endpoint。
func (p *parser) applyURLBullet(blk *h3Block, i int, value string, existing *Endpoint) *Endpoint {
	v := strings.Trim(strings.TrimSpace(value), "`")
	v = strings.TrimSpace(v)
	parts := strings.Fields(v)
	if len(parts) != 2 {
		p.issueAt(LevelError, ErrURLFormat, i+1, value, "- **URL**：`METHOD /path`（方法与路径各一段）", "URL 值必须形如 POST /api/v1.0/user/subscription")
		return existing
	}
	method, path := parts[0], parts[1]
	bad := false
	if !ValidMethod(method) {
		want := "GET / POST / PUT / DELETE / PATCH（大写）"
		hint := "方法必须是合法大写 HTTP 方法"
		if ValidMethod(strings.ToUpper(method)) {
			hint = "方法必须大写（如 " + strings.ToUpper(method) + "）"
		}
		p.issueAt(LevelError, ErrMethodInvalid, i+1, value, want, hint)
		bad = true
	}
	if !strings.HasPrefix(path, "/") {
		p.issueAt(LevelError, ErrURLFormat, i+1, value, "路径以 / 开头", "路径必须以 / 开头")
		bad = true
	}
	if bad {
		return existing
	}
	if existing != nil {
		p.issueAt(LevelError, ErrURLFormat, i+1, value, "一个 ### 块只允许一行 **URL**", "出现多行 URL bullet，只认第一行")
		return existing
	}
	ep := &Endpoint{Name: blk.title, Line: blk.heading0 + 1, DomainIdx: -1, BreakingDecl: -1,
		Method: method, PathRaw: path, IdentityKey: IdentityKey(method, path),
		ReqParamsState: SectionAbsent, PathParamsState: SectionAbsent, RespParamsState: SectionAbsent}
	if m := reH3Seq.FindStringSubmatch(blk.title); m != nil {
		ep.Seq = m[1]
		ep.Name = strings.TrimSpace(m[2])
	}
	return ep
}

func (p *parser) applyMetaBullet(ep *Endpoint, i int, label, value string) {
	switch label {
	case "Content-Type":
		ep.ContentType = strings.Trim(strings.TrimSpace(value), "`")
	case "认证":
		ep.Auth = strings.TrimSpace(value)
	case "Header":
		ep.Header = strings.TrimSpace(value)
	case "变更":
		ep.Change = strings.TrimSpace(value)
	case "变更点":
		ep.ChangePoint = strings.TrimSpace(value)
	case "breaking":
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			ep.BreakingDecl = 1
		case "false":
			ep.BreakingDecl = 0
		default:
			if p.strict {
				p.issueAt(LevelError, ErrChangeEnum, i+1, value, "breaking: true | false", "breaking 只接受 true/false")
			}
		}
	default:
		p.issueAt(LevelWarning, WarnUnparsedBlock, i+1, "- **"+label+"**："+value,
			"规范定义的 bullet：URL / Content-Type / 认证 / Header / 变更 / 变更点 / breaking",
			"未知元信息行被忽略")
	}
}

// validateDeclarations 变更声明校验（规范模式）。
func (p *parser) validateDeclarations(ep *Endpoint, blk *h3Block) {
	if !p.strict {
		return // init 模式允许缺 变更/变更点
	}
	switch ep.Change {
	case "":
		p.issueAt(LevelError, ErrChangeMissing, ep.Line, "### "+blk.title, "- **变更**：新增|修改|删除|未变更", "规范模式每个接口必须显式声明变更")
	case ChangeAdded, ChangeUpdated, ChangeRemoved, ChangeUnchanged:
	default:
		p.issueAt(LevelError, ErrChangeEnum, ep.Line, "- **变更**："+ep.Change, "变更 ∈ 新增 / 修改 / 删除 / 未变更", "")
	}
	if (ep.Change == ChangeUpdated || ep.Change == ChangeRemoved) && ep.ChangePoint == "" {
		p.issueAt(LevelError, ErrChangePointMissing, ep.Line, "### "+blk.title,
			"- **变更点**：<一行说明>", "变更=修改/删除 时必填（存进明细 declared_note，原文不改写）")
	}
}

// ===================== 趟 2b：章节分类（领域结构规则判定）=====================

// buildDomains 领域判定用结构规则（该 ## 下存在带 **URL** bullet 的 ###），不用标题黑名单。
// 必须在 bullet 趟之后跑（结构规则依赖 URL bullet 探测结果）。
func (p *parser) buildDomains() {
	for ci := range p.chapters {
		chap := &p.chapters[ci]
		urlish := false
		valid := 0
		for _, bi := range chap.h3s {
			if p.states[bi].urlish {
				urlish = true
			}
			if p.states[bi].ep != nil {
				valid++
			}
		}
		if !urlish {
			continue // 非接口章节（概述/错误响应/接口汇总/PaperVO 结构说明…）自动排除
		}
		name, internal := NormalizeDomainName(chap.title)
		dom := Domain{Name: name, RawTitle: chap.title, Internal: internal,
			Sort: len(p.doc.Domains), Line: chap.heading0 + 1, NotesRaw: p.chapterPreamble(chap)}
		p.doc.Domains = append(p.doc.Domains, dom)
		di := len(p.doc.Domains) - 1
		for _, bi := range chap.h3s {
			if ep := p.states[bi].ep; ep != nil {
				ep.DomainIdx = di
				p.doc.Domains[di].EndpointIdx = append(p.doc.Domains[di].EndpointIdx, indexOfEndpoint(p.doc.Endpoints, ep))
			}
		}
		if valid == 0 {
			p.issueAt(LevelWarning, WarnEmptyDomain, chap.heading0+1, "## "+chap.title,
				"领域章节下至少一个可入库的接口块", "章节含 URL 形态的 ### 但没有一条能解析入库，预览需人工确认")
		}
		// 章级 preamble 下渗给子接口（认证方式/调用方）
		p.inheritPreamble(di)
	}
	// 不在任何 ## 之下的接口块 → E-ENDPOINT-NO-DOMAIN
	for _, ep := range p.doc.Endpoints {
		if ep.DomainIdx < 0 {
			p.issueAt(LevelError, ErrEndpointNoDomain, ep.Line, "### "+ep.Name,
				"接口块必须位于某个 ## 领域章节之下", "## 标题 = 领域；孤立的 ### 无法归属")
		}
	}
	// base_path 归一（§2/§13.17）：meta.base_path 优先（规范的结构化声明位），
	// 概述散文「**基础路径**：`/x/y`」行只是兜底（存量散文文档多无 meta 块）。
	// 两处都写且不一致时以 meta 为准并报 W——base_path 只做断言、绝不剥离，
	// 断言不能因为写法不同而被静默跳过（meta-only 文档同样必须触发
	// W-BASEPATH-ASSERT-FAIL）。冲突警告复用 W-UNPARSED-BLOCK（§9 W 码表封闭，
	// 不新增码，同「meta 键重复」的处理先例）。
	prose := p.scanBasePathLine()
	switch {
	case p.doc.Meta.BasePath != "":
		p.doc.BasePath = p.doc.Meta.BasePath
		if prose != "" && prose != p.doc.Meta.BasePath {
			p.issue(LevelWarning, WarnUnparsedBlock, p.metaLine("base_path"),
				"base_path: "+p.doc.Meta.BasePath,
				"概述「**基础路径**」应与 meta.base_path 一致",
				"两处 base_path 声明冲突（散文行为 "+prose+"），断言以 meta.base_path 为准")
		}
	case prose != "":
		p.doc.BasePath = prose
	}
}

func indexOfEndpoint(list []*Endpoint, ep *Endpoint) int {
	for i, e := range list {
		if e == ep {
			return i
		}
	}
	return -1
}

// chapterPreamble 章级 preamble = H2 标题行到第一个 ### 之间的原文（含围栏，剔除首尾空行与 ---）。
func (p *parser) chapterPreamble(chap *chapter) string {
	start0, end := p.chapterPreambleRange(chap)
	var out []string
	for i := start0; i < end; i++ {
		t := strings.TrimSpace(p.lines[i])
		if t == "" || t == "---" {
			if len(out) == 0 {
				continue
			}
			out = append(out, p.lines[i])
			continue
		}
		out = append(out, p.lines[i])
	}
	// 去掉尾部空行
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// inheritPreamble 章级认证说明下渗：preamble 里明确写了认证方式而接口块没写 **认证** 时继承。
// 只认三个规范枚举关键词（确定性），不做散文理解。
func (p *parser) inheritPreamble(di int) {
	dom := &p.doc.Domains[di]
	auth := ""
	switch {
	case strings.Contains(dom.NotesRaw, AuthInternal), strings.Contains(dom.NotesRaw, "X-Internal-Token"):
		auth = AuthInternal
	case strings.Contains(dom.NotesRaw, AuthNoLogin):
		auth = AuthNoLogin
	case strings.Contains(dom.NotesRaw, AuthLogin):
		auth = AuthLogin
	}
	if auth == "" {
		return
	}
	for _, ei := range dom.EndpointIdx {
		ep := p.doc.Endpoints[ei]
		if ep.Auth == "" {
			ep.Auth = auth
			ep.AuthInherited = true
		}
	}
}

// scanBasePath 兜底扫描「**基础路径**」行（围栏外第一处）。
func (p *parser) scanBasePathLine() string {
	for i, line := range p.lines {
		if p.inFence[i] {
			continue
		}
		if m := reBasePath.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return strings.Trim(strings.TrimSpace(m[1]), "`")
		}
	}
	return ""
}

// NormalizeDomainName 领域名归一（顺序不可颠倒）：
//  1. 剥中文/阿拉伯序号前缀（一、 十二、 1.）；
//  2. 剥尾部括号限定语并转 flag：（内部）→ internal=true —— 必须先走这步，否则下一步剥不掉尾部「接口」；
//  3. 仅剥尾部的「接口」二字（剥后为空则保留原文）；
//  4. 空白折叠。
//
// 例：「七、RPC 论文写入接口（内部）」→「RPC 论文写入」+ internal；「一、Token状态接口」→「Token状态」。
func NormalizeDomainName(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(reNumPrefix.ReplaceAllString(s, ""))
	internal := false
	for {
		m := reTailParen.FindStringSubmatch(s)
		if m == nil {
			break
		}
		if strings.Contains(m[1], "内部") {
			internal = true
		}
		s = strings.TrimSpace(s[:len(s)-len(m[0])])
	}
	if t := strings.TrimSpace(strings.TrimSuffix(s, "接口")); t != "" {
		s = t
	}
	s = strings.Join(strings.Fields(s), " ")
	return s, internal
}

// ===================== 趟 4：表格收集（表头驱动）=====================

func (p *parser) passTables() {
	for i := 0; i < len(p.lines); i++ {
		if p.inFence[i] || !isTableRow(p.lines[i]) {
			continue
		}
		if i+1 >= len(p.lines) || p.inFence[i+1] || !isTableSep(p.lines[i+1]) {
			continue // 不是表头（下一行不是分隔行）
		}
		t := &mdTable{header0: i, bold0: -1}
		t.cols = splitTableRow(p.lines[i])
		t.summary = isSummaryHeader(t.cols)
		j := i + 2
		for ; j < len(p.lines) && !p.inFence[j] && isTableRow(p.lines[j]); j++ {
			t.rows = append(t.rows, mdRow{line0: j, cells: splitTableRow(p.lines[j])})
		}
		t.end0 = j - 1
		// 前置粗体标题（**Xxx 结构**：）
		for k := i - 1; k >= 0; k-- {
			if strings.TrimSpace(p.lines[k]) == "" {
				continue
			}
			if !p.inFence[k] {
				if m := reBoldTitle.FindStringSubmatch(strings.TrimSpace(p.lines[k])); m != nil {
					t.boldTitle = strings.TrimSpace(m[1])
					t.bold0 = k
				}
			}
			break
		}
		p.tables = append(p.tables, t)
		i = j - 1
	}
}

func isTableRow(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "|") && len(t) > 1
}

func isTableSep(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") || !strings.Contains(t, "-") {
		return false
	}
	for _, r := range t {
		if r != '|' && r != '-' && r != ':' && r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}

func splitTableRow(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	out := make([]string, len(parts))
	for i, s := range parts {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

func isSummaryHeader(cols []string) bool {
	hasMethod, hasURL := false, false
	for _, c := range cols {
		switch c {
		case "方法":
			hasMethod = true
		case "URL":
			hasURL = true
		}
	}
	return hasMethod && hasURL
}

// contains0 半开区间归属判定：pos ∈ [start0, end0)。全部区间 helper 的唯一比较逻辑。
func contains0(start0, end0, pos0 int) bool { return pos0 >= start0 && pos0 < end0 }

// tablesIn 返回表头行落在 [start0,end0) 内的表（文档序）。归属判定只看锚点行（表头行 / 开栏行）。
func (p *parser) tablesIn(start0, end0 int) []*mdTable {
	var out []*mdTable
	for _, t := range p.tables {
		if contains0(start0, end0, t.header0) {
			out = append(out, t)
		}
	}
	return out
}

// fencesIn 返回开栏行落在 [start0,end0) 内的围栏块（文档序）。
func (p *parser) fencesIn(start0, end0 int) []fenceBlock {
	var out []fenceBlock
	for _, f := range p.fences {
		if contains0(start0, end0, f.start0) {
			out = append(out, f)
		}
	}
	return out
}

// sectionAt 返回 pos 所属的 #### 小节（nil = 不在 blk 的任何小节内）。
func sectionAt(blk *h3Block, pos0 int) *h4Section {
	for i := range blk.sections {
		if contains0(blk.sections[i].start0, blk.sections[i].end0, pos0) {
			return &blk.sections[i]
		}
	}
	return nil
}

// inKnownSection pos 是否落在 blk 的某个已知 kind（7 种之一）的小节内。
func inKnownSection(blk *h3Block, pos0 int) bool {
	sec := sectionAt(blk, pos0)
	return sec != nil && sectionKind(sec.title) != ""
}

// posInAnyBlock pos 是否落在任意 ### 块范围内。
func (p *parser) posInAnyBlock(pos0 int) bool {
	for bi := range p.blocks {
		if contains0(p.blocks[bi].start0, p.blocks[bi].end0, pos0) {
			return true
		}
	}
	return false
}

// chapterPreambleRange 章级 preamble 区间：H2 标题行之后到第一个 ### 之前（无 ### 则到章尾）。
func (p *parser) chapterPreambleRange(chap *chapter) (int, int) {
	end := chap.end0
	if len(chap.h3s) > 0 {
		end = p.blocks[chap.h3s[0]].heading0
	}
	return chap.start0, end
}

// posInChapterPreamble pos 是否落在任意章节的 preamble 区间内。
func (p *parser) posInChapterPreamble(pos0 int) bool {
	for ci := range p.chapters {
		start0, end0 := p.chapterPreambleRange(&p.chapters[ci])
		if contains0(start0, end0, pos0) {
			return true
		}
	}
	return false
}

// ===================== 趟 5：#### 分桶 + 内容路由 =====================

// sectionKind #### 小节名前缀匹配归一到 7 种 kind（存在括号变体：请求参数（ArxivPaperDTO））。
func sectionKind(title string) string {
	switch {
	case strings.HasPrefix(title, "请求参数"):
		return KindRequestParams
	case strings.HasPrefix(title, "路径参数"):
		return KindPathParams
	case strings.HasPrefix(title, "返回参数"):
		return KindResponseParams
	case strings.HasPrefix(title, "请求示例"):
		return KindRequestExample
	case strings.HasPrefix(title, "返回示例"):
		return KindResponseExample
	case strings.HasPrefix(title, "错误响应"):
		return KindErrorExample // 「错误响应示例」与「错误响应」同桶
	case strings.HasPrefix(title, "测试 curl 命令"), strings.HasPrefix(title, "测试curl命令"):
		return KindCURL
	}
	return ""
}

func (p *parser) routeSections() {
	consumed := make(map[int]bool) // fence.start0 已被路由
	if p.metaFence != nil {
		consumed[p.metaFence.start0] = true
	}
	for bi := range p.blocks {
		blk := &p.blocks[bi]
		st := &p.states[bi]
		// 章节直挂围栏（块外）：领域 preamble 已在 NotesRaw 里保真；块范围内的孤儿围栏下面处理
		if st.ep == nil {
			continue // 未解析块不做路由（已报 Issue，原文已切片保存）
		}
		ep := st.ep
		for si := range blk.sections {
			sec := &blk.sections[si]
			sec.kind = sectionKind(sec.title)
			if sec.kind == "" {
				p.issueAt(LevelWarning, WarnUnknownSection, sec.heading0+1, "#### "+sec.title,
					"请求参数 / 路径参数 / 返回参数 / 请求示例 / 返回示例 / 错误响应示例 / 测试 curl 命令", "未知小节按原文忽略（不产生字段）")
				continue
			}
			tables := p.tablesIn(sec.start0, sec.end0)
			fences := p.fencesIn(sec.start0, sec.end0)
			for _, f := range fences {
				consumed[f.start0] = true
			}
			switch sec.kind {
			case KindRequestParams:
				st.req = p.routeParamSection(ep, sec, tables, KindRequestParams)
			case KindPathParams:
				st.path = p.routeParamSection(ep, sec, tables, KindPathParams)
			case KindResponseParams:
				st.resp = p.routeParamSection(ep, sec, tables, KindResponseParams)
			case KindRequestExample, KindResponseExample, KindErrorExample, KindCURL:
				// 示例小节没有围栏块时内容按原文忽略（可能是散文说明），不产生 Example
				for _, f := range fences {
					p.routeExample(ep, sec.kind, f)
				}
				for _, t := range tables {
					p.issueAt(LevelWarning, WarnUnparsedBlock, t.header0+1, "示例小节里的表格", "示例小节只收代码块", "该表格被忽略")
				}
			}
		}
		// 块内、小节外的表（粗体标题表 → 命名结构，交给趟 7 buildSchemas；否则报 W）与孤儿围栏
		for _, t := range p.tablesIn(blk.start0, blk.end0) {
			if t.boldTitle != "" || inKnownSection(blk, t.header0) {
				continue
			}
			p.issueAt(LevelWarning, WarnUnparsedBlock, t.header0+1, strings.Join(t.cols, "|"),
				"参数表必须位于 #### 请求参数/路径参数/返回参数 小节内", "该表格未路由到任何小节，被忽略")
		}
		for _, f := range p.fencesIn(blk.start0, blk.end0) {
			if !consumed[f.start0] && sectionAt(blk, f.start0) == nil {
				consumed[f.start0] = true
				p.issueAt(LevelWarning, WarnUnparsedBlock, f.start0+1, "```"+f.info,
					"代码块应位于示例/返回示例/curl 小节内", "接口块内小节外的孤儿代码块被忽略（原文仍在 raw_markdown 里）")
			}
		}
	}
	// 文档级孤儿围栏：不在任何章节 preamble、不在任何块、不是 meta 块
	for _, f := range p.fences {
		if consumed[f.start0] || p.posInChapterPreamble(f.start0) || p.posInAnyBlock(f.start0) {
			continue
		}
		raw := strings.Join(p.lines[f.start0:minInt(f.end0+1, len(p.lines))], "\n")
		p.doc.Unparsed = append(p.doc.Unparsed, UnparsedBlock{Line: f.start0 + 1, Heading: "（孤儿代码块）", RawMarkdown: raw})
		p.issueAt(LevelWarning, WarnUnparsedBlock, f.start0+1, "```"+f.info,
			"代码块应位于接口小节的上下文内", "孤儿代码块（无 ###/#### 归属）被保留原文，不产生任何元数据")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// routeParamSection 参数小节路由：主表 → 字段；粗体标题表 → 命名结构（buildSchemas 收集）；散文 → 状态。
func (p *parser) routeParamSection(ep *Endpoint, sec *h4Section, tables []*mdTable, kind string) sectionContent {
	sc := sectionContent{present: true, proseLine0: sec.start0}
	var prose []string
	for _, t := range tables {
		if t.boldTitle != "" {
			continue // 命名结构表：趟 7 收集
		}
		if sc.table == nil {
			sc.table = t
		} else {
			p.issueAt(LevelWarning, WarnUnparsedBlock, t.header0+1, strings.Join(t.cols, "|"),
				"一个小节只认第一张主表", "多余表格被忽略")
		}
	}
	// 散文 = 小节内非表格、非围栏、非粗体标题、非分隔线的非空行
	for i := sec.start0; i < sec.end0; i++ {
		if p.inFence[i] {
			continue
		}
		t := strings.TrimSpace(p.lines[i])
		if t == "" || t == "---" || strings.HasPrefix(t, "|") {
			continue
		}
		if reBoldTitle.MatchString(t) {
			continue
		}
		if len(prose) == 0 {
			sc.proseLine0 = i
		}
		prose = append(prose, t)
	}
	sc.prose = strings.Join(prose, "\n")
	if sc.table != nil {
		p.extractFields(ep, sc.table, kind)
	}
	return sc
}

// routeExample 示例/curl 围栏块 → Example + JSON/curl 校验。
func (p *parser) routeExample(ep *Endpoint, kind string, f fenceBlock) {
	ex := Example{Line: f.start0 + 1, Content: f.content, Sort: len(ep.Examples)}
	lang := f.info
	switch lang {
	case "json":
		ex.Language = "json"
	case "bash", "sh", "shell":
		ex.Language = "bash"
	case "":
		ex.Language = "text"
	default:
		ex.Language = lang
	}
	switch kind {
	case KindRequestExample:
		ex.Kind = ExampleRequest
		if f.info == "" && (strings.Contains(f.content, "Content-Disposition:") || strings.Contains(f.content, "multipart/form-data")) {
			ex.Kind = ExampleMultipart // 无语言标记的 multipart 原始报文
			ex.Language = "text"
		}
	case KindResponseExample:
		ex.Kind = ExampleResponse
	case KindErrorExample:
		ex.Kind = ExampleError
	case KindCURL:
		ex.Kind = ExampleCURL
		p.checkCURL(ep, f)
	}
	if ex.Kind != ExampleCURL && (f.info == "" || f.info == "json") {
		if !json.Valid([]byte(f.content)) {
			p.issueAt(LevelWarning, WarnExampleJSON, f.start0+1, firstLine(f.content),
				"示例块应为合法 JSON", "json.Unmarshal 失败（可能是 multipart 原始报文或请求行示例，属正常，预览确认即可）")
		}
	}
	ep.Examples = append(ep.Examples, ex)
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// checkCURL curl 是优秀的校验源，但绝不能当身份源（占位符被替换成实例值）：只做一致性警告。
func (p *parser) checkCURL(ep *Endpoint, f fenceBlock) {
	method := ""
	if m := reCURLMethod.FindStringSubmatch(f.content); m != nil {
		method = strings.ToUpper(m[1])
	} else if strings.Contains(f.content, "-d ") || strings.Contains(f.content, "--data") {
		method = "POST"
	} else {
		method = "GET"
	}
	urlStr := reCURLURL.FindString(f.content)
	path := ""
	if urlStr != "" {
		rest := urlStr
		if i := strings.Index(rest, "://"); i >= 0 {
			rest = rest[i+3:]
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			path = rest[i:]
		}
		if i := strings.Index(path, "?"); i >= 0 {
			path = path[:i]
		}
	}
	if urlStr == "" || method != ep.Method || !pathMatchTemplate(ep.PathRaw, path) {
		got := method + " " + path
		if urlStr == "" {
			got = "（curl 里没有找到 URL）"
		}
		p.issueAt(LevelWarning, WarnCURLMismatch, f.start0+1, got,
			ep.Method+" "+ep.PathRaw, "curl 的 method/path 与 **URL** 行不一致（占位符按段匹配）")
	}
}

// pathMatchTemplate 模板路径与实例路径按段匹配：{} 段匹配任意单段（大小写敏感）。
func pathMatchTemplate(tmpl, concrete string) bool {
	a := strings.Split(strings.Trim(NormPath(tmpl), "/"), "/")
	b := strings.Split(strings.Trim(concrete, "/"), "/")
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == "{}" {
			continue
		}
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ===================== 字段提取（表头驱动，绝不按列序）=====================

// 参数表列名字典（出现字典外的列 → E-PARAM-COL-UNKNOWN，防漂移）。
var paramColRoles = map[string]string{
	"参数名": "name", "字段": "name", "类型": "type", "必填": "required",
	"位置": "position", "默认值": "default", "说明": "desc",
}

// rowCell 表头驱动的单元格访问器（extractFields / buildSchemaNamed / checkSummary 共用的唯一切分实现）：
// 按角色取值，绝不按列序；下标越界安全返回空串。
type rowCell struct {
	role  map[string]int
	cells []string
}

func (rc rowCell) get(r string) string {
	if i, ok := rc.role[r]; ok && i < len(rc.cells) {
		return strings.TrimSpace(rc.cells[i])
	}
	return ""
}

// tableFieldRoles 表头驱动的列角色映射（extractFields / buildSchemaNamed 共用，消除重复的列名归一逻辑）：
// 列名 → 角色 → 列下标；字典外的列报 E-PARAM-COL-UNKNOWN（防漂移）；同角色多列取第一列。
func (p *parser) tableFieldRoles(t *mdTable) map[string]int {
	role := map[string]int{}
	for i, c := range t.cols {
		r, ok := paramColRoles[c]
		if !ok {
			p.issueAt(LevelError, ErrParamColUnknown, t.header0+1, strings.Join(t.cols, " | "),
				"允许列：参数名 / 字段 / 类型 / 必填 / 位置 / 默认值 / 说明", "出现字典外的列 "+c)
			continue
		}
		if _, dup := role[r]; !dup {
			role[r] = i
		}
	}
	return role
}

// requireNameCol 名称列存在性校验：缺 → E-PARAM-ROW-COLS（want 文案写明缺哪列，§13.4 复用不新增码）。
func (p *parser) requireNameCol(t *mdTable, role map[string]int, want string) bool {
	if _, ok := role["name"]; ok {
		return true
	}
	p.issueAt(LevelError, ErrParamRowCols, t.header0+1, strings.Join(t.cols, " | "), want, "没有名称列，无法定位字段")
	return false
}

// iterFieldRows 数据行遍历（extractFields / buildSchemaNamed 共用，消除重复的单元格切分/校验逻辑）：
//   - 数据行列数与表头不符 → E-PARAM-ROW-COLS 跳过该行；
//   - 名称为空 → 空行/格式噪声跳过；
//   - 同表名称重复 → E-PARAM-DUP 跳过（hint 指明与第几行重复）。
func (p *parser) iterFieldRows(t *mdTable, role map[string]int, fn func(row mdRow, cell rowCell, name string)) {
	seen := map[string]int{}
	for _, row := range t.rows {
		if len(row.cells) != len(t.cols) {
			p.issueAt(LevelError, ErrParamRowCols, row.line0+1, strings.Join(row.cells, " | "),
				strings.Join(t.cols, " | "), "数据行列数与表头不符")
			continue
		}
		cell := rowCell{role: role, cells: row.cells}
		name := cell.get("name")
		if name == "" {
			continue
		}
		if prev, dup := seen[name]; dup {
			p.issueAt(LevelError, ErrParamDup, row.line0+1, name, "同表参数名/字段名唯一", "与第 "+strconv.Itoa(prev+1)+" 行重复")
			continue
		}
		seen[name] = row.line0
		fn(row, cell, name)
	}
}

// parseRequiredCell 必填列取值解析：是→1 / 否→0 / 空→-1（未知）；无法识别 → -1 + W（可见不静默）。
// 没有必填列是第三种状态（Required=-1 保持不变），是否读取该格由调用方按 role 判定。
func (p *parser) parseRequiredCell(v string, line0 int) int {
	switch strings.TrimSpace(v) {
	case "是":
		return RequiredYes
	case "否":
		return RequiredNo
	case "":
		return RequiredUnknown
	default:
		p.issueAt(LevelWarning, WarnUnparsedBlock, line0+1, v, "必填 ∈ 是 / 否", "无法识别的必填值按未知（-1）处理")
		return RequiredUnknown
	}
}

// extractFields 从主表提取字段（含列校验 / 位置推断 / 重复检测），按 kind 落到 ep 的三个字段桶。
func (p *parser) extractFields(ep *Endpoint, t *mdTable, kind string) {
	role := p.tableFieldRoles(t)
	isParamTable := kind == KindRequestParams || kind == KindPathParams
	if !p.requireNameCol(t, role, "表头必须含「参数名」或「字段」列") {
		return
	}
	if p.strict {
		var missing []string
		if _, ok := role["type"]; !ok {
			missing = append(missing, "类型")
		}
		if isParamTable {
			if _, ok := role["required"]; !ok {
				missing = append(missing, "必填")
			}
		}
		if len(missing) > 0 {
			p.issueAt(LevelError, ErrParamRowCols, t.header0+1, strings.Join(t.cols, " | "),
				"规范模式必须含「参数名|类型|必填|位置」（返回参数/命名结构表只要求 名称|类型）",
				"缺列："+strings.Join(missing, "、"))
		}
	}
	// 位置列
	_, hasPos := role["position"]
	if isParamTable && !hasPos && p.strict {
		p.issueAt(LevelError, ErrPosColMissing, t.header0+1, strings.Join(t.cols, " | "),
			"请求参数/路径参数表必须含「位置」列（Path|Query|Body|Header）",
			"规范模式缺位置列一律拒收（D14）；不展开的引用式写法同理")
	}
	var inferredNames []string
	var out []Field
	p.iterFieldRows(t, role, func(row mdRow, cell rowCell, name string) {
		f := Field{
			Name:     name,
			TypeRaw:  cell.get("type"),
			Required: RequiredUnknown,
			Default:  cell.get("default"),
			Desc:     UnescapeText(cell.get("desc")),
			Line:     row.line0 + 1,
		}
		f.TypeKey = TypeKey(f.TypeRaw)
		// 必填列：是/否；没有该列 = 未知（-1），不是「否」——返回参数表根本没有必填列是真实第三态
		if _, ok := role["required"]; ok {
			f.Required = p.parseRequiredCell(cell.get("required"), row.line0)
		}
		// 位置
		switch kind {
		case KindPathParams:
			// 标题为「路径参数」的表 → Path；有位置列且值合法时以列值为准
			f.Position = PosPath
			if hasPos {
				if v := cell.get("position"); v != "" {
					if n := p.normalizePos(v, row.line0, name); n != "" {
						f.Position = n
					}
				}
			} else {
				inferredNames = append(inferredNames, name)
			}
		case KindRequestParams:
			if hasPos {
				f.Position = p.normalizePos(cell.get("position"), row.line0, name)
				if f.Position == "" {
					f.Position = inferPos(ep.Method) // normalizePos 已按行报 E（严格）或 W（init）
				}
			} else {
				f.Position = inferPos(ep.Method)
				inferredNames = append(inferredNames, name)
			}
		default: // response_params
			if v := cell.get("position"); v != "" {
				f.Position = p.normalizePos(v, row.line0, name)
			}
		}
		out = append(out, f)
	})
	if len(inferredNames) > 0 && p.mode == ModeInit {
		reason := "缺「位置」列，已按 method 推断 " + ep.Method + " → " + inferPos(ep.Method)
		if kind == KindPathParams {
			reason = "缺「位置」列，已按表标题「路径参数」推断为 Path"
		}
		p.issueAt(LevelWarning, WarnPosInferred, t.header0+1, strings.Join(t.cols, " | "),
			"请求参数/路径参数表应带「位置」列（Path|Query|Body|Header）",
			reason+"："+strings.Join(inferredNames, "、")+"（预览需人工核对）")
	}
	p.recordTableHeader(ep, t, kind, "")
	switch kind {
	case KindPathParams:
		ep.PathFields = append(ep.PathFields, out...)
	case KindRequestParams:
		ep.RequestFields = append(ep.RequestFields, out...)
	case KindResponseParams:
		ep.ResponseFields = append(ep.ResponseFields, out...)
	}
}

// normalizePos 位置列取值校验；非法值：规范模式报 E-POS-COL-MISSING（列存在但值不可用），init 返回 ""（触发推断）。
func (p *parser) normalizePos(v string, rowLine0 int, name string) string {
	for _, canon := range []string{PosPath, PosQuery, PosBody, PosHeader} {
		if strings.EqualFold(v, canon) {
			return canon
		}
	}
	if p.strict {
		p.issueAt(LevelError, ErrPosColMissing, rowLine0+1, name+" → "+v,
			"位置 ∈ Path / Query / Body / Header", "参数 "+name+" 的位置值无法识别")
		return ""
	}
	p.issueAt(LevelWarning, WarnPosInferred, rowLine0+1, name+" → "+v,
		"位置 ∈ Path / Query / Body / Header", "参数 "+name+" 的位置值无法识别，按 method 推断")
	return ""
}

// inferPos init 模式缺「位置」列的推断规则：GET/DELETE→Query，POST/PUT/PATCH→Body（路径参数表→Path 在调用处）。
func inferPos(method string) string {
	switch method {
	case "GET", "DELETE":
		return PosQuery
	default:
		return PosBody
	}
}

func (p *parser) recordTableHeader(ep *Endpoint, t *mdTable, kind, schemaName string) {
	section := kind
	if schemaName != "" {
		section = "schema:" + schemaName
	}
	cols := append([]string(nil), t.cols...)
	ep.TableHeaders = append(ep.TableHeaders, TableHeader{Section: section, Cols: cols, Line: t.header0 + 1})
}

// ===================== 趟 6：散文归类 =====================

// classifyProse 区分「写了 无。」与「根本没有这个小节」：前者是明确无参数，后者是信息缺失（Unresolved）。
func (p *parser) classifyProse() {
	for bi := range p.blocks {
		st := &p.states[bi]
		if st.ep == nil {
			continue
		}
		ep := st.ep
		ep.ReqParamsState = p.classifyOne(ep, &st.req, "request_params")
		ep.PathParamsState = p.classifyOne(ep, &st.path, "path_params")
		ep.RespParamsState = p.classifyOne(ep, &st.resp, "response_params")
		// Unresolved 槽位（删除声明允许省略参数/响应表，§8.2）
		if ep.Change != ChangeRemoved {
			if ep.ReqParamsState != SectionTable && ep.ReqParamsState != SectionEmpty {
				ep.Unresolved = append(ep.Unresolved, "request_params")
			}
			if ep.RespParamsState != SectionTable && ep.RespParamsState != SectionEmpty {
				ep.Unresolved = append(ep.Unresolved, "response_params")
			}
			if strings.Contains(NormPath(ep.PathRaw), "{}") && ep.PathParamsState != SectionTable && ep.PathParamsState != SectionEmpty {
				ep.Unresolved = append(ep.Unresolved, "path_params")
			}
		}
	}
}

func (p *parser) classifyOne(ep *Endpoint, sc *sectionContent, slot string) SectionState {
	if !sc.present {
		return SectionAbsent
	}
	if sc.table != nil {
		return SectionTable
	}
	prose := strings.TrimSpace(sc.prose)
	if prose == "" {
		return SectionProse // 小节存在但空：信息缺失
	}
	if strings.HasPrefix(prose, "无") {
		return SectionEmpty // 明确无参数（「无。」「无请求体。」「无请求体参数，需在请求头中携带认证Token。」）
	}
	if strings.HasPrefix(prose, "同") || reSeeRef.MatchString(prose) {
		// 引用式写法：不展开则字段表为空，重复导入时「空 vs 空」永远判未变化，真实改动被静默吞掉
		if p.strict {
			p.issueAt(LevelError, ErrCrossRef, sc.proseLine0+1, prose,
				"把被引用的参数表原样展开", "规范模式一律拒收引用式写法（E-CROSS-REF）")
		} else {
			p.issueAt(LevelWarning, WarnCrossRef, sc.proseLine0+1, prose,
				"把被引用的参数表原样展开", "init 模式降级保留：条目入库、参数表留空、parse_state=partial，预览标红「需人工补全」")
		}
		return SectionCrossRef
	}
	return SectionProse
}

// ===================== 趟 7：命名结构 registry =====================

func (p *parser) buildSchemas() {
	// 1) endpoint 级：块内所有粗体标题表
	for bi := range p.blocks {
		st := &p.states[bi]
		if st.ep == nil {
			continue
		}
		blk := &p.blocks[bi]
		for _, t := range p.tablesIn(blk.start0, blk.end0) {
			if t.boldTitle == "" || t.summary {
				continue
			}
			sd := p.buildSchema(t, st.ep)
			st.ep.Schemas = append(st.ep.Schemas, sd)
			p.doc.Schemas = append(p.doc.Schemas, sd)
		}
	}
	// 2) 文档级：非领域章节里的粗体标题表；或章节标题「Xxx 结构说明」+ 章内直挂表
	domainChaps := map[int]bool{}
	for ci := range p.chapters {
		for _, bi := range p.chapters[ci].h3s {
			if p.states[bi].urlish {
				domainChaps[ci] = true
				break
			}
		}
	}
	for ci := range p.chapters {
		if domainChaps[ci] {
			continue
		}
		chap := &p.chapters[ci]
		chapTitle, _ := NormalizeDomainName(chap.title)
		structName := ""
		if strings.HasSuffix(chapTitle, "结构说明") {
			structName = strings.TrimSpace(strings.TrimSuffix(chapTitle, "结构说明"))
		}
		for _, t := range p.tablesIn(chap.start0, chap.end0) {
			if t.summary || p.posInAnyBlock(t.header0) {
				continue // 汇总表 / 未解析 ### 块内的表不当文档级命名结构
			}
			name := ""
			switch {
			case t.boldTitle != "":
				name = schemaNameFromBold(t.boldTitle)
			case structName != "":
				name = structName
			default:
				continue
			}
			sd := p.buildSchemaNamed(t, name, "", -1)
			p.doc.Schemas = append(p.doc.Schemas, sd)
		}
	}
	// 3) 同名不同构 → ambiguous=1 且拒绝按名展开（主键 (name, owner_scope)）
	byName := map[string][]int{}
	for i := range p.doc.Schemas {
		byName[p.doc.Schemas[i].Name] = append(byName[p.doc.Schemas[i].Name], i)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names) // 确定性
	for _, n := range names {
		idxs := byName[n]
		distinct := map[string]int{} // bodyHash -> 首见 index
		var order []string
		for _, i := range idxs {
			h := p.doc.Schemas[i].BodyHash
			if _, ok := distinct[h]; !ok {
				distinct[h] = i
				order = append(order, h)
			}
		}
		if len(order) < 2 {
			continue
		}
		var lines []string
		for _, i := range idxs {
			p.doc.Schemas[i].Ambiguous = true
			lines = append(lines, strconv.Itoa(p.doc.Schemas[i].Line))
		}
		p.issueAt(LevelWarning, WarnStructDiverged, p.doc.Schemas[distinct[order[1]]].Line, "**"+n+" 结构**",
			"同名命名结构必须同构（主键 (name, owner_scope)）",
			"「"+n+"」存在 "+strconv.Itoa(len(order))+" 种不同定义（行 "+strings.Join(lines, "、")+"），已全部标记 ambiguous，拒绝按名自动展开")
	}
	// 4) 同步 endpoint 内嵌副本的 ambiguous 标记
	amb := map[string]bool{}
	for i := range p.doc.Schemas {
		if p.doc.Schemas[i].Ambiguous {
			amb[p.doc.Schemas[i].Name] = true
		}
	}
	for _, ep := range p.doc.Endpoints {
		for i := range ep.Schemas {
			if amb[ep.Schemas[i].Name] {
				ep.Schemas[i].Ambiguous = true
			}
		}
	}
}

func schemaNameFromBold(title string) string {
	s := strings.TrimSpace(title)
	if t := strings.TrimSuffix(s, "分页结构"); t != s {
		return strings.TrimSpace(t)
	}
	if t := strings.TrimSuffix(s, "结构"); t != s {
		return strings.TrimSpace(t)
	}
	return s
}

func (p *parser) buildSchema(t *mdTable, ep *Endpoint) SchemaDef {
	return p.buildSchemaNamed(t, schemaNameFromBold(t.boldTitle), ep.IdentityKey, indexOfEndpoint(p.doc.Endpoints, ep))
}

// buildSchemaNamed 命名结构字段提取：只要求 名称|类型（列校验宽松于请求参数表）。
func (p *parser) buildSchemaNamed(t *mdTable, name, ownerScope string, ownerIdx int) SchemaDef {
	sd := SchemaDef{Name: name, OwnerScope: ownerScope, OwnerIdx: ownerIdx, Line: t.header0 + 1}
	start := t.header0
	if t.bold0 >= 0 {
		start = t.bold0
	}
	sd.RawMarkdown = strings.Join(p.lines[start:t.end0+1], "\n")
	role := p.tableFieldRoles(t)
	if !p.requireNameCol(t, role, "命名结构表必须含「字段」或「参数名」列") {
		return sd
	}
	p.iterFieldRows(t, role, func(row mdRow, cell rowCell, fname string) {
		f := Field{Name: fname, TypeRaw: cell.get("type"), Required: RequiredUnknown,
			Default: cell.get("default"), Desc: UnescapeText(cell.get("desc")), Line: row.line0 + 1}
		f.TypeKey = TypeKey(f.TypeRaw)
		if _, ok := role["required"]; ok {
			f.Required = p.parseRequiredCell(cell.get("required"), row.line0)
		}
		sd.Fields = append(sd.Fields, f)
	})
	sd.BodyHash = schemaBodyHash(sd.Fields)
	// 表头列集合进宿主接口 doc_hash
	if ownerIdx >= 0 && ownerIdx < len(p.doc.Endpoints) {
		p.recordTableHeader(p.doc.Endpoints[ownerIdx], t, "", name)
	}
	return sd
}

// ===================== 趟 8：汇总表 join 自检（C1）=====================

func (p *parser) checkSummary() {
	var sum *mdTable
	for _, t := range p.tables {
		if t.summary {
			sum = t
			break
		}
	}
	sc := &p.doc.Summary
	if sum == nil {
		return
	}
	sc.Present = true
	sc.Line = sum.header0 + 1
	role := map[string]int{}
	for i, c := range sum.cols {
		switch c {
		case "接口":
			role["name"] = i
		case "方法":
			role["method"] = i
		case "URL":
			role["url"] = i
		case "说明":
			role["desc"] = i
		}
	}
	summaryKeys := map[string]bool{}
	for _, row := range sum.rows {
		cell := rowCell{role: role, cells: row.cells}
		method, path := strings.ToUpper(cell.get("method")), cell.get("url")
		if method == "" || path == "" {
			continue
		}
		sr := SummaryRow{Name: cell.get("name"), Method: method, Path: path, Desc: cell.get("desc"),
			Key: IdentityKey(method, path), Line: row.line0 + 1}
		sc.Rows = append(sc.Rows, sr)
		summaryKeys[sr.Key] = true
	}
	bodyKeys := map[string]bool{}
	for _, ep := range p.doc.Endpoints {
		bodyKeys[ep.IdentityKey] = true
	}
	// 双向差集（⚠️ 只校验 (method, path)：绝不可用名称做 join key，实测 12 处名称与 H3 标题不一致）
	for _, ep := range p.doc.Endpoints {
		if !summaryKeys[ep.IdentityKey] {
			sc.BodyOnly = append(sc.BodyOnly, ep.IdentityKey)
			p.issueAt(LevelWarning, WarnSummaryDiverged, ep.Line, ep.Method+" "+ep.PathRaw,
				"正文接口应出现在「接口汇总」表中", "C1 汇总闭合失败：正文有、汇总表没有（"+ep.IdentityKey+"）")
		}
	}
	for _, r := range sc.Rows {
		if !bodyKeys[r.Key] {
			sc.SummaryOnly = append(sc.SummaryOnly, r.Key)
			p.issueAt(LevelWarning, WarnSummaryDiverged, r.Line, r.Method+" "+r.Path,
				"汇总表行应能在正文找到对应接口块", "C1 汇总闭合失败：汇总表有、正文没有（"+r.Key+"）")
		}
	}
	sc.Closed = len(sc.BodyOnly) == 0 && len(sc.SummaryOnly) == 0
}

// ===================== 趟 9：identity + 双 hash =====================

func (p *parser) finalize() {
	seen := map[string]int{}
	for _, ep := range p.doc.Endpoints {
		if prev, dup := seen[ep.IdentityKey]; dup {
			p.issueAt(LevelError, ErrEndpointDup, ep.Line, ep.Method+" "+ep.PathRaw,
				"同文档内 identity_key 唯一", "与第 "+strconv.Itoa(p.doc.Endpoints[prev].Line)+" 行的接口重复（"+ep.IdentityKey+"）")
		} else {
			seen[ep.IdentityKey] = indexOfEndpoint(p.doc.Endpoints, ep)
		}
		// base_path 只做断言、绝不剥离（D18）
		if p.doc.BasePath != "" && !strings.HasPrefix(ep.PathRaw, p.doc.BasePath) {
			p.issueAt(LevelWarning, WarnBasePathAssert, ep.Line, ep.Method+" "+ep.PathRaw,
				"路径以 base_path（"+p.doc.BasePath+"）开头", "C3 base_path 前缀断言失败（不阻断，列出受影响接口）")
		}
		if len(ep.Unresolved) > 0 {
			ep.ParseState = ParsePartial
		} else {
			ep.ParseState = ParseOK
		}
		ep.SemanticHash = SemanticHash(ep)
		ep.DocHash = DocHash(ep)
	}
}

// ===================== Issue 辅助 =====================

func (p *parser) issue(level, code string, line1 int, snippet, want, hint string) {
	p.doc.Issues = append(p.doc.Issues, Issue{Level: level, Code: code, Line: line1, Snippet: snippet, Want: want, Hint: hint})
}

func (p *parser) issueAt(level, code string, line1 int, snippet, want, hint string) {
	if snippet == "" {
		snippet = p.snippet1(line1)
	}
	p.issue(level, code, line1, snippet, want, hint)
}

func (p *parser) snippet1(line1 int) string {
	if line1 < 1 || line1 > len(p.lines) {
		return ""
	}
	return strings.TrimSpace(p.lines[line1-1])
}
