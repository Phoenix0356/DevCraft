// Package apidoc 实现「接口变更文档规范」的确定性解析器：
// 纯 Go 行扫描（零新依赖，go.mod 无任何 markdown 库），9 趟顺序不可颠倒，
// 解析结果供 appsvc 导入编排（更新接口定义 + 生成变更事件）与 chat skill 共用。
//
// 契约来源：.trellis/tasks/10-18-api-docs/spec-draft.md（规范本体）与 prd.md（D1–D18）。
// 核心红线：
//   - 解析必须确定性：同一文档解析两次结果字节一致（测试断言）；
//   - 不许猜：解析失败降级为 Issue 列表 + parse_state + Unresolved 槽位 + raw_markdown 原文切片；
//   - fence 区间必须最先跑（样例里有无语言标记的 multipart 原始报文块，
//     块内含 Content-Type:/Content-Disposition:，不排除会污染元数据）。
package apidoc

// Issue 结构化校验输出（spec-draft §9）。
// E-* 阻断整批导入（事务回滚、零写入）；W-* 只在预览提示。
// Line 必给（1-based）——这是 chat 里 AI 能把错误码翻译成人话的前提。
type Issue struct {
	Level   string `json:"level"`   // E | W
	Code    string `json:"code"`    // 见下方码表常量
	Line    int    `json:"line"`    // 1-based 行号
	Snippet string `json:"snippet"` // 出错行原文（截断到合理长度）
	Want    string `json:"want"`    // 期望的写法
	Hint    string `json:"hint"`    // 给人/AI 的修复提示
}

// 级别取值。
const (
	LevelError   = "E" // 阻断导入
	LevelWarning = "W" // 仅预览提示
)

// 阻断错误码全集（spec-draft §9 表）。
const (
	ErrSpecBlockMissing     = "E-SPEC-BLOCK-MISSING"       // 缺 ```apidoc-meta 块
	ErrSpecVersion          = "E-SPEC-VERSION"             // spec != 1
	ErrScopeFullUnsupported = "E-SCOPE-FULL-NOT-SUPPORTED" // scope: full 二期才支持（D17）
	ErrMetaFieldMissing     = "E-META-FIELD-MISSING"       // meta 必填字段缺失
	ErrMetaEnum             = "E-META-ENUM"                // meta 字段取值越界
	ErrMetaUnknownKey       = "E-META-UNKNOWN-KEY"         // meta 出现字典外的键
	ErrEndpointNoDomain     = "E-ENDPOINT-NO-DOMAIN"       // 接口块不在任何 ## 之下
	ErrEndpointDup          = "E-ENDPOINT-DUP"             // 同文档内 identity_key 重复
	ErrURLFormat            = "E-URL-FORMAT"               // URL 行不合格式
	ErrMethodInvalid        = "E-METHOD-INVALID"           // 方法不在 GET/POST/PUT/DELETE/PATCH 大写集合
	ErrChangeMissing        = "E-CHANGE-MISSING"           // 缺 **变更** 行（规范模式）
	ErrChangeEnum           = "E-CHANGE-ENUM"              // 变更/breaking 取值越界
	ErrChangePointMissing   = "E-CHANGEPOINT-MISSING"      // 修改/删除缺 **变更点**
	ErrParamColUnknown      = "E-PARAM-COL-UNKNOWN"        // 参数表出现字典外的列（防漂移）
	ErrParamRowCols         = "E-PARAM-ROW-COLS"           // 参数表行列数与表头不符 / 缺必需列
	ErrParamDup             = "E-PARAM-DUP"                // 同表参数名重复
	ErrPosColMissing        = "E-POS-COL-MISSING"          // 规范模式请求/路径参数表缺「位置」列（D14）
	ErrCrossRef             = "E-CROSS-REF"                // 引用式写法（「同新增订阅。」），规范模式一律拒收
	// —— 以下四个码由导入编排层（appsvc，对照数据库状态）产生，解析器只定义词表 ——
	ErrAddedExists     = "E-ADDED-EXISTS"      // 声明新增但库里已有
	ErrUpdatedNotFound = "E-UPDATED-NOT-FOUND" // 声明修改但库里没有
	ErrUpdatedNoDiff   = "E-UPDATED-NO-DIFF"   // 声明修改但程序算出无变化
	ErrDeleteNotFound  = "E-DELETE-NOT-FOUND"  // 声明删除但库里找不到
	ErrDiffMismatchAdd = "E-DIFF-MISMATCH-ADDED"
	ErrDiffMismatchUpd = "E-DIFF-MISMATCH-UPDATED"
	ErrDiffMismatchUnc = "E-DIFF-MISMATCH-UNCHANGED"
	ErrBreakingDenied  = "E-BREAKING-DENIED" // 文档声明 breaking: false 但启发式判破坏
)

// 警告码全集（spec-draft §9 表 + §10 的 init 降级）。
const (
	WarnBasePathAssert  = "W-BASEPATH-ASSERT-FAIL" // 路径不以 base_path 开头（只断言不剥离，D18）
	WarnExampleJSON     = "W-EXAMPLE-JSON"         // 示例块不是合法 JSON（可能是 multipart 原始报文，属正常）
	WarnCURLMismatch    = "W-CURL-MISMATCH"        // curl 里的 method/path 与 **URL** 行不一致
	WarnStructDiverged  = "W-STRUCT-DIVERGED"      // 同名命名结构不同构 → ambiguous，拒绝按名自动展开
	WarnPosInferred     = "W-POS-INFERRED"         // init 模式缺「位置」列，按 method 推断（需人工核对）
	WarnEmptyDomain     = "W-EMPTY-DOMAIN"         // 章节像领域但没有可入库的接口
	WarnUnknownSection  = "W-UNKNOWN-SECTION"      // #### 小节名不在 7 种 kind 前缀内
	WarnUnparsedBlock   = "W-UNPARSED-BLOCK"       // 无法解析的块（孤儿代码块 / 无 URL 的 ### / 方言 bullet）
	WarnSummaryDiverged = "W-SUMMARY-DIVERGED"     // C1 汇总闭合失败（正文 URL 集 vs 接口汇总表双向差集）
	WarnCrossRef        = "W-CROSS-REF"            // init 模式下 E-CROSS-REF 的降级形态
)

// HasErrors 是否存在阻断级 Issue（整批拒收的判据）。
func (d *Document) HasErrors() bool {
	for i := range d.Issues {
		if d.Issues[i].Level == LevelError {
			return true
		}
	}
	return false
}

// Errors 返回全部阻断级 Issue。
func (d *Document) Errors() []Issue {
	var out []Issue
	for _, is := range d.Issues {
		if is.Level == LevelError {
			out = append(out, is)
		}
	}
	return out
}

// Warnings 返回全部警告级 Issue。
func (d *Document) Warnings() []Issue {
	var out []Issue
	for _, is := range d.Issues {
		if is.Level == LevelWarning {
			out = append(out, is)
		}
	}
	return out
}

// HasCode 报告里是否出现过某个错误码（测试与预览分类用）。
func (d *Document) HasCode(code string) bool {
	for _, is := range d.Issues {
		if is.Code == code {
			return true
		}
	}
	return false
}
