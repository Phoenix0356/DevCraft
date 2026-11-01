package apidoc

import (
	"strconv"
	"strings"
)

// ===================== 导出渲染（库内当前态 → 规范文档）=====================

// ExportUpdateItem 导出「本次更新」清单的一行（来自 api_change_items）。
type ExportUpdateItem struct {
	Method      string
	Path        string
	EntryName   string
	ChangeType  string // added|updated|removed|moved|restored
	ChangePoint string // = declared_note
}

// ExportUpdate 最新一次变更事件的摘要 + 涉及接口清单（PRD R2）。
// 只渲染 R2 要求的要素（标题/说明/时间/清单）；事件 kind/breaking 不参与导出，
// 所以不随结构体携带（曾带过：渲染器不读，纯死字段）。
type ExportUpdate struct {
	Seq       int
	Label     string
	Title     string
	Reason    string
	CreatedAt string
	Items     []ExportUpdateItem
}

// ExportDomain 一个领域章节：标题 + 章级 preamble + 接口（库内 sort 序）。
type ExportDomain struct {
	Name     string
	Internal bool
	NotesRaw string
	Entries  []*Endpoint
}

// ExportInput 一份全量导出文档的渲染输入。Meta 只需 Project/Service/BasePath/
// Scope（渲染侧强制 init）；Title/ExportedAt/Total 供头图水印。
type ExportInput struct {
	Title      string
	ExportedAt string
	Total      int
	Meta       Meta
	Update     *ExportUpdate // nil = 项目还没有变更事件，整节省略
	Domains    []ExportDomain
}

// changeTypeLabel 明细变更类型的中文文案（与事件详情页口径一致）。
func changeTypeLabel(t string) string {
	switch t {
	case "added":
		return "新增"
	case "updated":
		return "修改"
	case "removed":
		return "删除"
	case "moved":
		return "移动"
	case "restored":
		return "恢复"
	}
	return t
}

// RenderExport 把库内当前态渲染为符合写作规范的 Markdown 文档。
//
// 设计约束（round-trip：导出文本必须能被 Parse 以 0 阻断收回归基线）：
//   - meta 固定 scope: init：导出是快照而不是某次变更声明，init 档放宽变更/bullet 硬规则；
//   - 「本次更新」写成 ## 章节但块内不含任何 ###（非领域章节 + 章级 preamble 散文/列表，
//     结构规则天然排除，不发 Issue）；接口清单用列表而不是表（章 preamble 的表不进任何槽位，
//     但列表更稳且不会被误认作汇总表自检的对象）；
//   - 小节内绝不落“表外散文”：routeParamSection 把小节内非表非粗体标题行归为 prose，
//     prose 会以 Unresolved/partial 落回库里——所以参数小节只有三种形态：表 / 「无。」 / 整体省略；
//   - 命名结构表（**X 结构**：）排在返回参数之后、示例小节之前：buildSchemas 按块收集粗体表，
//     位置不影响归属；但绝不能落在示例小节内（那里的表格会被直接忽略并发警告）。
//
// 已知限制（裁决为「接受」，不引入转义通道）：
//   - 单元格里的裸「|」无法往返：解析器 splitTableRow 按每个 | 切列、无转义通道，
//     含 | 的导出表重导会触发 E-PARAM-ROW-COLS（该行被跳过）。类型列实测保真写法是
//     List\<Long\>（反斜杠转义只作用于 <>，不作用于 |），说明/默认值含 | 属极少数脏数据。
//   - 「- **Header**：」bullet 不导出：api_entries 没有 header 列，库内当前态已丢，
//     导出侧无米下锅（内部 Token 接口的识别信息因此只剩「认证：内部Token」）。
//     两条同时记录在任务 prd.md「已知限制」。
func RenderExport(in ExportInput) string {
	var b strings.Builder
	b.WriteString("# " + in.Title + "\n\n")

	b.WriteString(fence3 + "apidoc-meta\n")
	b.WriteString("spec: 1\n")
	b.WriteString("project: " + in.Meta.Project + "\n")
	if in.Meta.Service != "" {
		b.WriteString("service: " + in.Meta.Service + "\n")
	}
	if in.Meta.BasePath != "" {
		b.WriteString("base_path: " + in.Meta.BasePath + "\n")
	}
	b.WriteString("scope: init\n")
	if in.Meta.VersionTag != "" {
		b.WriteString("version_tag: " + in.Meta.VersionTag + "\n")
	}
	b.WriteString(fence3 + "\n\n")

	if in.ExportedAt != "" {
		// R7 水印（PRD 原文口径「由 DevCraft 生成」）：导出时间 + 接口总数
		b.WriteString("> 由 DevCraft 生成 · " + in.ExportedAt + " · 共 " +
			strconv.Itoa(in.Total) + " 个接口\n\n")
	}

	if in.Update != nil {
		renderUpdate(&b, in.Update)
	}

	for di := range in.Domains {
		renderDomain(&b, di+1, &in.Domains[di])
	}
	return b.String()
}

// renderUpdate 「本次更新」= ## 标题 + 纯散文/preamble（事件摘要 + 接口清单列表）。
// 刻意不放 ###：无 URL bullet 的 ### 会落 Unparsed 并发 W-UNPARSED-BLOCK；
// preamble 散文行不产生任何 Issue。
func renderUpdate(b *strings.Builder, u *ExportUpdate) {
	b.WriteString("## 本次更新\n\n")
	title := u.Title
	if u.Label != "" {
		title = u.Label + "：" + title
	}
	b.WriteString("**" + title + "**（v" + strconv.Itoa(u.Seq) + " · " + u.CreatedAt + "）\n\n")
	if u.Reason != "" {
		b.WriteString(u.Reason + "\n\n")
	}
	if len(u.Items) > 0 {
		b.WriteString("本次涉及接口：\n\n")
		for _, it := range u.Items {
			line := "- " + it.Method + " " + it.Path + " —— " + changeTypeLabel(it.ChangeType)
			if it.EntryName != "" {
				line += "（" + it.EntryName + "）"
			}
			if it.ChangePoint != "" {
				line += "：" + it.ChangePoint
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
}

func renderDomain(b *strings.Builder, di int, dom *ExportDomain) {
	name := dom.Name
	if dom.Internal {
		name += "（内部）" // NormalizeDomainName 剥掉尾部限定语并置 Internal，复原写法保持往返一致
	}
	if name == "" {
		name = "未分类"
	}
	b.WriteString("## " + name + "\n\n")
	notes := strings.TrimSpace(dom.NotesRaw)
	if notes != "" {
		b.WriteString(notes + "\n\n")
	}
	for ei := range dom.Entries {
		renderEndpoint(b, di, ei+1, dom.Entries[ei])
	}
}

// renderEndpoint 一个接口块：### 标题 + 说明散文 + 元信息 bullet + 各小节 + 命名结构表。
// 变更/breaking 声明不导出：它们是「增量文档」的字段，快照文档由 scope: init 放宽。
func renderEndpoint(b *strings.Builder, di, ei int, ep *Endpoint) {
	b.WriteString("### " + strconv.Itoa(di) + "." + strconv.Itoa(ei) + " " + ep.Name + "\n\n")
	if desc := strings.TrimSpace(ep.Description); desc != "" {
		b.WriteString(desc + "\n\n")
	}
	b.WriteString("- **URL**：" + q + ep.Method + " " + ep.PathRaw + q + "\n")
	if ep.ContentType != "" {
		b.WriteString("- **Content-Type**：" + q + ep.ContentType + q + "\n")
	}
	if ep.Auth != "" {
		b.WriteString("- **认证**：" + ep.Auth + "\n")
	}
	b.WriteString("\n")

	unres := func(slot string) bool {
		for _, s := range ep.Unresolved {
			if s == slot {
				return true
			}
		}
		return false
	}

	if len(ep.PathFields) > 0 {
		b.WriteString("#### 路径参数\n\n")
		writeParamTable(b, ep.PathFields, true)
	}
	// 参数小节三态：有表 → 表；槽位在 Unresolved → 整体省略（导出没有的信息不编造，
	// 重导后依旧 unresolved，partial 状态忠实往返）；否则 → 「无。」（明确无参数）。
	b.WriteString("#### 请求参数\n\n")
	if len(ep.RequestFields) > 0 {
		writeParamTable(b, ep.RequestFields, true)
	} else if unres("request_params") {
		b.WriteString("\n") // 省略小节内容：小节存在但空 → 依旧 unresolved（忠实）
	} else {
		b.WriteString("无。\n\n")
	}

	// 示例：req_example 与 multipart_raw 共用「请求示例」小节（多个围栏 = 多个示例，
	// routeSections 按小节内围栏逐个路由；重复小节名会互相覆盖槽位，绝不输出两次同名 ####）。
	writeExampleSection(b, "#### 请求示例", ep, ExampleRequest, ExampleMultipart)

	b.WriteString("#### 返回参数\n\n")
	if len(ep.ResponseFields) > 0 {
		writeParamTable(b, ep.ResponseFields, false)
	} else if unres("response_params") {
		b.WriteString("\n")
	} else {
		b.WriteString("无。\n\n")
	}

	// 命名结构表紧跟返回参数小节、排在示例小节之前：示例小节只收代码围栏
	// （routeExample 见表即发 W-UNPARSED-BLOCK「该表格被忽略」），而参数小节会主动跳过
	// 粗体标题表（t.boldTitle != "" → continue），buildSchemas 又是按 ### 块全局收集粗体表——
	// 因此这个落位既不会被任何小节当成主表，也不会丢结构。
	for i := range ep.Schemas {
		sc := &ep.Schemas[i]
		b.WriteString("**" + sc.Name + " 结构**：\n\n")
		writeParamTable(b, sc.Fields, false)
	}

	writeExampleSection(b, "#### 返回示例", ep, ExampleResponse)
	writeExampleSection(b, "#### 错误响应示例", ep, ExampleError)
	writeExampleSection(b, "#### 测试 curl 命令", ep, ExampleCURL)
}

// writeExampleSection 一个示例小节：按 kind 集合过滤 Examples（保持 Sort 序），
// 每种围栏 info string 按 language 复原（multipart/text → 无标记，json → json，curl → bash）。
func writeExampleSection(b *strings.Builder, heading string, ep *Endpoint, kinds ...string) {
	var picked []Example
	for _, ex := range ep.Examples {
		for _, k := range kinds {
			if ex.Kind == k {
				picked = append(picked, ex)
			}
		}
	}
	if len(picked) == 0 {
		return
	}
	b.WriteString(heading + "\n\n")
	for _, ex := range picked {
		info := ""
		switch ex.Language {
		case "json":
			info = "json"
		case "bash":
			info = "bash"
		}
		content := strings.TrimRight(ex.Content, "\n")
		b.WriteString(fence3 + info + "\n" + content + "\n" + fence3 + "\n\n")
	}
}

// writeParamTable 参数/字段表。reqTable=true 用 6 列规范表头（含 必填/位置/默认值），
// 否则用 3 列（参数名|类型|说明；命名结构同样只要求 名称|类型，位置/必填对响应/结构字段无意义）。
// Required -1 → 必填格留空（parseRequiredCell 空值 = 未知，不伪造「否」）。
func writeParamTable(b *strings.Builder, fields []Field, reqTable bool) {
	if reqTable {
		b.WriteString("| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n")
		b.WriteString("|--------|------|------|------|--------|------|\n")
		for _, f := range fields {
			b.WriteString("| " + f.Name + " | " + f.TypeRaw + " | " + requiredCell(f.Required) +
				" | " + f.Position + " | " + f.Default + " | " + f.Desc + " |\n")
		}
	} else {
		b.WriteString("| 参数名 | 类型 | 说明 |\n")
		b.WriteString("|--------|------|------|\n")
		for _, f := range fields {
			b.WriteString("| " + f.Name + " | " + f.TypeRaw + " | " + f.Desc + " |\n")
		}
	}
	b.WriteString("\n")
}

func requiredCell(r int) string {
	switch r {
	case RequiredYes:
		return "是"
	case RequiredNo:
		return "否"
	}
	return ""
}
