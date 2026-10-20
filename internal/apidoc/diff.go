package apidoc

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// ===================== diff 载荷（存 api_change_items.diff）=====================

// DiffItem 字段级 diff 载荷：{section,key,op,before,after,breaking,rule}。
// before/after 存「能独立渲染的最小完整片段」（自带完整字段描述），不是 RFC 6902 补丁——
// 前端要直接渲染，不能回查上下文。Before/After 的实际类型：FieldFragment / SchemaFragment / string（moved）。
type DiffItem struct {
	Section      string `json:"section"` // meta | path.params | request.query | request.body | request.header | response.body
	Key          string `json:"key"`     // 字段名（renamed 用 before 名；schema 字段用 <Schema>.<field>）
	Op           string `json:"op"`      // added | removed | updated | renamed | moved
	Before       any    `json:"before,omitempty"`
	After        any    `json:"after,omitempty"`
	Breaking     bool   `json:"breaking"`               // 自动判破坏（三态里的 1）
	NeedsConfirm bool   `json:"needsConfirm,omitempty"` // 需人工确认（三态里的 2，等同破坏处理）
	Rule         string `json:"rule"`                   // oasdiff check id 词表（UI 统一 tooltip）
}

// section 固定词表（与「位置」列取值一一对应，让「参数从 Query 挪到 Body」能被显式表达）。
const (
	SectionMeta          = "meta"
	SectionPathParams    = "path.params"
	SectionRequestQuery  = "request.query"
	SectionRequestBody   = "request.body"
	SectionRequestHeader = "request.header"
	SectionResponseBody  = "response.body"
)

// op 词表。
const (
	OpAdded   = "added"
	OpRemoved = "removed"
	OpUpdated = "updated"
	OpRenamed = "renamed"
	OpMoved   = "moved"
)

// breaking 三态（事件/明细行 breaking 列：0 无 | 1 破坏 | 2 需人工确认）。
const (
	BreakingNo      = 0
	BreakingYes     = 1
	BreakingConfirm = 2
)

// FieldFragment 能独立渲染的最小字段片段。
// Required 用 -1/0/1 哨兵（与 api_fields.required 一致，仓库约定零指针可空字段），
// 而不是 spec-draft 示例 JSON 里的 true/false——渲染层按需映射。
type FieldFragment struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // type_raw 显示值
	Required int    `json:"required"`
	Position string `json:"position,omitempty"`
	Default  string `json:"default,omitempty"`
	Desc     string `json:"desc,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// SchemaFragment 命名结构片段（schema 级 added/removed 用）。
type SchemaFragment struct {
	Name   string          `json:"name"`
	Fields []FieldFragment `json:"fields"`
}

func fieldFragment(f Field) FieldFragment {
	return FieldFragment{Name: f.Name, Type: f.TypeRaw, Required: f.Required,
		Position: f.Position, Default: f.Default, Desc: f.Desc, Line: f.Line}
}

func schemaFragment(s SchemaDef) SchemaFragment {
	fr := SchemaFragment{Name: s.Name}
	for _, f := range s.Fields {
		fr.Fields = append(fr.Fields, fieldFragment(f))
	}
	return fr
}

// ===================== 双 hash（spec-draft §6）=====================
//
// semantic_hash：结构化事实，参与变更判定、进事件明细。
// doc_hash：中文措辞与示例——只变 doc_hash → 不算接口变更、不发事件
// （| message | String | 响应消息 | 这类 envelope 行在 20 张表里逐字重复，
// 说明/示例一旦算进变更判定，一次全局润色就能造 20+ 条假事件，打爆幂等验收）。
//
// ⚠️ SemanticHash 只使用能从数据库回建的输入（method/normPath/content_type/auth/
// 字段 (位置,名称,type_key,必填,默认值)/响应 (名称,type_key)/命名结构集合），
// 保证「导入时存的 hash」与「下次导入重算的 hash」可比。

// SemanticHash 结构化事实指纹（sha256 hex）。
func SemanticHash(ep *Endpoint) string {
	var b strings.Builder
	b.WriteString("M|" + ep.Method + "\n")
	b.WriteString("P|" + NormPath(ep.PathRaw) + "\n")
	b.WriteString("CT|" + ep.ContentType + "\n")
	b.WriteString("AU|" + ep.Auth + "\n")
	// 参数集合 (位置,名称,type_key,必填,默认值)，排序保证确定性
	var params []string
	for _, f := range ep.PathFields {
		params = append(params, paramLine(f))
	}
	for _, f := range ep.RequestFields {
		params = append(params, paramLine(f))
	}
	sort.Strings(params)
	for _, l := range params {
		b.WriteString("F|" + l + "\n")
	}
	// 返回结构 (字段名,type_key)
	var resps []string
	for _, f := range ep.ResponseFields {
		resps = append(resps, f.Name+"|"+f.TypeKey)
	}
	sort.Strings(resps)
	for _, l := range resps {
		b.WriteString("R|" + l + "\n")
	}
	// 命名结构集合
	var schs []string
	for _, s := range ep.Schemas {
		schs = append(schs, s.Name+"|"+schemaFieldsCanon(s.Fields))
	}
	sort.Strings(schs)
	for _, l := range schs {
		b.WriteString("S|" + l + "\n")
	}
	return sha256Hex(b.String())
}

func paramLine(f Field) string {
	return f.Position + "|" + f.Name + "|" + f.TypeKey + "|" + strconv.Itoa(f.Required) + "|" + f.Default
}

// schemaFieldsCanon 命名结构字段的规范化串（排序，name:type_key）。
func schemaFieldsCanon(fields []Field) string {
	var parts []string
	for _, f := range fields {
		parts = append(parts, f.Name+":"+f.TypeKey)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// schemaBodyHash 命名结构体指纹（同名不同构判定用）。
func schemaBodyHash(fields []Field) string {
	return sha256Hex(schemaFieldsCanon(fields))
}

// DocHash 中文措辞与示例指纹（sha256 hex）：中文说明、接口名、示例内容、curl 文本、表头列集合。
func DocHash(ep *Endpoint) string {
	var b strings.Builder
	b.WriteString("N|" + ep.Name + "\n")
	b.WriteString("D|" + ep.Description + "\n")
	// 字段中文说明（含命名结构字段），文档序
	for _, f := range ep.PathFields {
		b.WriteString("FD|path|" + f.Name + "|" + f.Desc + "\n")
	}
	for _, f := range ep.RequestFields {
		b.WriteString("FD|req|" + f.Name + "|" + f.Desc + "\n")
	}
	for _, f := range ep.ResponseFields {
		b.WriteString("FD|resp|" + f.Name + "|" + f.Desc + "\n")
	}
	for _, s := range ep.Schemas {
		for _, f := range s.Fields {
			b.WriteString("FD|schema:" + s.Name + "|" + f.Name + "|" + f.Desc + "\n")
		}
	}
	// 表头列集合（集合语义：列序无关，排序后参与）
	for _, h := range ep.TableHeaders {
		cols := append([]string(nil), h.Cols...)
		sort.Strings(cols)
		b.WriteString("H|" + h.Section + "|" + strings.Join(cols, ",") + "\n")
	}
	// 示例与 curl 原文，文档序
	for _, ex := range ep.Examples {
		b.WriteString("E|" + ex.Kind + "|" + ex.Content + "\n")
	}
	return sha256Hex(b.String())
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ===================== 字段级 diff =====================

// DiffEndpoints 计算 before → after 的字段级 diff 载荷（spec-draft §7）。
//
// 规则要点：
//   - 路径静态段或 method 变更 → 落一条 moved（{before:"POST /a",after:"POST /b"}），
//     不拆成 removed+added（否则前端读成「接口下线 + 新接口上线」）；
//   - 占位符改名 {id}→{paperId} 是 cosmetic：NormPath 相同 → 不产生 moved；
//     path.params 组内的 renamed 配对也不记（客户端实际 URL 一字未变）；
//   - 只有说明/示例/curl/中文名变化 → 返回空（那些只进 doc_hash）。
//
// 注意：semantic_hash 变化但 diff 为空的唯一情形就是「占位符改名」这类 cosmetic——
// 导入层应以 diff 载荷为准判定是否生成事件（diff 为空则不生成事件，D10）。
func DiffEndpoints(before, after *Endpoint) []DiffItem {
	var items []DiffItem

	// —— meta：路径/方法改名落一条 moved ——
	if before.IdentityKey != after.IdentityKey {
		rule := "path-changed"
		if before.Method != after.Method && NormPath(before.PathRaw) == NormPath(after.PathRaw) {
			rule = "method-changed"
		}
		items = append(items, DiffItem{
			Section: SectionMeta, Key: "endpoint", Op: OpMoved,
			Before:   before.Method + " " + before.PathRaw,
			After:    after.Method + " " + after.PathRaw,
			Breaking: true, Rule: rule,
		})
	}
	// —— meta：Content-Type 变更（json ↔ multipart 自动破坏；空↔有值 = 文档补全非破坏）——
	if before.ContentType != after.ContentType {
		items = append(items, DiffItem{
			Section: SectionMeta, Key: "content_type", Op: OpUpdated,
			Before: before.ContentType, After: after.ContentType,
			Breaking: before.ContentType != "" && after.ContentType != "",
			Rule:     "request-content-type-changed",
		})
	}
	// —— meta：认证枚举变化参与 breaking ——
	if before.Auth != after.Auth {
		breaking, needs := false, false
		rule := "request-authentication-changed"
		switch {
		case after.Auth == AuthInternal && before.Auth != "" && before.Auth != AuthInternal:
			// 「外部可调用 → 内部Token」= 前端从此不能调这个接口 → 自动破坏
			breaking = true
			rule = "request-authentication-internalized"
		case before.Auth != "" && after.Auth != "":
			// 鉴权要求其他方向变化 → 需人工确认
			needs = true
		}
		// 一边为空 = 文档补全，不算破坏
		items = append(items, DiffItem{
			Section: SectionMeta, Key: "auth", Op: OpUpdated,
			Before: before.Auth, After: after.Auth,
			Breaking: breaking, NeedsConfirm: needs, Rule: rule,
		})
	}

	// —— 参数跨位置迁移（Query→Body 这类真实破坏）先摘出来，避免被拆成 removed+added ——
	movedNames := map[string]bool{}
	beforeReq := requestFieldMap(before)
	afterReq := requestFieldMap(after)
	for name, bpos := range beforeReq {
		apos, ok := afterReq[name]
		if !ok || apos == bpos {
			continue
		}
		movedNames[name] = true
		items = append(items, DiffItem{
			Section: sectionOfPosition(bpos), Key: name, Op: OpMoved,
			Before:   fieldFragment(*findReqField(before, name)),
			After:    fieldFragment(*findReqField(after, name)),
			Breaking: true, Rule: "request-parameter-position-changed",
		})
	}
	sortItems(items[metaCount(items):]) // moved 内部按 key 排序保证确定性（map 遍历序随机）

	// —— 分组字段 diff（固定组序，组内文档序）——
	for _, g := range []struct{ section, position string }{
		{SectionPathParams, PosPath},
		{SectionRequestQuery, PosQuery},
		{SectionRequestBody, PosBody},
		{SectionRequestHeader, PosHeader},
	} {
		bf := fieldsAtPosition(before, g.position)
		af := fieldsAtPosition(after, g.position)
		items = append(items, diffFieldGroup(g.section, bf, af, movedNames, g.position == PosPath)...)
	}
	items = append(items, diffFieldGroup(SectionResponseBody, before.ResponseFields, after.ResponseFields, nil, false)...)

	// —— 命名结构集合 diff（结构变化会体现在 semantic_hash，必须落到载荷）——
	items = append(items, diffSchemas(before.Schemas, after.Schemas)...)

	return items
}

func metaCount(items []DiffItem) int {
	n := 0
	for _, it := range items {
		if it.Section == SectionMeta {
			n++
		}
	}
	return n
}

func sortItems(items []DiffItem) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].Key < items[j].Key })
}

// requestFieldMap 请求侧字段（Path/Query/Body/Header）name → position。
func requestFieldMap(ep *Endpoint) map[string]string {
	m := map[string]string{}
	for _, f := range ep.PathFields {
		m[f.Name] = PosPath
	}
	for _, f := range ep.RequestFields {
		if f.Position == "" {
			continue
		}
		m[f.Name] = f.Position
	}
	return m
}

func findReqField(ep *Endpoint, name string) *Field {
	for i := range ep.PathFields {
		if ep.PathFields[i].Name == name {
			return &ep.PathFields[i]
		}
	}
	for i := range ep.RequestFields {
		if ep.RequestFields[i].Name == name {
			return &ep.RequestFields[i]
		}
	}
	return nil
}

func fieldsAtPosition(ep *Endpoint, pos string) []Field {
	var out []Field
	if pos == PosPath {
		out = append(out, ep.PathFields...)
	}
	for _, f := range ep.RequestFields {
		if f.Position == pos {
			out = append(out, f)
		}
	}
	return out
}

func sectionOfPosition(pos string) string {
	switch pos {
	case PosPath:
		return SectionPathParams
	case PosQuery:
		return SectionRequestQuery
	case PosBody:
		return SectionRequestBody
	case PosHeader:
		return SectionRequestHeader
	}
	return SectionRequestBody
}

// diffFieldGroup 单组字段 diff：
// updated（type_key/必填/默认值）→ renamed（removed+added 同型配对）→ added → removed。
// isPath 组特殊：占位符改名是 cosmetic，renamed 配对不记。
func diffFieldGroup(section string, bf, af []Field, skipNames map[string]bool, isPath bool) []DiffItem {
	var items []DiffItem
	bIdx := map[string]int{}
	for i := range bf {
		if _, dup := bIdx[bf[i].Name]; !dup {
			bIdx[bf[i].Name] = i
		}
	}
	aIdx := map[string]int{}
	for i := range af {
		if _, dup := aIdx[af[i].Name]; !dup {
			aIdx[af[i].Name] = i
		}
	}
	isRequest := section != SectionResponseBody

	// 先做 renamed 配对（removed+added 同型：type_key/必填/默认值一致），配对成功的不再落 added/removed
	pairedA := map[string]bool{}
	pairedB := map[string]bool{}
	type pair struct{ b, a Field }
	var pairs []pair
	for i := range bf {
		b := bf[i]
		if skipNames[b.Name] || pairedB[b.Name] {
			continue
		}
		if _, still := aIdx[b.Name]; still {
			continue // 两边同名 → updated 路径处理
		}
		for j := range af {
			a := af[j]
			if pairedA[a.Name] || skipNames[a.Name] {
				continue
			}
			if _, still := bIdx[a.Name]; still {
				continue
			}
			if a.TypeKey == b.TypeKey && a.Required == b.Required && a.Default == b.Default {
				pairs = append(pairs, pair{b, a})
				pairedA[a.Name] = true
				pairedB[b.Name] = true
				break
			}
		}
	}
	for _, pr := range pairs {
		if isPath {
			continue // 占位符改名 {id}→{paperId}：客户端实际 URL 一字未变，cosmetic 不记
		}
		rule := "response-property-removed+added"
		if isRequest {
			rule = "request-property-removed+added"
		}
		items = append(items, DiffItem{
			Section: section, Key: pr.b.Name, Op: OpRenamed,
			Before: fieldFragment(pr.b), After: fieldFragment(pr.a),
			NeedsConfirm: true, Rule: rule, // 字段改名 = 需人工确认（等同破坏处理）
		})
	}

	// updated / added（after 文档序）
	for j := range af {
		a := af[j]
		if skipNames[a.Name] || pairedA[a.Name] {
			continue
		}
		i, ok := bIdx[a.Name]
		if !ok {
			breaking, rule := false, "response-property-added"
			if isRequest {
				if a.Required == RequiredYes {
					breaking, rule = true, "required-request-parameter-added" // 新增必填请求参数：老客户端不带 → 直接 400
				} else {
					rule = "request-parameter-added"
				}
				if isPath {
					breaking, rule = true, "path-parameter-added"
				}
			}
			items = append(items, DiffItem{
				Section: section, Key: a.Name, Op: OpAdded,
				After: fieldFragment(a), Breaking: breaking, Rule: rule,
			})
			continue
		}
		b := bf[i]
		if it, ok := diffUpdated(section, b, a, isRequest, isPath); ok {
			items = append(items, it)
		}
	}
	// removed（before 文档序）
	for i := range bf {
		b := bf[i]
		if skipNames[b.Name] || pairedB[b.Name] {
			continue
		}
		if _, still := aIdx[b.Name]; still {
			continue
		}
		switch {
		case isPath:
			items = append(items, DiffItem{
				Section: section, Key: b.Name, Op: OpRemoved,
				Before: fieldFragment(b), Breaking: true, Rule: "path-parameter-removed",
			})
		case isRequest:
			// 请求参数删除：结构上算破坏，实际客户端照发、后端多忽略——
			// 放自动档会让确认闸天天空响 → 需人工确认（breaking=2）
			items = append(items, DiffItem{
				Section: section, Key: b.Name, Op: OpRemoved,
				Before: fieldFragment(b), NeedsConfirm: true, Rule: "request-parameter-removed",
			})
		default:
			items = append(items, DiffItem{
				Section: section, Key: b.Name, Op: OpRemoved,
				Before: fieldFragment(b), Breaking: true, Rule: "response-property-removed",
			})
		}
	}
	return items
}

// diffUpdated 同名同组字段的 type_key/必填/默认值比较（说明变化 = cosmetic，不产生条目）。
func diffUpdated(section string, b, a Field, isRequest, isPath bool) (DiffItem, bool) {
	breaking, needs := false, false
	rule := ""
	if b.TypeKey != a.TypeKey {
		breaking = true // 类型变更（按 type_key 归一后比）
		rule = "response-property-type-changed"
		if isRequest {
			rule = "request-property-type-changed"
		}
		if isPath {
			rule = "path-parameter-type-changed"
		}
	}
	if b.Required != a.Required && isRequest {
		switch {
		case b.Required == RequiredNo && a.Required == RequiredYes:
			breaking = true // 必填 否→是
			if rule == "" {
				rule = "request-property-became-required"
			}
		case b.Required == RequiredUnknown && a.Required == RequiredYes:
			needs = true // 必填列从无到有且为「是」：无法确定历史语义 → 人工确认
			if rule == "" {
				rule = "request-property-became-required"
			}
		case b.Required == RequiredYes && a.Required != RequiredYes:
			if rule == "" {
				rule = "request-property-no-longer-required"
			}
		default:
			if rule == "" {
				rule = "request-property-required-changed"
			}
		}
	}
	if b.Default != a.Default && isRequest {
		if b.Default != "" && a.Default == "" {
			breaking = true // 默认值被清空 = 事实上的收紧（§7 档 1 ②）
			if rule == "" {
				rule = "request-parameter-default-removed"
			}
		} else if rule == "" {
			rule = "request-parameter-default-changed"
		}
	}
	if rule == "" {
		return DiffItem{}, false // 只有说明/顺序变化 → cosmetic 不记
	}
	return DiffItem{
		Section: section, Key: a.Name, Op: OpUpdated,
		Before: fieldFragment(b), After: fieldFragment(a),
		Breaking: breaking, NeedsConfirm: needs && !breaking, Rule: rule,
	}, true
}

// diffSchemas 命名结构集合 diff（section 一律 response.body，key 用 <Schema> 或 <Schema>.<field>）。
func diffSchemas(bs, as []SchemaDef) []DiffItem {
	var items []DiffItem
	bIdx := map[string]int{}
	for i := range bs {
		if _, dup := bIdx[bs[i].Name]; !dup {
			bIdx[bs[i].Name] = i
		}
	}
	aIdx := map[string]int{}
	for i := range as {
		if _, dup := aIdx[as[i].Name]; !dup {
			aIdx[as[i].Name] = i
		}
	}
	for j := range as {
		a := as[j]
		i, ok := bIdx[a.Name]
		if !ok {
			items = append(items, DiffItem{
				Section: SectionResponseBody, Key: a.Name, Op: OpAdded,
				After: schemaFragment(a), Rule: "response-schema-added",
			})
			continue
		}
		b := bs[i]
		if b.BodyHash == a.BodyHash {
			continue
		}
		sub := diffFieldGroup(SectionResponseBody, b.Fields, a.Fields, nil, false)
		for k := range sub {
			sub[k].Key = a.Name + "." + sub[k].Key
		}
		items = append(items, sub...)
	}
	for i := range bs {
		if _, still := aIdx[bs[i].Name]; !still {
			items = append(items, DiffItem{
				Section: SectionResponseBody, Key: bs[i].Name, Op: OpRemoved,
				Before: schemaFragment(bs[i]), Breaking: true, Rule: "response-schema-removed",
			})
		}
	}
	return items
}

// ===================== breaking 三态聚合与人工覆盖 =====================

// AggregateBreaking 明细 → 事件头三态：
// 任一自动破坏 → 1；否则任一需人工确认或含 removed → 2（D16 确认闸：removed 必须二次确认）；否则 0。
func AggregateBreaking(items []DiffItem) int {
	has1, has2 := false, false
	for _, it := range items {
		if it.Breaking {
			has1 = true
		}
		if it.NeedsConfirm || it.Op == OpRemoved {
			has2 = true
		}
	}
	switch {
	case has1:
		return BreakingYes
	case has2:
		return BreakingConfirm
	}
	return BreakingNo
}

// ValidateBreakingOverride 人工覆盖校验（spec-draft §3.1/§7）：
// 文档声明 breaking: false 而启发式判破坏 → 拒收（E-BREAKING-DENIED，由调用方落 Issue）；
// 声明 true 而启发式非破坏 → 接受（人比规则懂业务）。
func ValidateBreakingOverride(decl int, items []DiffItem) bool {
	if decl == 0 {
		return AggregateBreaking(items) == BreakingNo
	}
	return true
}
