// repair.go 是「聊天附件导入」的确定性修复器（任务 10-20-chat-file-import，PRD D2/D3）。
// 只做机械格式修复（白名单见下），语义问题一个字不改、只留在 Issue 清单里等用户处理。
// 确定性 = 同一份输入，Preview 与 Commit 先后两次调用 Repair 必然得到字节一致的修复稿
// （D5：两遍走同一个 apidoc.Repair，不存在第二份导入实现，也不存在"AI 改写版"）。
//
// 与解析器同一纪律：stdlib 零依赖；围栏内一个字节都不碰（围栏掩码先行，语义与
// passFences 完全一致：标记逐段交替，未闭合时闭到文末）。
//
// ===================== 「解析器是否已容忍」逐条核查结论（动手前先验证）=====================
// 原则：已经被 parse.go 容忍的写法不"修"——改了不产生任何收益，只会制造无意义 diff。
//   - 白名单 5（H4 小节标题括号后缀，如「#### 请求参数（ArxivPaperDTO）」）：
//     parse.go sectionKind 是前缀匹配（spec-draft §12），括号变体本来就能识别，golden fixture
//     里就有此形态且解析正常 → 不实现，一条 diff 都不产生。
//   - 白名单 6（章节序号与 --- 分隔线噪声）：passBullets / routeParamSection / chapterPreamble
//     已逐类跳过 `---` 与空行，序号前缀由 NormalizeDomainName / reH3Seq 剥离 → 不实现；
//     清理它们不改变任何解析产物，纯制造 diff。
//   - meta 块全/半角冒号：cutMetaLine 两者都切（spec-draft §13.6）→ 不在修复范围。
//   - 白名单 2（未闭合围栏）：解析器把未闭合围栏闭到文末并报 W——补一个文末闭合后
//     围栏区间与解析器已采用的区间完全相同（零语义变化），收益只在「修正稿本身是合法
//     markdown、可下载」与消除 W → 实现，且仅在全文围栏标记数为奇数时（PRD 原文）。
//   - 白名单 3（缺 位置 列）：规范模式 E-POS-COL-MISSING 阻断导入 → 必须修。注意解析器
//     对缺列的字段本来就按 inferPos 填了推断值（extractFields），所以补列写入的是同一个
//     推断值——修复前后 Field.Position 逐字节相等（性质测试可证），只有 TableHeaders 列集
//     与 doc_hash 变化（文档确实被改了，这是修复的本意）。每条都标 NeedsReview=true（D2）。
//   - 白名单 1（bullet 方言形态 `**URL:**` 等）：URL 行触发 E-URL-FORMAT（整个接口块不入库），
//     其余标签行触发 W-UNPARSED-BLOCK（值被忽略、槽位进 Unresolved）→ 必须修，收益真实。
//     只作用于 7 个已知标签（URL/变更/变更点/Content-Type/认证/Header/breaking），且带取值
//     守卫（变更∈枚举、认证∈枚举、breaking∈{true,false}、URL 形如 `METHOD /path`），
//     绝不碰正文里带冒号的中文句子；非粗体形态（`- 认证：需要登录`）与解析器的
//     reBulletNearMiss 口径一致不动（它只认 `**` 粗体方言；非粗体与散文说明无法确定性区分）。
//   - 白名单 4（交叉引用确定性展开）：规范模式 E-CROSS-REF 一律拒收 → 实现纯搬运展开。
//     只在「被引用对象按 ### N.M 编号或接口名唯一精确命中、且其对应槽位确有已路由参数表」
//     时把那张表逐字节原样搬运插入；找不到、多候选、自引用、小节归属歧义 → 一个字不改，
//     保留原 E-CROSS-REF（AC5），绝不编造字段表。
//
// ===================== 「不改」清单（PRD D2，只能出现在剩余 Issue 里）=====================
// VO 同名不同构改名、缺 **URL** 行、缺 **变更** 行（增量模式无法推断增/改）、字段增删、
// 类型值、必填值、说明文字、认证值——修复器一律不产生态度，全部原样留给校验报告。
//
// ===================== Fix.Line 坐标系约定 =====================
// 除围栏补闭合（新行在文末，行号按修复稿计）外，Fix.Line 是被改动行在「修复前文本」中的
// 1-based 行号。同轮内所有插入/替换按行号降序应用，因此更靠前的修复行号在同轮内不漂移；
// 跨轮叠加修复（bullet/位置轮之后才做交叉引用轮）时，交叉引用修复的行号相对前一轮的
// 产物坐标——对典型文档（≤3 轮收敛）即原文行号。卡片展示按此定位原文件。

package apidoc

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func itoa(n int) string { return strconv.Itoa(n) }

// Fix.Kind 取值（封闭词表——修复器只做这四类，对应上面核查后保留的白名单条目）。
const (
	FixBulletForm     = "bullet-form"      // 1. bullet 元信息行冒号/加粗形态归一
	FixFenceClose     = "fence-close"      // 2. 未闭合代码围栏文末补闭合
	FixPosColumn      = "pos-column"       // 3. 参数表补「位置」列（逐条待人工核对）
	FixCrossRefExpand = "cross-ref-expand" // 4. 交叉引用确定性展开（原样搬运参数表）
)

// Fix 一处机械修复的完整记录（卡片逐条展示：行号 + 改了什么 + 是否待核对 + 为什么）。
type Fix struct {
	Kind        string `json:"kind"`
	Line        int    `json:"line"`        // 坐标系见文件头约定（1-based）
	Before      string `json:"before"`      // 修复前该处文本（截断到合理长度）
	After       string `json:"after"`       // 修复后该处文本 / 对插入内容的概述
	NeedsReview bool   `json:"needsReview"` // 推断类修复恒 true（沿用 W-POS-INFERRED 语义）
	Note        string `json:"note"`        // 中文说明：为什么改、按什么规则改
}

// 轮数上限：每轮要么收敛要么零改动，正常文档 ≤3 轮（bullet/位置 → 交叉引用 → 复核）。
// 触顶即停（防御性），剩余问题由后续完整校验原样报出——修复器永远不是闸门的替代。
const repairMaxRounds = 8

// bullet 标签白名单（与 reBulletNearMiss 同一套；字典外标签的粗体行绝不触碰）。
var repairMetaLabels = map[string]bool{
	"URL": true, "Content-Type": true, "认证": true, "Header": true,
	"变更": true, "变更点": true, "breaking": true,
}

var (
	// 冒号在粗体内的方言：**URL:** / **URL：**
	reBulletDialectIn = regexp.MustCompile(`^-\s+\*\*([^*:：]+?)\s*[:：]\s*\*\*[ \t]*(.*)$`)
	// 冒号在粗体外但为半角：**URL**:
	reBulletDialectOut = regexp.MustCompile(`^-\s+\*\*([^*:：]+?)\*\*:[ \t]*(.*)$`)
	// 交叉引用的编号引用形如「见 7.1」「同 7.1」：带点的十进制序号 token
	reRepairSeqToken = regexp.MustCompile(`\d+(?:\.\d+)+`)
	// 交叉引用的名称引用形如「同新增订阅。」：同 + 名称，到第一个句读符为止
	reRepairSameRef = regexp.MustCompile(`^同[ \t]*([^。，；,;]+)`)
)

// Repair 对一份接口文档做确定性机械修复。
// 返回：修复后文本（零修复时原样返回输入字节，含 CRLF 也不动）、
// 修复清单（按应用轮次与行号升序）、修复后文本的默认档位解析结果（供调用方核对；
// 编排层的正式校验仍按自己的 Options 重新 Parse，修复器不越权替代闸门）。
func Repair(text string) (string, []Fix, *Document) {
	lines := splitLines(text)
	var fixes []Fix
	posInserted := map[string]bool{} // 被补过「位置」列的表头行内容（交叉引用搬运时据此传递待核对标记）
	changed := false

	for round := 0; round < repairMaxRounds; round++ {
		var edits []repairEdit
		if marker, open := unclosedFenceMarker(lines); open {
			edits = append(edits, repairEdit{
				kind: editAppend,
				new:  []string{marker},
				fix: Fix{Kind: FixFenceClose, Line: len(lines) + 1,
					Before: "（文档末尾）", After: marker, NeedsReview: false,
					Note: "全文代码围栏标记数为奇数，在文末补一个闭合标记；解析器本就把未闭合围栏闭到文末，此修复不改变任何解析产物，只让修正稿本身成为合法 markdown",
				},
			})
		}
		doc := Parse(strings.Join(lines, "\n"), Options{})
		edits = append(edits, collectBulletEdits(lines)...)
		edits = append(edits, collectPosEdits(lines, doc, posInserted)...)

		if len(edits) > 0 {
			// bullet/位置轮：都是行内替换（行数不变）或文末追加，先收敛它们，
			// 交叉引用搬运必须等源表稳定后再做（下一轮），否则搬到的可能是补列前的旧表。
			applyRepairEdits(&lines, edits)
			changed = true
			fixes = append(fixes, editsFixes(edits)...)
			continue
		}
		cross := collectCrossRefEdits(lines, doc, posInserted)
		if len(cross) == 0 {
			break
		}
		applyRepairEdits(&lines, cross)
		changed = true
		fixes = append(fixes, editsFixes(cross)...)
	}

	final := text
	if changed {
		final = strings.Join(lines, "\n")
	}
	return final, fixes, Parse(final, Options{})
}

// ===================== 编辑模型 =====================

const (
	editReplace     = "replace"     // 整行替换（行数不变）
	editInsertAfter = "insertAfter" // 在锚点行之后插入若干行
	editAppend      = "append"      // 文末追加
)

type repairEdit struct {
	kind  string
	line0 int      // 0-based 锚点行（append 忽略）
	new   []string // replace = 单行新内容；insertAfter/append = 要插入的行
	fix   Fix
}

// applyRepairEdits 按锚点行降序应用（更靠前的编辑坐标不被后面的插入漂移影响——
// Fix.Line 坐标系约定的实现处）。append 最后处理。
func applyRepairEdits(lines *[]string, edits []repairEdit) {
	sorted := append([]repairEdit(nil), edits...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].kind == editAppend {
			return false
		}
		if sorted[j].kind == editAppend {
			return true
		}
		return sorted[i].line0 > sorted[j].line0
	})
	for _, e := range sorted {
		switch e.kind {
		case editAppend:
			*lines = append(*lines, e.new...)
		case editInsertAfter:
			if e.line0+1 > len(*lines) {
				e.line0 = len(*lines) - 1
			}
			tail := append([]string(nil), (*lines)[e.line0+1:]...)
			*lines = append((*lines)[:e.line0+1], e.new...)
			*lines = append(*lines, tail...)
		case editReplace:
			if e.line0 >= 0 && e.line0 < len(*lines) && len(e.new) == 1 {
				(*lines)[e.line0] = e.new[0]
			}
		}
	}
}

// editsFixes 提取编辑清单并轮内按行号升序（卡片展示顺序稳定）。
// 分隔行/数据行/搬运行这类「并入表头修复说明」的编辑没有自己的 Fix，跳过。
func editsFixes(edits []repairEdit) []Fix {
	out := make([]Fix, 0, len(edits))
	for _, e := range edits {
		if e.fix.Kind == "" {
			continue
		}
		out = append(out, e.fix)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

func clipFixText(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// ===================== 围栏辅助（与 passFences 同一交替语义）=====================

// repairFenceMask 返回逐行的「在围栏内（含围栏标记行）」掩码。
func repairFenceMask(lines []string) []bool {
	mask := make([]bool, len(lines))
	open := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		m := ""
		switch {
		case strings.HasPrefix(t, "```"):
			m = "```"
		case strings.HasPrefix(t, "~~~"):
			m = "~~~"
		}
		if m != "" {
			mask[i] = true
			open = !open
			continue
		}
		if open {
			mask[i] = true
		}
	}
	return mask
}

// unclosedFenceMarker 全文标记数为奇数（存在未闭合围栏）时返回开栏风格（补闭合用），
// 判定与 passFences 完全一致。
func unclosedFenceMarker(lines []string) (string, bool) {
	open := false
	var marker string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		m := ""
		switch {
		case strings.HasPrefix(t, "```"):
			m = "```"
		case strings.HasPrefix(t, "~~~"):
			m = "~~~"
		}
		if m == "" {
			continue
		}
		if !open {
			open = true
			marker = m
		} else {
			open = false
		}
	}
	return marker, open
}

// ===================== 1. bullet 元信息行形态归一 =====================

func collectBulletEdits(lines []string) []repairEdit {
	var edits []repairEdit
	mask := repairFenceMask(lines)
	for i, line := range lines {
		if mask[i] {
			continue
		}
		// 已是规范形态：不动（这是「已容忍就不修」的落地——幂等的根保证）
		if reBulletStrict.MatchString(line) {
			continue
		}
		var label, value string
		if m := reBulletDialectIn.FindStringSubmatch(line); m != nil {
			label, value = strings.TrimSpace(m[1]), m[2]
		} else if m := reBulletDialectOut.FindStringSubmatch(line); m != nil {
			label, value = strings.TrimSpace(m[1]), m[2]
		} else {
			continue
		}
		if !repairMetaLabels[label] {
			continue // 白名单外标签：一个字不改
		}
		value = strings.TrimSpace(value)
		if value == "" || !repairBulletValueOK(label, value) {
			continue // 取值守卫不过 = 可能是正文句子/待报问题，绝不代改语义
		}
		fixed := "- **" + label + "**：" + value
		edits = append(edits, repairEdit{
			kind: editReplace, line0: i, new: []string{fixed},
			fix: Fix{Kind: FixBulletForm, Line: i + 1,
				Before: clipFixText(line), After: clipFixText(fixed), NeedsReview: false,
				Note: "元信息行冒号/加粗形态归一为 `- **标签**：值`（全角冒号、冒号在粗体外）；标签与取值逐字保留",
			},
		})
	}
	return edits
}

// repairBulletValueOK 标签对应的取值守卫：能确定「这行就是元信息」才归一形态。
// 守卫只判断、不改动任何取值字节（认证值/变更值等属于 D2「不改」清单）。
func repairBulletValueOK(label, value string) bool {
	switch label {
	case "URL":
		v := strings.Trim(value, "` \t")
		parts := strings.Fields(v)
		return len(parts) == 2 && strings.HasPrefix(parts[1], "/")
	case "变更":
		switch value {
		case ChangeAdded, ChangeUpdated, ChangeRemoved, ChangeUnchanged:
			return true
		}
		return false
	case "认证":
		v := strings.Trim(value, "` \t")
		switch v {
		case AuthLogin, AuthNoLogin, AuthInternal:
			return true
		}
		return false
	case "breaking":
		switch strings.ToLower(value) {
		case "true", "false":
			return true
		}
		return false
	case "变更点", "Header", "Content-Type":
		return !strings.ContainsAny(value, "\r\n")
	}
	return false
}

// ===================== 3. 参数表补「位置」列 =====================

// collectPosEdits 给缺「位置」列的请求/路径参数表补列：
// 列位按规范骨架插在「必填」之后（无必填列退「类型」后，再退「名称」后，最后表尾）；
// 取值 = 解析器同一规则（路径参数表→Path；其余按 method：GET/DELETE→Query，其他→Body，
// 见 inferPos），补列后解析器读到的 Position 与它此前推断的逐字节相等；
// 其余单元格文本逐字节不变（只允许首尾空白按表格字面约定归一为 `| a | b |` 形态）；
// 每一条都标 NeedsReview=true。
func collectPosEdits(lines []string, doc *Document, posInserted map[string]bool) []repairEdit {
	var edits []repairEdit
	mask := repairFenceMask(lines)
	done := map[int]bool{} // 同一表头行只处理一次
	for _, ep := range doc.Endpoints {
		for _, th := range ep.TableHeaders {
			if th.Section != KindRequestParams && th.Section != KindPathParams {
				continue // 返回参数/命名结构表本就不需要位置列（§13 列要求）
			}
			hasPos := false
			for _, c := range th.Cols {
				if c == "位置" {
					hasPos = true
				}
			}
			if hasPos {
				continue
			}
			h0 := th.Line - 1
			if h0 < 0 || h0 >= len(lines) || mask[h0] || done[h0] || !isTableRow(lines[h0]) {
				continue
			}
			cols := splitTableRow(lines[h0])
			if len(cols) != len(th.Cols) {
				continue // 行内容与解析产物不一致（文本已被别的修复改动）：保守跳过，下一轮重看
			}
			if h0+1 >= len(lines) || !isTableSep(lines[h0+1]) {
				continue
			}
			done[h0] = true
			at := repairPosColIndex(cols)
			newCols := repairInsertCell(cols, at, "位置")
			newHeader := repairJoinRow(newCols)
			sepCells := splitTableRow(lines[h0+1])
			newSep := ""
			if len(sepCells) == len(cols) {
				newSep = repairJoinRow(repairInsertCell(sepCells, at, "----"))
			}
			pos := inferPos(ep.Method)
			reason := "按 method " + ep.Method + " 推断（GET/DELETE→Query，其余→Body）= " + pos
			if th.Section == KindPathParams {
				pos = PosPath
				reason = "小节标题为「路径参数」，推断为 Path"
			}
			edited := 1 // 表头行
			edits = append(edits, repairEdit{kind: editReplace, line0: h0, new: []string{newHeader},
				fix: Fix{Kind: FixPosColumn, Line: th.Line,
					Before: clipFixText(lines[h0]), After: clipFixText(newHeader), NeedsReview: true,
					Note: "补「位置」列，取值" + reason + "；" + "该列全部取值由规则推断，需人工核对",
				}})
			if newSep != "" {
				edits = append(edits, repairEdit{kind: editReplace, line0: h0 + 1, new: []string{newSep}})
			}
			for j := h0 + 2; j < len(lines) && !mask[j] && isTableRow(lines[j]); j++ {
				cells := splitTableRow(lines[j])
				if len(cells) != len(cols) {
					continue // 列数与表头不符的行（E-PARAM-ROW-COLS）不代改，留给校验报出
				}
				edited++
				edits = append(edits, repairEdit{kind: editReplace, line0: j,
					new: []string{repairJoinRow(repairInsertCell(cells, at, pos))}})
			}
			posInserted[strings.TrimSpace(newHeader)] = true
			// 合并同表的行编辑 fix 说明到表头 fix 的 Note（卡片一条 = 一张表，含行数）
			for k := range edits {
				if edits[k].fix.Kind == FixPosColumn && edits[k].line0 == h0 {
					edits[k].fix.Note += "（共 " + itoa(edited-1) + " 个数据行）"
				}
			}
		}
	}
	return edits
}

func repairInferredName(method string) string { return inferPos(method) }

// repairPosColIndex 新列插入位：必填后 > 类型后 > 名称后 > 表尾（规范骨架：参数名|类型|必填|位置|…）。
func repairPosColIndex(cols []string) int {
	for i, c := range cols {
		if c == "必填" {
			return i + 1
		}
	}
	for i, c := range cols {
		if c == "类型" {
			return i + 1
		}
	}
	for i, c := range cols {
		if c == "参数名" || c == "字段" {
			return i + 1
		}
	}
	return len(cols)
}

func repairInsertCell(cells []string, at int, val string) []string {
	if at < 0 {
		at = 0
	}
	if at > len(cells) {
		at = len(cells)
	}
	out := make([]string, 0, len(cells)+1)
	out = append(out, cells[:at]...)
	out = append(out, val)
	out = append(out, cells[at:]...)
	return out
}

func repairJoinRow(cells []string) string {
	return "| " + strings.Join(cells, " | ") + " |"
}

// ===================== 4. 交叉引用确定性展开 =====================

// collectCrossRefEdits 把「同新增订阅。」「元素结构见 7.1」这类引用式写法展开为
// 被引用接口参数表的原样搬运。找不到被引用对象、多候选、小节归属歧义 → 一字不改，
// 原 E-CROSS-REF / W-CROSS-REF 保留（AC5：绝不自己编字段表）。
func collectCrossRefEdits(lines []string, doc *Document, posInserted map[string]bool) []repairEdit {
	var edits []repairEdit
	mask := repairFenceMask(lines)
	for _, is := range doc.Issues {
		if is.Code != ErrCrossRef && is.Code != WarnCrossRef {
			continue
		}
		l1 := is.Line
		if l1 < 1 || l1 > len(lines) || mask[l1-1] {
			continue
		}
		ep := repairOwnerEndpoint(doc, l1)
		if ep == nil {
			continue
		}
		kind, stateOK := repairSlotOfLine(lines, mask, ep, l1)
		if !stateOK {
			continue // 归属小节不是该接口的 cross_ref 槽位：歧义，不动
		}
		target := repairResolveRefTarget(doc, ep, is.Snippet)
		if target == nil {
			continue // 找不到 / 多候选 / 自引用 → 保留原 Issue，不改
		}
		table := repairSourceTable(lines, mask, target, kind)
		if table == nil {
			continue // 被引用接口该槽位没有可搬运的表 → 不改
		}
		// 插到该段散文（引用行及其后续非空行）末尾之后，散文本身原样保留（保真）
		last := l1 - 1
		for i := l1; i < len(lines); i++ {
			t := strings.TrimSpace(lines[i])
			hl, _ := headingLevel(lines[i])
			if t == "" || mask[i] || strings.HasPrefix(t, "|") || hl != 0 {
				break
			}
			last = i
		}
		newLines := make([]string, 0, len(table)+1)
		newLines = append(newLines, table...)
		newLines = append(newLines, "")
		needsReview := posInserted[strings.TrimSpace(table[0])]
		note := "原样搬运被引用接口「" + target.Name + "」（" + target.Method + " " + target.PathRaw + "）的参数表，共 " + itoa(len(table)) + " 行；未生成任何新字段"
		if needsReview {
			note += "（注意：被搬运的表本身含修复器推断的「位置」列，需人工核对）"
		}
		edits = append(edits, repairEdit{
			kind: editInsertAfter, line0: last, new: newLines,
			fix: Fix{Kind: FixCrossRefExpand, Line: l1,
				Before:      clipFixText(is.Snippet),
				After:       clipFixText(table[0]) + "（搬运整表 " + itoa(len(table)) + " 行插入于此后）",
				NeedsReview: needsReview, Note: note,
			},
		})
	}
	return edits
}

// repairOwnerEndpoint l1 行所在接口块（[Line, EndLine)，EndLine 是 1-based 独占端点）。
func repairOwnerEndpoint(doc *Document, l1 int) *Endpoint {
	for _, ep := range doc.Endpoints {
		if ep.Line <= l1 && l1 < ep.EndLine {
			return ep
		}
	}
	return nil
}

// repairSlotOfLine 从引用行向上找所属 #### 小节标题（与解析器同一前缀匹配口径），
// 并要求该槽位在解析产物里确实是 SectionCrossRef —— 双条件都过才认定归属。
func repairSlotOfLine(lines []string, mask []bool, ep *Endpoint, l1 int) (string, bool) {
	for i := l1 - 2; i >= ep.Line-1; i-- {
		if mask[i] {
			continue
		}
		level, title := headingLevel(lines[i])
		if level != 4 {
			continue
		}
		switch {
		case strings.HasPrefix(title, "请求参数"):
			return KindRequestParams, ep.ReqParamsState == SectionCrossRef
		case strings.HasPrefix(title, "路径参数"):
			return KindPathParams, ep.PathParamsState == SectionCrossRef
		case strings.HasPrefix(title, "返回参数"):
			return KindResponseParams, ep.RespParamsState == SectionCrossRef
		default:
			return "", false // 归属未知小节：歧义，不动
		}
	}
	return "", false
}

// repairResolveRefTarget 解析引用对象：先按编号 token（### N.M 的 Seq 精确匹配），
// 再按「同<名称>」（H3 标题剥序号后的接口名精确匹配）。0 个或 >1 个候选返回 nil。
func repairResolveRefTarget(doc *Document, self *Endpoint, prose string) *Endpoint {
	if toks := reRepairSeqToken.FindAllString(prose, -1); len(toks) > 0 {
		var found *Endpoint
		count := 0
		for _, ep := range doc.Endpoints {
			if ep == self || ep.Seq == "" {
				continue
			}
			for _, tk := range toks {
				if ep.Seq == tk {
					count++
					found = ep
					break
				}
			}
		}
		if count == 1 {
			return found
		}
		return nil // 编号引用不做名称回退（两个口径同时命中反而是歧义信号）
	}
	if m := reRepairSameRef.FindStringSubmatch(strings.TrimSpace(prose)); m != nil {
		name := strings.TrimSpace(m[1])
		var found *Endpoint
		count := 0
		for _, ep := range doc.Endpoints {
			if ep == self {
				continue
			}
			if ep.Name == name {
				count++
				found = ep
			}
		}
		if count == 1 {
			return found
		}
	}
	return nil
}

// repairSourceTable 取被引用接口在 kind 槽位已路由的主表原文（表头行 → 最后数据行）。
// 判据 = 解析产物 TableHeaders（只有真正被路由的表才在里面），绝不搬运未识别的表。
func repairSourceTable(lines []string, mask []bool, target *Endpoint, kind string) []string {
	headerLine := 0
	for _, th := range target.TableHeaders {
		if th.Section == kind {
			headerLine = th.Line
			break
		}
	}
	if headerLine <= 0 {
		return nil
	}
	h0 := headerLine - 1
	if h0 >= len(lines) || mask[h0] || !isTableRow(lines[h0]) {
		return nil
	}
	cols := len(splitTableRow(lines[h0]))
	if h0+1 >= len(lines) || !isTableSep(lines[h0+1]) || len(splitTableRow(lines[h0+1])) != cols {
		return nil
	}
	j := h0 + 2
	for j < len(lines) && !mask[j] && isTableRow(lines[j]) {
		j++
	}
	if j <= h0+2 {
		// 无数据行的空表不搬（搬运后仍是「无字段」，只会掩盖原 Issue）
		return nil
	}
	out := make([]string, 0, j-h0)
	out = append(out, lines[h0:j]...)
	return out
}
