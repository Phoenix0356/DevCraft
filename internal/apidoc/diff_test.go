package apidoc

import "testing"

// newEp 构造 diff 测试用的最小 Endpoint（默认 json + 需要登录）。
func newEp(method, path string) *Endpoint {
	return &Endpoint{
		Name: "接口", Method: method, PathRaw: path,
		IdentityKey: IdentityKey(method, path),
		ContentType: "application/json", Auth: AuthLogin,
	}
}

func rf(name, typ string, required int, pos, def string) Field {
	return Field{Name: name, TypeRaw: typ, TypeKey: TypeKey(typ), Required: required, Position: pos, Default: def}
}

func findItem(items []DiffItem, section, key, op string) *DiffItem {
	for i := range items {
		if items[i].Section == section && items[i].Key == key && items[i].Op == op {
			return &items[i]
		}
	}
	return nil
}

// TestDiffCosmeticOnly 只改中文说明/接口名/示例/curl → semantic_hash 不变、doc_hash 变、diff 为空
// （AC4 无噪声：文档润色不产生假事件）。
func TestDiffCosmeticOnly(t *testing.T) {
	before := newEp("POST", "/api/v1/x")
	before.Description = "旧说明：查询用户。"
	before.Name = "查询用户"
	before.RequestFields = []Field{rf("id", "Long", RequiredYes, PosBody, "")}
	before.ResponseFields = []Field{rf("message", "String", RequiredUnknown, "", "")}
	before.Examples = []Example{{Kind: ExampleRequest, Content: `{"id":1}`}}

	after := newEp("POST", "/api/v1/x")
	after.Description = "新说明：按 ID 精确查询一个用户（润色过）。"
	after.Name = "按 ID 查询用户"
	after.RequestFields = []Field{rf("id", "Long", RequiredYes, PosBody, "")}
	after.ResponseFields = []Field{rf("message", "String", RequiredUnknown, "", "")}
	after.Examples = []Example{{Kind: ExampleRequest, Content: `{"id": 2}`}}

	if SemanticHash(before) != SemanticHash(after) {
		t.Fatal("措辞变化不应改变 semantic_hash")
	}
	if DocHash(before) == DocHash(after) {
		t.Fatal("措辞/示例变化应改变 doc_hash")
	}
	if items := DiffEndpoints(before, after); len(items) != 0 {
		t.Fatalf("措辞变化不应产生 diff: %+v", items)
	}
	// 字段中文说明变化同样是 cosmetic
	after2 := newEp("POST", "/api/v1/x")
	after2.RequestFields = []Field{rf("id", "Long", RequiredYes, PosBody, "")}
	after2.RequestFields[0].Desc = "改过的字段说明"
	after2.ResponseFields = []Field{rf("message", "String", RequiredUnknown, "", "")}
	if SemanticHash(before) != SemanticHash(after2) {
		t.Fatal("字段说明不进 semantic_hash")
	}
	if items := DiffEndpoints(before, after2); len(items) != 0 {
		t.Fatalf("字段说明变化不应产生 diff: %+v", items)
	}
	// 类型写法变化但 type_key 相同（List\<Long\> vs List<Long>）→ 不算类型变更
	before3 := newEp("GET", "/y")
	before3.RequestFields = []Field{rf("ids", `List\<Long\>`, RequiredNo, PosQuery, "")}
	after3 := newEp("GET", "/y")
	after3.RequestFields = []Field{rf("ids", "List<Long>", RequiredNo, PosQuery, "")}
	if SemanticHash(before3) != SemanticHash(after3) {
		t.Fatal("type_key 归一后相同 → semantic_hash 相同")
	}
	if items := DiffEndpoints(before3, after3); len(items) != 0 {
		t.Fatalf("转义写法差异不产生 diff: %+v", items)
	}
}

// TestDiffPlaceholderRenameCosmetic 占位符改名 {id}→{paperId}：客户端实际 URL 一字未变，
// 不产生 moved、也不产生 renamed（cosmetic）。
func TestDiffPlaceholderRenameCosmetic(t *testing.T) {
	before := newEp("DELETE", "/favorite/{id}")
	before.PathFields = []Field{rf("id", "Long", RequiredYes, PosPath, "")}
	after := newEp("DELETE", "/favorite/{paperId}")
	after.PathFields = []Field{rf("paperId", "Long", RequiredYes, PosPath, "")}

	if before.IdentityKey != after.IdentityKey {
		t.Fatal("占位符折叠后 identity_key 必须相同")
	}
	if items := DiffEndpoints(before, after); len(items) != 0 {
		t.Fatalf("占位符改名是 cosmetic，不记: %+v", items)
	}
}

// TestDiffBreakingRules §7 三态规则逐条。
func TestDiffBreakingRules(t *testing.T) {
	base := func() *Endpoint {
		e := newEp("POST", "/api/v1/sub")
		e.RequestFields = []Field{
			rf("topCategory", "String", RequiredNo, PosBody, ""),
			rf("keywords", "String", RequiredNo, PosBody, "a,b"),
		}
		e.ResponseFields = []Field{
			rf("message", "String", RequiredUnknown, "", ""),
			rf("data", "String", RequiredUnknown, "", ""),
		}
		return e
	}

	// ① 新增必填请求参数 → 自动破坏
	a := base()
	b := base()
	b.RequestFields = append(b.RequestFields, rf("source", "String", RequiredYes, PosBody, ""))
	it := mustItem(t, DiffEndpoints(a, b), SectionRequestBody, "source", OpAdded)
	if !it.Breaking || it.Rule != "required-request-parameter-added" {
		t.Fatalf("新增必填请求参数应自动破坏: %+v", it)
	}
	if AggregateBreaking([]DiffItem{*it}) != BreakingYes {
		t.Fatal("聚合应为 1")
	}

	// 新增可选请求参数 → 非破坏
	c := base()
	c.RequestFields = append(c.RequestFields, rf("source", "String", RequiredNo, PosBody, ""))
	it = mustItem(t, DiffEndpoints(a, c), SectionRequestBody, "source", OpAdded)
	if it.Breaking || it.NeedsConfirm {
		t.Fatalf("新增可选参数应非破坏: %+v", it)
	}

	// ② 必填 否→是 → 自动破坏（request-property-became-required）
	d := base()
	d.RequestFields[0].Required = RequiredYes
	it = mustItem(t, DiffEndpoints(a, d), SectionRequestBody, "topCategory", OpUpdated)
	if !it.Breaking || it.Rule != "request-property-became-required" {
		t.Fatalf("必填翻转应自动破坏: %+v", it)
	}
	frag, ok := it.After.(FieldFragment)
	if !ok || frag.Name != "topCategory" || frag.Required != RequiredYes || frag.Type != "String" {
		t.Fatalf("after 应是能独立渲染的最小完整片段: %+v", it.After)
	}

	// ②' 默认值被清空 → 自动破坏
	e := base()
	e.RequestFields[1].Default = ""
	it = mustItem(t, DiffEndpoints(a, e), SectionRequestBody, "keywords", OpUpdated)
	if !it.Breaking || it.Rule != "request-parameter-default-removed" {
		t.Fatalf("默认值清空应自动破坏: %+v", it)
	}

	// ③ 响应字段删除 → 自动破坏；响应新增字段 → 非破坏
	f := base()
	f.ResponseFields = f.ResponseFields[:1] // 删 data
	it = mustItem(t, DiffEndpoints(a, f), SectionResponseBody, "data", OpRemoved)
	if !it.Breaking || it.Rule != "response-property-removed" {
		t.Fatalf("响应字段删除应自动破坏: %+v", it)
	}
	g := base()
	g.ResponseFields = append(g.ResponseFields, rf("extra", "String", RequiredUnknown, "", ""))
	it = mustItem(t, DiffEndpoints(a, g), SectionResponseBody, "extra", OpAdded)
	if it.Breaking || it.NeedsConfirm {
		t.Fatalf("响应新增字段应非破坏: %+v", it)
	}

	// ④ 请求参数删除 → 需人工确认（breaking=2，不放自动档避免确认闸空响）
	h := base()
	h.RequestFields = h.RequestFields[:1] // 删 keywords
	items := DiffEndpoints(a, h)
	it = mustItem(t, items, SectionRequestBody, "keywords", OpRemoved)
	if it.Breaking || !it.NeedsConfirm {
		t.Fatalf("请求参数删除应需人工确认: %+v", it)
	}
	if AggregateBreaking(items) != BreakingConfirm {
		t.Fatalf("聚合应为 2，got %d", AggregateBreaking(items))
	}

	// ⑤ 类型变更（按 type_key 归一后比）→ 自动破坏
	i2 := base()
	i2.RequestFields[0].TypeRaw = "Long"
	i2.RequestFields[0].TypeKey = TypeKey("Long")
	it = mustItem(t, DiffEndpoints(a, i2), SectionRequestBody, "topCategory", OpUpdated)
	if !it.Breaking || it.Rule != "request-property-type-changed" {
		t.Fatalf("类型变更应自动破坏: %+v", it)
	}

	// ⑥ 路径静态段变更 / method 变更 → 落一条 moved，不拆成 removed+added
	j := base()
	j.PathRaw = "/api/v1/sub2"
	j.IdentityKey = IdentityKey("POST", "/api/v1/sub2")
	items = DiffEndpoints(a, j)
	if len(items) != 1 {
		t.Fatalf("路径改名应恰好一条 moved: %+v", items)
	}
	if items[0].Op != OpMoved || items[0].Section != SectionMeta || !items[0].Breaking || items[0].Rule != "path-changed" {
		t.Fatalf("moved 载荷不对: %+v", items[0])
	}
	if items[0].Before != "POST /api/v1/sub" || items[0].After != "POST /api/v1/sub2" {
		t.Fatalf("moved before/after 应为 METHOD path 串: %+v", items[0])
	}
	k := base()
	k.Method = "PUT"
	k.IdentityKey = IdentityKey("PUT", "/api/v1/sub")
	items = DiffEndpoints(a, k)
	if len(items) != 1 || items[0].Rule != "method-changed" {
		t.Fatalf("method 改名应落一条 method-changed moved: %+v", items)
	}

	// ⑦ Content-Type 变更（json ↔ multipart）→ 自动破坏
	l := base()
	l.ContentType = "multipart/form-data"
	it = mustItem(t, DiffEndpoints(a, l), SectionMeta, "content_type", OpUpdated)
	if !it.Breaking || it.Rule != "request-content-type-changed" {
		t.Fatalf("Content-Type 变更应自动破坏: %+v", it)
	}
	// 空 → 有值 = 文档补全，非破坏
	m := base()
	m.ContentType = ""
	it = mustItem(t, DiffEndpoints(m, a), SectionMeta, "content_type", OpUpdated)
	if it.Breaking {
		t.Fatalf("Content-Type 补全不应判破坏: %+v", it)
	}

	// ⑧ 认证「外部可调用 → 内部Token」→ 自动破坏；其他方向变化 → 需人工确认
	n := base()
	n.Auth = AuthInternal
	it = mustItem(t, DiffEndpoints(a, n), SectionMeta, "auth", OpUpdated)
	if !it.Breaking || it.Rule != "request-authentication-internalized" {
		t.Fatalf("转内部Token应自动破坏: %+v", it)
	}
	o := base()
	o.Auth = AuthNoLogin
	it = mustItem(t, DiffEndpoints(a, o), SectionMeta, "auth", OpUpdated)
	if it.Breaking || !it.NeedsConfirm {
		t.Fatalf("鉴权其他方向变化应需人工确认: %+v", it)
	}
}

// TestDiffMovedAndRenamed 参数位置迁移与字段改名。
func TestDiffMovedAndRenamed(t *testing.T) {
	before := newEp("POST", "/x")
	before.RequestFields = []Field{rf("keyword", "String", RequiredNo, PosQuery, ""), rf("title", "String", RequiredNo, PosBody, "")}
	after := newEp("POST", "/x")
	after.RequestFields = []Field{rf("keyword", "String", RequiredNo, PosBody, ""), rf("paperTitle", "String", RequiredNo, PosBody, "")}

	items := DiffEndpoints(before, after)
	mv := findItem(items, SectionRequestQuery, "keyword", OpMoved)
	if mv == nil {
		t.Fatalf("Query→Body 应落一条 moved: %+v", items)
	}
	if !mv.Breaking || mv.Rule != "request-parameter-position-changed" {
		t.Fatalf("位置迁移应自动破坏: %+v", mv)
	}
	// 不能同时把 keyword 拆成 removed+added
	if findItem(items, SectionRequestQuery, "keyword", OpRemoved) != nil ||
		findItem(items, SectionRequestBody, "keyword", OpAdded) != nil {
		t.Fatalf("moved 不应拆成 removed+added: %+v", items)
	}
	// title → paperTitle（同型 removed+added）→ renamed，需人工确认
	rn := findItem(items, SectionRequestBody, "title", OpRenamed)
	if rn == nil {
		t.Fatalf("同型改名应落 renamed: %+v", items)
	}
	if rn.Breaking || !rn.NeedsConfirm || rn.Rule != "request-property-removed+added" {
		t.Fatalf("字段改名应需人工确认: %+v", rn)
	}
	if AggregateBreaking(items) != BreakingYes { // moved 是自动破坏
		t.Fatal("聚合应为 1")
	}
}

// TestDiffSchemaSet 命名结构集合参与 semantic_hash 与 diff。
func TestDiffSchemaSet(t *testing.T) {
	before := newEp("GET", "/s")
	before.Schemas = []SchemaDef{{Name: "UserVO", Fields: []Field{rf("id", "String", RequiredUnknown, "", "")}}}
	before.Schemas[0].BodyHash = schemaBodyHash(before.Schemas[0].Fields)
	after := newEp("GET", "/s")
	after.Schemas = []SchemaDef{{Name: "UserVO", Fields: []Field{
		rf("id", "String", RequiredUnknown, "", ""),
		rf("nickname", "String", RequiredUnknown, "", ""),
	}}}
	after.Schemas[0].BodyHash = schemaBodyHash(after.Schemas[0].Fields)

	if SemanticHash(before) == SemanticHash(after) {
		t.Fatal("命名结构变化应改变 semantic_hash")
	}
	items := DiffEndpoints(before, after)
	it := findItem(items, SectionResponseBody, "UserVO.nickname", OpAdded)
	if it == nil {
		t.Fatalf("结构新增字段应落 UserVO.nickname: %+v", items)
	}
	// 整个结构被删 → 自动破坏
	gone := newEp("GET", "/s")
	items = DiffEndpoints(before, gone)
	it = findItem(items, SectionResponseBody, "UserVO", OpRemoved)
	if it == nil || !it.Breaking {
		t.Fatalf("命名结构删除应自动破坏: %+v", items)
	}
}

// TestBreakingOverride 人工覆盖：声明 false 而启发式判破坏 → 拒收；声明 true 而启发式非破坏 → 接受。
func TestBreakingOverride(t *testing.T) {
	before := newEp("POST", "/x")
	before.RequestFields = []Field{rf("a", "String", RequiredNo, PosBody, "")}
	after := newEp("POST", "/x")
	after.RequestFields = []Field{rf("a", "String", RequiredYes, PosBody, "")}
	items := DiffEndpoints(before, after)

	if ValidateBreakingOverride(0, items) {
		t.Fatal("声明 breaking:false 而启发式判破坏 → 必须拒收（E-BREAKING-DENIED 由调用方落）")
	}
	nonBreaking := DiffEndpoints(before, before)
	if !ValidateBreakingOverride(0, nonBreaking) {
		t.Fatal("声明 false 且确实非破坏 → 接受")
	}
	if !ValidateBreakingOverride(1, nonBreaking) {
		t.Fatal("声明 true 而启发式非破坏 → 接受（人比规则懂业务）")
	}
	if !ValidateBreakingOverride(-1, items) {
		t.Fatal("未声明 breaking → 不由该闸拦截")
	}
}

// newPageItemEps 构造 10-21 事故形态：同名结构 FavoriteVO 两套字段（item 与分页），
// before 侧是库里被同名字段压成一行的分页表，after 侧是解析出的同名两条（ambiguous）。
func collisionEps(ambiguous bool) (*Endpoint, *Endpoint) {
	before := newEp("GET", "/api/v1.0/user/favorite")
	paging := SchemaDef{Name: "FavoriteVO", Ambiguous: ambiguous, Fields: []Field{
		rf("records", `List\<FavoriteVO\>`, RequiredUnknown, "", ""),
		rf("total", "long", RequiredUnknown, "", ""),
		rf("pages", "long", RequiredUnknown, "", ""),
	}}
	paging.BodyHash = schemaBodyHash(paging.Fields)
	before.Schemas = []SchemaDef{paging}

	after := newEp("GET", "/api/v1.0/user/favorite")
	item := SchemaDef{Name: "FavoriteVO", Ambiguous: ambiguous, Fields: []Field{
		rf("id", "String", RequiredUnknown, "", ""),
		rf("paperId", "String", RequiredUnknown, "", ""),
	}}
	item.BodyHash = schemaBodyHash(item.Fields)
	after.Schemas = []SchemaDef{item, paging}
	return before, after
}

// TestDiffAmbiguousSchemaDowngraded 命名结构歧义（同名不同构）产生的差异一律降级：
// Breaking=false + NeedsConfirm=true + rule=struct-ambiguous，且不得成为「作者违约」的证据（D2）。
// 变异依据：删掉 diffSchemas 里的 ambiguousName 降级分支，本用例红（假破坏重新顶掉整批导入）。
func TestDiffAmbiguousSchemaDowngraded(t *testing.T) {
	before, after := collisionEps(true)
	items := DiffEndpoints(before, after)
	if len(items) == 0 {
		t.Fatal("歧义差异仍要可见（降级 ≠ 静默）：事件载荷必须带着这些项让人确认")
	}
	for _, it := range items {
		if it.Rule != RuleStructAmbiguous {
			t.Fatalf("涉及 ambiguous 结构的项必须 rule=%s: %+v", RuleStructAmbiguous, it)
		}
		if it.Breaking {
			t.Fatalf("程序自己都认不出该用哪套定义，不得自动判破坏: %+v", it)
		}
		if !it.NeedsConfirm {
			t.Fatalf("降级项必须 NeedsConfirm（等同破坏处理，走确认闸）: %+v", it)
		}
	}
	if got := AggregateBreaking(items); got != BreakingConfirm {
		t.Fatalf("聚合 = %d, want 2（需人工确认）", got)
	}
	if !ValidateBreakingOverride(0, items) {
		t.Fatal("声明 breaking:false 时不得因歧义噪声判作者违约（E-BREAKING-DENIED 必须放行）")
	}
	if n := len(AuthoritativeItems(items)); n != 0 {
		t.Fatalf("AuthoritativeItems 应剔光全部降级项, got %d: %+v", n, items)
	}
	// 声明「未变更」的交叉校验同样只看权威项（E-DIFF-MISMATCH-UNCHANGED 的判据）
	if len(AuthoritativeItems(items)) != 0 {
		t.Fatal("歧义项不得进入「未变更却算出差异」的裁决")
	}

	// 整个结构名只在 before 存在（分页表被删）：删除是 ambiguous 名 → 一样降级
	goneAfter := newEp("GET", "/api/v1.0/user/favorite")
	goneAfter.Schemas = []SchemaDef{{Name: "SomethingElse", Fields: []Field{rf("x", "String", RequiredUnknown, "", "")}}}
	goneAfter.Schemas[0].BodyHash = schemaBodyHash(goneAfter.Schemas[0].Fields)
	sub := DiffEndpoints(before, goneAfter)
	var removed *DiffItem
	for i := range sub {
		if sub[i].Key == "FavoriteVO" && sub[i].Op == OpRemoved {
			removed = &sub[i]
		}
	}
	if removed == nil || removed.Breaking || !removed.NeedsConfirm || removed.Rule != RuleStructAmbiguous {
		t.Fatalf("ambiguous 结构的整名删除必须降级: %+v", removed)
	}
}

// TestDiffRealSchemaBreakingNotDowngraded AC5 防退化：降级只针对 ambiguous。
// 结构无歧义时，删响应字段 / 删整个命名结构必须照旧自动判破坏，breaking:false 声明照旧被拒。
// 变异依据：若把降级条件写成「所有 response 结构差异都降级」（把闸门修哑），本用例红。
func TestDiffRealSchemaBreakingNotDowngraded(t *testing.T) {
	before, after := collisionEps(false) // 同名但都不标 ambiguous → 真实字段变化
	items := DiffEndpoints(before, after)
	removed := findItem(items, SectionResponseBody, "FavoriteVO.records", OpRemoved)
	if removed == nil {
		t.Fatalf("无歧义时必须照常算出字段删除: %+v", items)
	}
	if !removed.Breaking || removed.Rule != "response-property-removed" {
		t.Fatalf("无歧义的响应字段删除必须自动破坏: %+v", removed)
	}
	if AggregateBreaking(items) != BreakingYes {
		t.Fatalf("聚合 = %d, want 1", AggregateBreaking(items))
	}
	if ValidateBreakingOverride(0, items) {
		t.Fatalf("声明 breaking:false 而程序判真破坏 → 仍必须拒收（E-BREAKING-DENIED）: %+v", items)
	}

	// 整个无歧义结构被删 → response-schema-removed 仍是自动破坏
	goneAfter := newEp("GET", "/api/v1.0/user/favorite")
	gone := DiffEndpoints(before, goneAfter)
	it := findItem(gone, SectionResponseBody, "FavoriteVO", OpRemoved)
	if it == nil || !it.Breaking || it.Rule != "response-schema-removed" {
		t.Fatalf("无歧义的结构删除必须自动破坏: %+v", gone)
	}
	if ValidateBreakingOverride(0, gone) {
		t.Fatal("结构删除 + breaking:false 声明必须仍被拒收")
	}
	// 响应字段删除（非结构域）也不受本次降级影响
	b2 := newEp("POST", "/y")
	b2.ResponseFields = []Field{rf("message", "String", RequiredUnknown, "", ""), rf("data", "String", RequiredUnknown, "", "")}
	a2 := newEp("POST", "/y")
	a2.ResponseFields = []Field{rf("message", "String", RequiredUnknown, "", "")}
	realItems := DiffEndpoints(b2, a2)
	if ValidateBreakingOverride(0, realItems) {
		t.Fatalf("删响应字段仍必须判破坏、breaking:false 声明仍必须被拒收: %+v", realItems)
	}
	if n := len(AuthoritativeItems(realItems)); n != len(realItems) {
		t.Fatalf("无歧义的差异不得被降级剔除: %+v", realItems)
	}
}

func mustItem(t *testing.T, items []DiffItem, section, key, op string) *DiffItem {
	t.Helper()
	it := findItem(items, section, key, op)
	if it == nil {
		t.Fatalf("缺 diff 条目 %s/%s/%s: %+v", section, key, op, items)
	}
	return it
}
