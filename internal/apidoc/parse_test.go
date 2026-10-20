package apidoc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 仓库测试约定：真文件 + 手写 t.Fatalf，无断言库。
// golden fixture：testdata/ 下两份样例是 tracked 文件（.doc/ 被 .git/info/exclude 忽略，
// 不复制的话 CI 和协作者机器上 fixture 根本不存在），不可就地修改；修改需同步更新本文件断言。

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func parseInit(t *testing.T, text string) *Document {
	t.Helper()
	doc := Parse(text, Options{Mode: ModeInit})
	if doc.HasErrors() {
		for _, is := range doc.Errors() {
			t.Logf("E %s line=%d snippet=%s hint=%s", is.Code, is.Line, is.Snippet, is.Hint)
		}
		t.Fatalf("init 模式解析 golden fixture 不应产生 E-* 阻断，共 %d 条", len(doc.Errors()))
	}
	return doc
}

// ===================== golden fixture：用户服务（20 条）=====================

func TestParseUserServiceGolden(t *testing.T) {
	doc := parseInit(t, readFixture(t, "user-service.md"))

	// AC5：条目数 20
	if len(doc.Endpoints) != 20 {
		t.Fatalf("用户服务接口数 = %d, want 20", len(doc.Endpoints))
	}
	// 领域：12 个接口章节；概述/错误响应/接口汇总不产生领域（结构规则，非标题黑名单）
	if len(doc.Domains) != 12 {
		names := []string{}
		for _, d := range doc.Domains {
			names = append(names, d.Name)
		}
		t.Fatalf("领域数 = %d (%v), want 12", len(doc.Domains), names)
	}
	for _, banned := range []string{"概述", "错误响应", "接口汇总"} {
		for _, d := range doc.Domains {
			if d.Name == banned {
				t.Fatalf("非接口章节 %q 不应产生领域", banned)
			}
		}
	}
	wantDomains := []string{"Token状态", "用户注册", "用户登录", "获取当前用户", "按用户名查询用户",
		"密码修改", "用户信息修改", "订阅源管理", "收藏夹", "暂缓池", "下载记录", "操作留痕"}
	for i, want := range wantDomains {
		if doc.Domains[i].Name != want {
			t.Fatalf("领域[%d] = %q, want %q", i, doc.Domains[i].Name, want)
		}
	}

	// GET /api/v1.0/user 与 base_path 逐字相等：正常入库而非空串（绝不剥离 base_path）
	ep := doc.FindEndpoint("GET|/api/v1.0/user")
	if ep == nil {
		t.Fatal("GET /api/v1.0/user 必须正常入库（identity_key 不含空串）")
	}
	if ep.PathRaw != "/api/v1.0/user" || ep.Name != "根据用户名查询用户信息" {
		t.Fatalf("GET /api/v1.0/user 入库内容不对: %+v", ep)
	}
	if doc.BasePath != "/api/v1.0/user" {
		t.Fatalf("base_path 兜底（概述「**基础路径**」）= %q", doc.BasePath)
	}
	// 4 组同 path 多 method 不被合并（8 个 key 全部独立存在）
	for _, key := range []string{
		"GET|/api/v1.0/user/subscription", "POST|/api/v1.0/user/subscription",
		"PUT|/api/v1.0/user/subscription/{}", "DELETE|/api/v1.0/user/subscription/{}",
		"GET|/api/v1.0/user/favorite", "POST|/api/v1.0/user/favorite",
		"GET|/api/v1.0/user/deferred", "POST|/api/v1.0/user/deferred",
	} {
		if doc.FindEndpoint(key) == nil {
			t.Fatalf("同 path 多 method 的 key 缺失: %s", key)
		}
	}
	// 占位符折叠但 path_raw 保留变量名
	del := doc.FindEndpoint("DELETE|/api/v1.0/user/favorite/{}")
	if del == nil || del.PathRaw != "/api/v1.0/user/favorite/{paperId}" {
		t.Fatalf("占位符折叠/path_raw 保真失败: %+v", del)
	}

	// C1 汇总闭合：20 行，双向差集为 0
	if !doc.Summary.Present || !doc.Summary.Closed {
		t.Fatalf("C1 汇总闭合失败: bodyOnly=%v summaryOnly=%v", doc.Summary.BodyOnly, doc.Summary.SummaryOnly)
	}
	if len(doc.Summary.Rows) != 20 || len(doc.Summary.BodyOnly) != 0 || len(doc.Summary.SummaryOnly) != 0 {
		t.Fatalf("汇总自检 = %+v", doc.Summary)
	}

	// 同名 VO 冲突：IPage 两处定义不同构 → ambiguous + W-STRUCT-DIVERGED
	var ipages []*SchemaDef
	for i := range doc.Schemas {
		if doc.Schemas[i].Name == "IPage" {
			ipages = append(ipages, &doc.Schemas[i])
		}
	}
	if len(ipages) != 2 {
		t.Fatalf("IPage 定义数 = %d, want 2", len(ipages))
	}
	for _, s := range ipages {
		if !s.Ambiguous {
			t.Fatalf("IPage 同名不同构必须标记 ambiguous: %+v", s)
		}
		if s.OwnerScope == "" || s.OwnerIdx < 0 {
			t.Fatalf("IPage 的 owner_scope 必须是宿主接口: %+v", s)
		}
	}
	if !doc.HasCode(WarnStructDiverged) {
		t.Fatal("缺 W-STRUCT-DIVERGED")
	}

	// 散文归类：「无。」/「无请求体。」/「无请求体参数，…」= 明确无参数，不是信息缺失
	sub := doc.FindEndpoint("GET|/api/v1.0/user/subscription")
	if sub.ReqParamsState != SectionEmpty || len(sub.Unresolved) > 0 {
		t.Fatalf("「无。」应判 SectionEmpty 且不进 Unresolved: state=%s unresolved=%v", sub.ReqParamsState, sub.Unresolved)
	}
	dl := doc.FindEndpoint("POST|/api/v1.0/user/download/{}")
	if dl == nil || dl.ReqParamsState != SectionEmpty {
		t.Fatalf("「无请求体。」应判 SectionEmpty: %+v", dl)
	}
	token := doc.FindEndpoint("GET|/api/v1.0/user/token/status")
	if token.ReqParamsState != SectionEmpty {
		t.Fatalf("「无请求体参数，需在请求头中携带认证Token。」应判 SectionEmpty")
	}

	// 交叉引用「同新增订阅。」：init 模式降级 W-CROSS-REF + partial + Unresolved 槽位
	put := doc.FindEndpoint("PUT|/api/v1.0/user/subscription/{}")
	if put == nil {
		t.Fatal("8.3 修改订阅必须入库")
	}
	if put.ParseState != ParsePartial || put.ReqParamsState != SectionCrossRef {
		t.Fatalf("交叉引用条目应 partial: state=%s reqState=%s", put.ParseState, put.ReqParamsState)
	}
	found := false
	for _, u := range put.Unresolved {
		if u == "request_params" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Unresolved 应记录 request_params 槽位: %v", put.Unresolved)
	}
	if !doc.HasCode(WarnCrossRef) || doc.HasCode(ErrCrossRef) {
		t.Fatal("init 模式 E-CROSS-REF 应降级为 W-CROSS-REF")
	}
	// 路径参数表照常解析（id|Long|是）
	if len(put.PathFields) != 1 || put.PathFields[0].Name != "id" ||
		put.PathFields[0].Position != PosPath || put.PathFields[0].Required != RequiredYes {
		t.Fatalf("8.3 路径参数解析失败: %+v", put.PathFields)
	}

	// init 放宽：缺「位置」列按 method 推断并逐条打 W-POS-INFERRED
	if !doc.HasCode(WarnPosInferred) {
		t.Fatal("缺 W-POS-INFERRED")
	}
	reg := doc.FindEndpoint("POST|/api/v1.0/user/register")
	if len(reg.RequestFields) != 3 {
		t.Fatalf("2.1 请求参数 = %+v", reg.RequestFields)
	}
	for _, f := range reg.RequestFields {
		if f.Position != PosBody {
			t.Fatalf("POST 缺位置列应推断 Body: %+v", f)
		}
	}
	byName := doc.FindEndpoint("GET|/api/v1.0/user")
	if len(byName.RequestFields) != 1 || byName.RequestFields[0].Position != PosQuery ||
		byName.RequestFields[0].Required != RequiredYes {
		t.Fatalf("5.1 带位置列的表应按列取值: %+v", byName.RequestFields)
	}
	// 必填第三态：返回参数表没有必填列 → required=-1（未知），不是「否」
	for _, f := range token.ResponseFields {
		if f.Required != RequiredUnknown {
			t.Fatalf("返回参数无必填列应为 -1: %+v", f)
		}
	}
	// 类型双字段：type_raw 保真 + type_key 归一
	var listField *Field
	for i := range sub.ResponseFields {
		if sub.ResponseFields[i].Name == "data" {
			listField = &sub.ResponseFields[i]
		}
	}
	if listField == nil || listField.TypeRaw != `List\<SubscriptionVO\>` || listField.TypeKey != "List<Subscriptionvo>" {
		t.Fatalf("类型双字段失败: %+v", listField)
	}

	// raw_markdown 原文切片保真兜底
	if !strings.Contains(sub.RawMarkdown, "### 8.1 获取当前用户订阅列表") {
		t.Fatalf("raw_markdown 缺失原文: %.80s", sub.RawMarkdown)
	}
	// curl 校验源：8.1 的 curl 与 URL 行一致，不应有 W-CURL-MISMATCH
	for _, is := range doc.Warnings() {
		if is.Code == WarnCURLMismatch && is.Line > sub.Line && is.Line < sub.EndLine {
			t.Fatalf("8.1 curl 不应失配: %+v", is)
		}
	}
	// 所有 Issue 行号必给
	for _, is := range doc.Issues {
		if is.Line <= 0 {
			t.Fatalf("Issue 行号必给: %+v", is)
		}
	}
}

// ===================== golden fixture：论文服务（11 条）=====================

func TestParsePaperServiceGolden(t *testing.T) {
	doc := parseInit(t, readFixture(t, "paper-service.md"))

	// AC5：条目数 11（内部 RPC 不跳过，否则计数对不上）
	if len(doc.Endpoints) != 11 {
		t.Fatalf("论文服务接口数 = %d, want 11", len(doc.Endpoints))
	}
	if len(doc.Domains) != 7 {
		t.Fatalf("领域数 = %d, want 7（概述/PaperVO 结构说明/错误响应/接口汇总不产生领域）", len(doc.Domains))
	}
	for _, d := range doc.Domains {
		switch d.Name {
		case "概述", "错误响应", "接口汇总", "PaperVO 结构说明":
			t.Fatalf("非接口章节 %q 不应产生领域", d.Name)
		}
	}

	// /arxiv/save 与 /arxiv/saveBatch 是两条（path 不做大小写归一，也不做前缀合并）
	save := doc.FindEndpoint("POST|/api/v1.0/paper/rpc/arxiv/save")
	batch := doc.FindEndpoint("POST|/api/v1.0/paper/rpc/arxiv/saveBatch")
	if save == nil || batch == nil || save == batch {
		t.Fatalf("save 与 saveBatch 必须是两条独立接口: save=%v batch=%v", save != nil, batch != nil)
	}

	// 内部 RPC 章节：领域名剥「接口（内部）」→「RPC 论文写入」+ internal；接口入库且 visibility=internal
	rpc := doc.FindEndpoint("POST|/api/v1.0/paper/rpc/arxiv/save")
	dom := doc.DomainOf(rpc)
	if dom == nil || dom.Name != "RPC 论文写入" || !dom.Internal {
		t.Fatalf("RPC 领域归一失败: %+v", dom)
	}
	if doc.VisibilityOf(rpc) != "internal" || doc.VisibilityOf(batch) != "internal" {
		t.Fatal("内部 RPC 接口必须 visibility=internal")
	}
	if doc.VisibilityOf(doc.FindEndpoint("POST|/api/v1.0/paper/retrieval")) != "public" {
		t.Fatal("普通接口必须 visibility=public")
	}
	// 章级 preamble 下渗：认证方式（X-Internal-Token）→ 7.1/7.2 继承 内部Token
	if rpc.Auth != AuthInternal || !rpc.AuthInherited {
		t.Fatalf("章级 preamble 认证未下渗: auth=%q inherited=%v", rpc.Auth, rpc.AuthInherited)
	}
	if !strings.Contains(dom.NotesRaw, "X-Internal-Token") || !strings.Contains(dom.NotesRaw, "请重新登录") {
		t.Fatalf("章级 preamble（含孤儿 401 JSON 块）应保留在 notes_raw: %.120s", dom.NotesRaw)
	}
	if !strings.Contains(rpc.Header, "X-Internal-Token") {
		t.Fatalf("**Header** bullet 未解析: %q", rpc.Header)
	}

	// fence 区间最先跑：multipart 原始报文块里的 Content-Type: 不污染元数据
	parse := doc.FindEndpoint("POST|/api/v1.0/paper/parse")
	if parse.ContentType != "multipart/form-data" {
		t.Fatalf("Content-Type 被围栏内容污染: %q", parse.ContentType)
	}
	if len(parse.RequestFields) != 1 || parse.RequestFields[0].Name != "pdfFile" ||
		parse.RequestFields[0].TypeKey != "Multipartfile" {
		t.Fatalf("multipart 接口参数被污染: %+v", parse.RequestFields)
	}
	// 无语言标记的 multipart 报文块 → multipart_raw 示例 + W-EXAMPLE-JSON（属正常）
	var multipartSeen bool
	for _, ex := range parse.Examples {
		if ex.Kind == ExampleMultipart && strings.Contains(ex.Content, "Content-Disposition:") {
			multipartSeen = true
		}
	}
	if !multipartSeen {
		t.Fatalf("multipart 原始报文块应存为 multipart_raw 示例: %+v", parse.Examples)
	}
	if !doc.HasCode(WarnExampleJSON) {
		t.Fatal("multipart/请求行示例应产生 W-EXAMPLE-JSON")
	}

	// #### 括号变体前缀匹配：请求参数（ArxivPaperDTO）/ 返回参数（ApiResponse\<Boolean\>）
	if len(rpc.RequestFields) != 11 || rpc.RequestFields[0].Name != "arxivId" {
		t.Fatalf("「请求参数（ArxivPaperDTO）」应前缀匹配 request_params: %+v", rpc.RequestFields)
	}
	if len(rpc.ResponseFields) != 4 {
		t.Fatalf("「返回参数（ApiResponse\\<Boolean\\>）」应前缀匹配 response_params: %+v", rpc.ResponseFields)
	}

	// 7.2 的两处交叉引用（元素结构见 7.1 / 同 7.1）→ partial
	if batch.ParseState != ParsePartial || batch.ReqParamsState != SectionCrossRef || batch.RespParamsState != SectionCrossRef {
		t.Fatalf("7.2 交叉引用应 partial: state=%s req=%s resp=%s", batch.ParseState, batch.ReqParamsState, batch.RespParamsState)
	}

	// C1 汇总闭合：11 行，双向差集为 0
	if !doc.Summary.Present || !doc.Summary.Closed || len(doc.Summary.Rows) != 11 {
		t.Fatalf("C1 汇总闭合失败: %+v", doc.Summary)
	}

	// 文档级命名结构：## 八、PaperVO 结构说明 → owner_scope='' 的 PaperVO（8 字段）
	var paperVO *SchemaDef
	for i := range doc.Schemas {
		if doc.Schemas[i].Name == "PaperVO" {
			paperVO = &doc.Schemas[i]
		}
	}
	if paperVO == nil || paperVO.OwnerScope != "" || paperVO.OwnerIdx != -1 || len(paperVO.Fields) != 8 {
		t.Fatalf("文档级 PaperVO 结构缺失或不对: %+v", paperVO)
	}
	// 同名 VO 冲突：PaperBriefVO 两处定义不同构 → ambiguous
	var briefs []*SchemaDef
	for i := range doc.Schemas {
		if doc.Schemas[i].Name == "PaperBriefVO" {
			briefs = append(briefs, &doc.Schemas[i])
		}
	}
	if len(briefs) != 2 {
		t.Fatalf("PaperBriefVO 定义数 = %d, want 2", len(briefs))
	}
	for _, s := range briefs {
		if !s.Ambiguous {
			t.Fatalf("PaperBriefVO 必须标记 ambiguous: %+v", s)
		}
	}
	// 4.1 的嵌套命名结构链（Brief/Tags/PostCard/ImageSource）都归属该接口
	briefEp := doc.FindEndpoint("GET|/api/v1.0/paper/recommend/briefs")
	names := map[string]bool{}
	for _, s := range briefEp.Schemas {
		names[s.Name] = true
	}
	for _, want := range []string{"PaperBriefVO", "Brief", "Tags", "PostCard", "ImageSource"} {
		if !names[want] {
			t.Fatalf("4.1 缺命名结构 %s: %v", want, names)
		}
	}
	// curl 校验源：saveBatch 没有 curl 块也正常；6.2 的 curl 用实例值 cs，按 {} 段匹配不算失配
	for _, is := range doc.Warnings() {
		if is.Code == WarnCURLMismatch {
			t.Fatalf("golden 文档不应有 curl 失配: line=%d snippet=%s", is.Line, is.Snippet)
		}
	}
}

// ===================== 确定性 =====================

func TestParseDeterministic(t *testing.T) {
	for _, name := range []string{"user-service.md", "paper-service.md"} {
		text := readFixture(t, name)
		b1, err := json.Marshal(Parse(text, Options{Mode: ModeInit}))
		if err != nil {
			t.Fatalf("marshal 1: %v", err)
		}
		b2, err := json.Marshal(Parse(text, Options{Mode: ModeInit}))
		if err != nil {
			t.Fatalf("marshal 2: %v", err)
		}
		if string(b1) != string(b2) {
			t.Fatalf("%s 两次解析结果不一致（确定性被破坏）", name)
		}
		if len(b1) < 10000 {
			t.Fatalf("%s 序列化结果异常小: %d", name, len(b1))
		}
	}
}

// ===================== 规范模式（strict）=====================

// strictSpecDoc 一份完全合规的规范文档（meta 块 + 变更声明 + 位置列齐全），strict 解析应零 Issue。
func strictSpecDoc() string {
	return "# 用户服务接口变更文档\n\n" + fence3 + "apidoc-meta\n" +
		"spec: 1\nproject: paper-manager\nservice: 用户服务\nbase_path: /api/v1.0/user\n" +
		"scope: incremental\nchange_type: feature\nchange_title: 新增论文下载记录同步接口\n" +
		"change_note: 前端需要跨端展示下载历史，旧接口不变\nversion_tag: REQ-1024\n" + fence3 + "\n\n" +
		"## 一、订阅源管理\n\n" +
		"### 8.2 新增订阅\n\n为当前用户新增一条 Arxiv 订阅。\n\n" +
		"- **URL**：" + q + "POST /api/v1.0/user/subscription" + q + "\n" +
		"- **Content-Type**：" + q + "application/json" + q + "\n" +
		"- **认证**：需要登录\n" +
		"- **变更**：修改\n" +
		"- **变更点**：请求体新增可选字段 source\n" +
		"- **breaking**：false\n\n" +
		"#### 请求参数\n\n" +
		"| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n" +
		"|--------|------|------|------|--------|------|\n" +
		"| topCategory | String | 否 | Body | | Arxiv 顶级分类 |\n" +
		"| source | String | 否 | Body | | 来源渠道 |\n\n" +
		"#### 返回参数\n\n" +
		"| 参数名 | 类型 | 说明 |\n" +
		"|--------|------|------|\n" +
		"| message | String | 响应消息 |\n" +
		"| data | SubscriptionVO | 订阅信息 |\n\n" +
		"**SubscriptionVO 结构**：\n\n" +
		"| 字段 | 类型 | 说明 |\n" +
		"|------|------|------|\n" +
		"| id | String | 订阅ID |\n"
}

func TestParseStrictModeCleanDoc(t *testing.T) {
	doc := Parse(strictSpecDoc(), Options{})
	if doc.Mode != ModeIncremental {
		t.Fatalf("meta scope=incremental 应生效: %s", doc.Mode)
	}
	if len(doc.Issues) != 0 {
		t.Fatalf("合规规范文档应零 Issue: %+v", doc.Issues)
	}
	m := doc.Meta
	if m.Project != "paper-manager" || m.Service != "用户服务" || m.BasePath != "/api/v1.0/user" ||
		m.ChangeType != "feature" || m.ChangeTitle != "新增论文下载记录同步接口" ||
		m.ChangeNote != "前端需要跨端展示下载历史，旧接口不变" || m.VersionTag != "REQ-1024" || m.Spec != "1" {
		t.Fatalf("meta 解析不对: %+v", m)
	}
	if len(doc.Endpoints) != 1 {
		t.Fatalf("接口数 = %d", len(doc.Endpoints))
	}
	ep := doc.Endpoints[0]
	if ep.Seq != "8.2" || ep.Name != "新增订阅" || ep.Change != ChangeUpdated ||
		ep.ChangePoint != "请求体新增可选字段 source" || ep.BreakingDecl != 0 ||
		ep.ContentType != "application/json" || ep.Auth != AuthLogin ||
		ep.Description != "为当前用户新增一条 Arxiv 订阅。" {
		t.Fatalf("接口块解析不对: %+v", ep)
	}
	if ep.ParseState != ParseOK || len(ep.Unresolved) != 0 {
		t.Fatalf("齐全小节应 parse_state=ok: %s %v", ep.ParseState, ep.Unresolved)
	}
	if len(ep.RequestFields) != 2 || ep.RequestFields[1].Name != "source" || ep.RequestFields[1].Position != PosBody {
		t.Fatalf("请求参数解析不对: %+v", ep.RequestFields)
	}
	if len(ep.Schemas) != 1 || ep.Schemas[0].Name != "SubscriptionVO" || len(ep.Schemas[0].Fields) != 1 {
		t.Fatalf("命名结构解析不对: %+v", ep.Schemas)
	}
	if len(doc.Schemas) != 1 || doc.Schemas[0].OwnerScope != ep.IdentityKey {
		t.Fatalf("registry owner_scope 应为宿主接口: %+v", doc.Schemas)
	}
}

// TestParseBasePathMetaFlow 钉住 §13.17 修复：meta.base_path 必须流入 doc.BasePath
// 并参与 C3 前缀断言——meta-only 文档（无「**基础路径**」散文行）同样要能触发
// W-BASEPATH-ASSERT-FAIL，断言不能因为写法不同而被静默跳过（§2：base_path 只做断言）。
func TestParseBasePathMetaFlow(t *testing.T) {
	// meta-only：base_path 只在 meta 块里声明，文档含一条不以其开头的 URL → 必须出 W
	d := Parse(strings.Replace(strictSpecDoc(), "base_path: /api/v1.0/user", "base_path: /api/v9.9/other", 1), Options{})
	if d.BasePath != "/api/v9.9/other" {
		t.Fatalf("meta.base_path 应流入 doc.BasePath: %q", d.BasePath)
	}
	if !d.HasCode(WarnBasePathAssert) {
		t.Fatalf("URL 不以 meta.base_path 开头应报 %s: %+v", WarnBasePathAssert, d.Issues)
	}
	// meta 与 URL 逐字匹配 → 断言通过、零警告（strictSpecDoc 原始形态）
	ok := Parse(strictSpecDoc(), Options{})
	if ok.BasePath != "/api/v1.0/user" {
		t.Fatalf("doc.BasePath = %q, want meta 值 /api/v1.0/user", ok.BasePath)
	}
	if ok.HasCode(WarnBasePathAssert) {
		t.Fatalf("URL 全部以 base_path 开头不应触发断言警告: %+v", ok.Warnings())
	}
	// 散文「**基础路径**」兜底行与 meta 并存且冲突 → 以 meta 为准并出 W
	//（W 码表封闭，复用 W-UNPARSED-BLOCK，同「meta 键重复」先例）
	conflict := Parse(strings.Replace(strictSpecDoc(), "# 用户服务接口变更文档\n\n",
		"# 用户服务接口变更文档\n\n**基础路径**：`/prose/base`\n\n", 1), Options{})
	if conflict.BasePath != "/api/v1.0/user" {
		t.Fatalf("冲突时应以 meta.base_path 为准: %q", conflict.BasePath)
	}
	var sawConflict bool
	for _, is := range conflict.Warnings() {
		if is.Code == WarnUnparsedBlock && strings.Contains(is.Hint, "冲突") {
			sawConflict = true
		}
	}
	if !sawConflict {
		t.Fatalf("散文与 meta 的 base_path 冲突应出 W: %+v", conflict.Warnings())
	}
	// 散文与 meta 一致 → 不报冲突
	same := Parse(strings.Replace(strictSpecDoc(), "# 用户服务接口变更文档\n\n",
		"# 用户服务接口变更文档\n\n**基础路径**：`/api/v1.0/user`\n\n", 1), Options{})
	if same.BasePath != "/api/v1.0/user" {
		t.Fatalf("一致时 doc.BasePath = %q", same.BasePath)
	}
	for _, is := range same.Warnings() {
		if strings.Contains(is.Hint, "冲突") {
			t.Fatalf("散文与 meta 一致不应报冲突: %+v", is)
		}
	}
	// 无 meta 的散文文档：兜底行照旧生效（golden fixture 的行为不回归）
	proseOnly := Parse("# t\n\n**基础路径**：`/only/prose`\n\n## 一、x接口\n\n### 1.1 a\n\n- **URL**："+q+"GET /only/prose/a"+q+"\n",
		Options{Mode: ModeInit})
	if proseOnly.BasePath != "/only/prose" {
		t.Fatalf("散文兜底应生效: %q", proseOnly.BasePath)
	}
	if proseOnly.HasCode(WarnBasePathAssert) {
		t.Fatalf("URL 以散文 base_path 开头不应告警: %+v", proseOnly.Warnings())
	}
}

func TestParseStrictModeRejections(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		code string
	}{
		{"缺 meta 块", "## 一、x接口\n\n### 1.1 a\n\n- **URL**：" + q + "GET /a" + q + "\n", ErrSpecBlockMissing},
		{"scope full", strings.Replace(strictSpecDoc(), "scope: incremental", "scope: full", 1), ErrScopeFullUnsupported},
		{"未知 meta 键", strings.Replace(strictSpecDoc(), "version_tag: REQ-1024", "oops: 1", 1), ErrMetaUnknownKey},
		{"spec 版本", strings.Replace(strictSpecDoc(), "spec: 1", "spec: 2", 1), ErrSpecVersion},
		{"缺 change_type", strings.Replace(strictSpecDoc(), "change_type: feature\n", "", 1), ErrMetaFieldMissing},
		{"change_type 越界", strings.Replace(strictSpecDoc(), "change_type: feature", "change_type: chore", 1), ErrMetaEnum},
		{"半角冒号 URL", strings.Replace(strictSpecDoc(), "- **URL**："+q+"POST", "- **URL:** "+q+"POST", 1), ErrURLFormat},
		{"方法小写", strings.Replace(strictSpecDoc(), q+"POST /api", q+"post /api", 1), ErrMethodInvalid},
		{"方法越界", strings.Replace(strictSpecDoc(), q+"POST /api", q+"FETCH /api", 1), ErrMethodInvalid},
		{"路径不以 / 开头", strings.Replace(strictSpecDoc(), "POST /api/v1.0/user/subscription", "POST api/v1.0/user/subscription", 1), ErrURLFormat},
		{"缺变更行", strings.Replace(strictSpecDoc(), "- **变更**：修改\n", "", 1), ErrChangeMissing},
		{"变更越界", strings.Replace(strictSpecDoc(), "- **变更**：修改", "- **变更**：大改", 1), ErrChangeEnum},
		{"缺变更点", strings.Replace(strictSpecDoc(), "- **变更点**：请求体新增可选字段 source\n", "", 1), ErrChangePointMissing},
		{"缺位置列", strings.Replace(strictSpecDoc(),
			"| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n|--------|------|------|------|--------|------|\n| topCategory | String | 否 | Body | | Arxiv 顶级分类 |\n| source | String | 否 | Body | | 来源渠道 |",
			"| 参数名 | 类型 | 必填 | 说明 |\n|--------|------|------|------|\n| topCategory | String | 否 | Arxiv 顶级分类 |", 1), ErrPosColMissing},
		{"交叉引用", strings.Replace(strictSpecDoc(),
			"| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n|--------|------|------|------|--------|------|\n| topCategory | String | 否 | Body | | Arxiv 顶级分类 |\n| source | String | 否 | Body | | 来源渠道 |",
			"同新增订阅。", 1), ErrCrossRef},
		{"字典外的列", strings.Replace(strictSpecDoc(), "| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |", "| 参数名 | 类型 | 必填 | 位置 | 默认值 | 备注栏 |", 1), ErrParamColUnknown},
		{"参数名重复", strings.Replace(strictSpecDoc(), "| source | String | 否 | Body | | 来源渠道 |", "| topCategory | String | 否 | Body | | 重复 |", 1), ErrParamDup},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := Parse(c.doc, Options{})
			if !doc.HasCode(c.code) {
				codes := []string{}
				for _, is := range doc.Issues {
					codes = append(codes, is.Code)
				}
				t.Fatalf("应产生 %s，实际 Issue: %v", c.code, codes)
			}
			// 行号必给
			for _, is := range doc.Issues {
				if is.Line <= 0 {
					t.Fatalf("Issue 行号必给: %+v", is)
				}
			}
		})
	}
}

func TestParseStrictDuplicateAndNoDomain(t *testing.T) {
	// 同文档内 identity_key 重复
	dupDoc := strictSpecDoc() +
		"\n### 8.3 又一个新增订阅\n\n- **URL**：" + q + "POST /api/v1.0/user/subscription" + q + "\n- **变更**：新增\n- **变更点**：重复\n\n" +
		"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| a | String | 否 | Body | x |\n"
	doc := Parse(dupDoc, Options{})
	if !doc.HasCode(ErrEndpointDup) {
		t.Fatalf("同 key 重复应报 E-ENDPOINT-DUP: %+v", doc.Issues)
	}
	// 接口块不在任何 ## 之下
	noDomain := "# t\n\n" + fence3 + "apidoc-meta\nspec: 1\nproject: p\nscope: incremental\nchange_type: fix\nchange_title: t\nchange_note: n\n" + fence3 + "\n\n" +
		"### 1.1 孤儿接口\n\n- **URL**：" + q + "GET /x" + q + "\n- **变更**：新增\n- **变更点**：x\n"
	doc2 := Parse(noDomain, Options{})
	if !doc2.HasCode(ErrEndpointNoDomain) {
		t.Fatalf("无领域接口应报 E-ENDPOINT-NO-DOMAIN: %+v", doc2.Issues)
	}
}

func TestParseInitModeRelaxations(t *testing.T) {
	// 同一份缺位置列 + 交叉引用的文档：init 放宽为 W-POS-INFERRED / W-CROSS-REF + partial
	doc := strings.Replace(strictSpecDoc(), "scope: incremental", "scope: init", 1)
	doc = strings.Replace(doc, "- **变更**：修改\n- **变更点**：请求体新增可选字段 source\n- **breaking**：false\n", "", 1)
	doc = strings.Replace(doc,
		"| 参数名 | 类型 | 必填 | 位置 | 默认值 | 说明 |\n|--------|------|------|------|--------|------|\n| topCategory | String | 否 | Body | | Arxiv 顶级分类 |\n| source | String | 否 | Body | | 来源渠道 |",
		"同新增订阅。", 1)
	d := Parse(doc, Options{})
	if d.Mode != ModeInit {
		t.Fatalf("meta scope=init 应生效: %s", d.Mode)
	}
	if d.HasErrors() {
		t.Fatalf("init 模式不应阻断: %+v", d.Errors())
	}
	if !d.HasCode(WarnCrossRef) {
		t.Fatalf("init 降级警告缺失: %+v", d.Issues)
	}
	// 缺位置列的 W-POS-INFERRED 推断在 golden 测试里覆盖（TestParseUserServiceGolden）
	ep := d.Endpoints[0]
	if ep.ParseState != ParsePartial || ep.ReqParamsState != SectionCrossRef {
		t.Fatalf("交叉引用条目应 partial: %+v", ep)
	}
	if ep.Change != "" {
		t.Fatalf("init 允许缺变更行: %q", ep.Change)
	}
}

// ===================== 对抗用例 =====================

func TestParseHalfWidthColonBullet(t *testing.T) {
	doc := "# t\n\n## 一、测试接口\n\n### 1.1 半角冒号\n\n- **URL:** " + q + "GET /api/t/x" + q + "\n"
	for _, mode := range []string{ModeIncremental, ModeInit} {
		d := Parse(doc, Options{Mode: mode})
		if !d.HasCode(ErrURLFormat) {
			t.Fatalf("%s: 半角冒号 **URL:** 必须报 E-URL-FORMAT: %+v", mode, d.Issues)
		}
		if len(d.Endpoints) != 0 {
			t.Fatalf("%s: 半角冒号接口不能入库: %+v", mode, d.Endpoints)
		}
		if len(d.Unparsed) != 1 || !strings.Contains(d.Unparsed[0].RawMarkdown, "**URL:**") {
			t.Fatalf("%s: 未解析块应保留原文: %+v", mode, d.Unparsed)
		}
		if !d.HasCode(WarnEmptyDomain) {
			t.Fatalf("%s: 空领域应报 W-EMPTY-DOMAIN: %+v", mode, d.Issues)
		}
	}
}

func TestParseOrphanCodeBlock(t *testing.T) {
	doc := "# t\n\n" + fence3 + "json\n{\"stray\": true}\n" + fence3 + "\n\n## 一、测试接口\n\n### 1.1 x\n\n- **URL**：" + q + "GET /t/x" + q + "\n"
	d := Parse(doc, Options{Mode: ModeInit})
	if !d.HasCode(WarnUnparsedBlock) {
		t.Fatalf("孤儿代码块应报 W-UNPARSED-BLOCK: %+v", d.Issues)
	}
	var found bool
	for _, u := range d.Unparsed {
		if strings.Contains(u.RawMarkdown, `"stray": true`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("孤儿代码块原文应保留: %+v", d.Unparsed)
	}
	if len(d.Endpoints) != 1 {
		t.Fatalf("孤儿块不影响正常接口: %d", len(d.Endpoints))
	}
}

// TestParseFenceContentIsInert fence 区间必须最先跑（spec-draft §1/§12）：围栏内的 URL bullet、
// ###/#### 标题、参数表行都是「内容」不是「结构」。golden fixture 的围栏里恰好没有这类行
// （变异测试证实：去掉 inFence 标记两份 golden 依然全绿），此用例专门钉死该不变量——
// 否则 multipart 原始报文里的 Content-Type: 会污染元数据、块内表格会混入字段。
func TestParseFenceContentIsInert(t *testing.T) {
	doc := "# t\n\n" + fence3 + "apidoc-meta\nspec: 1\nproject: p\nscope: incremental\nchange_type: fix\nchange_title: t\nchange_note: n\n" + fence3 + "\n\n" +
		"## 一、真实接口\n\n" +
		"### 1.1 真接口\n\n- **URL**：" + q + "GET /t/real" + q + "\n- **变更**：新增\n- **变更点**：x\n\n" +
		"#### 请求示例\n\n" + fence3 + "\n" +
		"### 9.9 围栏里的假接口\n" +
		"- **URL**：" + q + "POST /t/fake" + q + "\n" +
		"#### 请求参数\n" +
		"| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| fake | String | 是 | Body | x |\n" +
		fence3 + "\n"
	d := Parse(doc, Options{})
	if d.HasErrors() {
		t.Fatalf("合规文档不应有阻断: %+v", d.Errors())
	}
	if len(d.Endpoints) != 1 || d.Endpoints[0].IdentityKey != "GET|/t/real" {
		t.Fatalf("围栏内的 ### 与 URL bullet 不得产生接口: %+v", d.Endpoints)
	}
	if d.FindEndpoint("POST|/t/fake") != nil {
		t.Fatal("围栏内的假接口混入了 Endpoints")
	}
	if len(d.Domains) != 1 || d.Domains[0].Name != "真实" { // 剥序号 + 剥尾部「接口」
		t.Fatalf("围栏内的标题不得改变领域结构: %+v", d.Domains)
	}
	real := d.Endpoints[0]
	if len(real.RequestFields) != 0 {
		t.Fatalf("围栏内的参数表行不得混入字段: %+v", real.RequestFields)
	}
	if len(real.Examples) != 1 || !strings.Contains(real.Examples[0].Content, "9.9 围栏里的假接口") {
		t.Fatalf("围栏内容应作为示例原文保真存储: %+v", real.Examples)
	}
}

func TestNormalizeDomainName(t *testing.T) {
	cases := []struct {
		raw      string
		name     string
		internal bool
	}{
		{"一、Token状态接口", "Token状态", false},
		{"七、RPC 论文写入接口（内部）", "RPC 论文写入", true},
		{"十二、操作留痕接口", "操作留痕", false},
		{"八、订阅源管理接口", "订阅源管理", false},
		{"1. 数字序号接口", "数字序号", false},
		{"接口", "接口", false}, // 剥后为空则保留原文
		{"八、PaperVO 结构说明", "PaperVO 结构说明", false},
		{"九、错误  响应", "错误 响应", false}, // 剥序号 + 空白折叠
	}
	for _, c := range cases {
		name, internal := NormalizeDomainName(c.raw)
		if name != c.name || internal != c.internal {
			t.Fatalf("NormalizeDomainName(%q) = (%q, %v), want (%q, %v)", c.raw, name, internal, c.name, c.internal)
		}
	}
}

func TestSpecTextIsSingleSource(t *testing.T) {
	// AC9 的前提：规范常量非空、含关键契约要素（skill prompt 与界面导出都引用它）
	if len(SpecText) < 5000 {
		t.Fatalf("SpecText 异常短: %d", len(SpecText))
	}
	for _, want := range []string{"apidoc-meta", "identity_key", "E-CROSS-REF", "W-POS-INFERRED",
		"- **URL**：", "breaking", "scope: init", "最小示例"} {
		if !strings.Contains(SpecText, want) {
			t.Fatalf("SpecText 缺少关键内容 %q", want)
		}
	}
	if !strings.Contains(SpecText, "`POST /api/v1.0/user/subscription`") {
		t.Fatal("SpecText 的 URL 示例必须带 backtick")
	}
}
