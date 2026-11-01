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

// readLocalFixture 读取不入仓库的本地参考文档：先 testdata/，再仓库根 .doc/（git 排除目录）。
// 两处都没有则 Skip——文档不随仓库分发，新克隆环境不能因此红。
func readLocalFixture(t *testing.T, name string) string {
	t.Helper()
	for _, p := range []string{filepath.Join("testdata", name), filepath.Join("..", "..", ".doc", name)} {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Skipf("本地参考文档 %s 不存在（testdata/ 与 .doc/ 均未找到），跳过", name)
	return ""
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

	// 同名 VO 冲突：两处 **IPage 分页结构** 定义不同构 → ambiguous + W-STRUCT-DIVERGED。
	// 结构名 = IPage 分页：10-21 之前这里剥「分页结构」得到 IPage，与 **Foo 结构** 的剥法
	// 叠在一起会让 **FavoriteVO 分页结构** 撞成 FavoriteVO（假破坏性差异的根源）。
	// 「分页表仍与同族 item 表不同名」这条语义没变，只是名字里留着「分页」二字。
	var ipages []*SchemaDef
	for i := range doc.Schemas {
		if doc.Schemas[i].Name == "IPage 分页" {
			ipages = append(ipages, &doc.Schemas[i])
		}
	}
	if len(ipages) != 2 {
		t.Fatalf("IPage 分页 定义数 = %d, want 2", len(ipages))
	}
	for _, s := range ipages {
		if !s.Ambiguous {
			t.Fatalf("IPage 分页 同名不同构必须标记 ambiguous: %+v", s)
		}
		if s.OwnerScope == "" || s.OwnerIdx < 0 {
			t.Fatalf("IPage 分页 的 owner_scope 必须是宿主接口: %+v", s)
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

	// 8.4 删除订阅 / 9.3 取消收藏 / 10.3 移出暂缓池：三条 DELETE 都缺请求参数小节，
	// 且都缺返回参数表（只有「返回示例」JSON，没有 #### 返回参数 表）。
	// 11-01-req-section-absent D1 豁免 request_params 后，这三条仍是 partial
	// （response_params 真缺失不放松，D2），但 Unresolved 从 [request_params,response_params]
	// 收窄为恰 [response_params]，并各发一条 W-REQ-SECTION-ABSENT（整篇共 3 条）。
	// 这是预期语义变化（不是为绿而绿）：请求小节缺失不再污染 Unresolved，但返回表真缺失
	// 照样 partial——豁免只针对「请求小节整体缺失」，不掩盖其它信息缺失。
	for _, key := range []string{
		"DELETE|/api/v1.0/user/subscription/{}",
		"DELETE|/api/v1.0/user/favorite/{}",
		"DELETE|/api/v1.0/user/deferred/{}",
	} {
		del := doc.FindEndpoint(key)
		if del == nil {
			t.Fatalf("%s 必须入库", key)
		}
		if del.ReqParamsState != SectionAbsent {
			t.Fatalf("%s 请求小节应 SectionAbsent: %s", key, del.ReqParamsState)
		}
		if del.ParseState != ParsePartial || !equalNames(del.Unresolved, []string{"response_params"}) {
			t.Fatalf("%s 返回表真缺失仍 partial 且槽位恰 [response_params]（request_params 已豁免）: state=%s unresolved=%v", key, del.ParseState, del.Unresolved)
		}
	}
	if n := countCode(doc, WarnReqSectionAbsent); n != 3 {
		t.Fatalf("用户服务 golden 恰 3 条请求小节整体缺失（8.4/9.3/10.3），W-REQ-SECTION-ABSENT 应=3，实际 %d", n)
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
	// 无语言标记的 multipart 报文块 → multipart_raw 示例（分类不变）。
	// 11-01 修复（PRD Decision 2/3）：multipart 报文与纯请求行示例不再误告警 W-EXAMPLE-JSON——
	// 它们本就不是 JSON，JSON 检查现在排除 ExampleMultipart 并剥离请求行前缀（stripRequestLine）。
	// 本 golden 的全部 ```json 示例都是合法 JSON，故修复后整篇应零 W-EXAMPLE-JSON。
	// 这是预期语义变化（不是为绿而绿）：旧断言要求「multipart/请求行示例应产生 W-EXAMPLE-JSON」已随修复反转。
	var multipartSeen bool
	for _, ex := range parse.Examples {
		if ex.Kind == ExampleMultipart && strings.Contains(ex.Content, "Content-Disposition:") {
			multipartSeen = true
		}
	}
	if !multipartSeen {
		t.Fatalf("multipart 原始报文块应存为 multipart_raw 示例: %+v", parse.Examples)
	}
	if doc.HasCode(WarnExampleJSON) {
		for _, is := range doc.Warnings() {
			if is.Code == WarnExampleJSON {
				t.Logf("W-EXAMPLE-JSON line=%d snippet=%s", is.Line, is.Snippet)
			}
		}
		t.Fatal("multipart/请求行示例已排除、golden 全为合法 JSON，不应再有 W-EXAMPLE-JSON")
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

	// 5.2 查询论文简报（GET /api/v1.0/paper/brief/{paperId}）：请求参数小节整体缺失，
	// 但路径参数表 + 返回参数表齐全。11-01-req-section-absent D1 之前这条是 partial
	// （Unresolved=[request_params]）；D1「请求小节缺失=按无请求参数处理」之后升级为
	// parse_state=ok、零 Unresolved，并发一条 W-REQ-SECTION-ABSENT（行号=接口标题行）。
	// 这是预期语义变化（不是为绿而绿）：GET 无 Query/Body/Header 参数本就无需请求小节，
	// 旧规则把「作者没写=没有」误判成「作者漏写=信息缺失」。ReqParamsState 仍诚实记 absent。
	briefGet := doc.FindEndpoint("GET|/api/v1.0/paper/brief/{}")
	if briefGet == nil {
		t.Fatal("5.2 查询论文简报必须入库")
	}
	if briefGet.ReqParamsState != SectionAbsent {
		t.Fatalf("5.2 请求小节应判 SectionAbsent（状态字段不撒谎）: %s", briefGet.ReqParamsState)
	}
	if briefGet.ParseState != ParseOK || len(briefGet.Unresolved) != 0 {
		t.Fatalf("5.2 请求小节缺失豁免后应 ok 且零 Unresolved: state=%s unresolved=%v", briefGet.ParseState, briefGet.Unresolved)
	}
	if n := countCode(doc, WarnReqSectionAbsent); n != 1 {
		t.Fatalf("论文服务 golden 恰 1 条请求小节整体缺失（5.2），W-REQ-SECTION-ABSENT 应=1，实际 %d", n)
	}
}

// ===================== 分页结构撞名（10-21 事故复现）=====================

// findSchema 按名取命名结构（fixture 断言用）。
func findSchema(schemas []SchemaDef, name string) *SchemaDef {
	for i := range schemas {
		if schemas[i].Name == name {
			return &schemas[i]
		}
	}
	return nil
}

func schemaFieldNames(s *SchemaDef) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		out = append(out, f.Name)
	}
	return out
}

// TestSchemaNameFromBoldKeepsPagingSuffix 钉住结构名归一规则本身（事故根因的单测位）：
// 只剥尾部「结构」二字，**Foo 分页结构** 不得被归约成 **Foo 结构** 的同一个名字。
// 变异依据：把旧的两级剥离（先「分页结构」再「结构」）加回来，本用例立刻红。
func TestSchemaNameFromBoldKeepsPagingSuffix(t *testing.T) {
	cases := []struct{ title, want string }{
		{"FavoriteVO 结构", "FavoriteVO"},
		{"FavoriteVO 分页结构", "FavoriteVO 分页"}, // 旧行为剥成 FavoriteVO → 与上一行撞名（假破坏的源头）
		{"IPage 分页结构", "IPage 分页"},           // 旧基线写法同样保持独立名（D6：两种命名都不撞名）
		{"  SubscriptionVO 结构  ", "SubscriptionVO"},
		{"Brief", "Brief"}, // 没有「结构」后缀就原样保留
		{"PaperVO 结构说明", "PaperVO 结构说明"},
	}
	for _, c := range cases {
		if got := schemaNameFromBold(c.title); got != c.want {
			t.Fatalf("schemaNameFromBold(%q) = %q, want %q", c.title, got, c.want)
		}
	}
	if schemaNameFromBold("FavoriteVO 分页结构") == schemaNameFromBold("FavoriteVO 结构") {
		t.Fatal("分页表与 item 表必须归一成两个不同名字")
	}
}

// TestParsePagingCollisionFixture 用真实文档片段（testdata/paperpilot-paging-collision.md，
// 摘自用户 PaperPilot-接口文档-v1.md 的 9.1/9.2 收藏与 13.2 反馈三节）钉住修复效果：
// 分页表与 item 表各自是独立命名结构、字段不合并、都不标 ambiguous、零 W-STRUCT-DIVERGED。
// 变异依据：恢复旧的「分页结构」剥离后，本 fixture 里 FavoriteVO（行 47/59/121 三条定义）与
// FeedbackVO（行 190/203）各自撞成一个名字 → 实测 2 条 W-STRUCT-DIVERGED、5 条结构全标
// ambiguous；用户那份 46 接口文档据此整批 E-BREAKING-DENIED（详见 appsvc 同名导入用例的对照注释）。
func TestParsePagingCollisionFixture(t *testing.T) {
	doc := Parse(readFixture(t, "paperpilot-paging-collision.md"), Options{})
	for _, is := range doc.Errors() {
		t.Logf("E %s line=%d snippet=%s hint=%s", is.Code, is.Line, is.Snippet, is.Hint)
	}
	if doc.HasErrors() {
		t.Fatalf("规范模式（scope: incremental）解析真实片段不应产生 E-*，共 %d 条", len(doc.Errors()))
	}
	if doc.Mode != ModeIncremental {
		t.Fatalf("mode = %s, want incremental", doc.Mode)
	}
	if len(doc.Endpoints) != 3 {
		keys := []string{}
		for _, e := range doc.Endpoints {
			keys = append(keys, e.IdentityKey)
		}
		t.Fatalf("接口数 = %d %v, want 3", len(doc.Endpoints), keys)
	}
	if doc.HasCode(WarnStructDiverged) {
		for _, is := range doc.Warnings() {
			t.Logf("W-STRUCT-DIVERGED %+v", is)
		}
		t.Fatal("分页表与 item 表不撞名后不得再有 W-STRUCT-DIVERGED（AC1）")
	}

	getFav := doc.FindEndpoint("GET|/api/v1.0/user/favorite")
	item := findSchema(getFav.Schemas, "FavoriteVO")
	if item == nil {
		t.Fatalf("9.1 缺 **FavoriteVO 结构**: %v", schemaNames(getFav.Schemas))
	}
	if want := []string{"id", "paperId", "title", "arxivId", "doi", "briefSnapshot", "createTime"}; !equalNames(schemaFieldNames(item), want) {
		t.Fatalf("FavoriteVO 字段 = %v, want %v（item 表必须独立成结构，不被分页字段污染）", schemaFieldNames(item), want)
	}
	page := findSchema(getFav.Schemas, "FavoriteVO 分页")
	if page == nil {
		t.Fatalf("9.1 缺 **FavoriteVO 分页结构**: %v", schemaNames(getFav.Schemas))
	}
	if want := []string{"records", "total", "size", "current", "pages"}; !equalNames(schemaFieldNames(page), want) {
		t.Fatalf("FavoriteVO 分页 字段 = %v, want %v", schemaFieldNames(page), want)
	}
	for _, s := range []*SchemaDef{item, page} {
		if s.Ambiguous {
			t.Fatalf("两套字段属于两个不同结构名，不得标 ambiguous: %+v", s)
		}
		if s.OwnerScope != getFav.IdentityKey || s.OwnerIdx < 0 {
			t.Fatalf("分页表与 item 表都必须作为命名结构挂在接口下: %+v", s)
		}
	}
	if item.BodyHash == page.BodyHash {
		t.Fatal("两套字段的 bodyHash 必须不同（撞名同源同味的判据本身不能失效）")
	}
	// IPage<FavoriteVO> 仍按类型字符串展示（D5：不做泛型展开）
	var dataField *Field
	for i := range getFav.ResponseFields {
		if getFav.ResponseFields[i].Name == "data" {
			dataField = &getFav.ResponseFields[i]
		}
	}
	if dataField == nil || dataField.TypeRaw != `IPage\<FavoriteVO\>` {
		t.Fatalf("返回参数 data 的类型原文必须保真（不展开泛型）: %+v", dataField)
	}

	// 9.2 里重复内联的 **FavoriteVO 结构** 与 9.1 同构 → 跨接口同名也不标 ambiguous
	postFav := doc.FindEndpoint("POST|/api/v1.0/user/favorite")
	again := findSchema(postFav.Schemas, "FavoriteVO")
	if again == nil || again.Ambiguous || again.BodyHash != item.BodyHash {
		t.Fatalf("9.2 的 FavoriteVO 应与 9.1 同构且不标 ambiguous: %+v", again)
	}

	// 13.2 反馈一节同样两套并存（真实复现里它产出的正是 E-DIFF-MISMATCH-UNCHANGED）
	feedback := doc.FindEndpoint("GET|/api/v1.0/user/feedback")
	fbItem := findSchema(feedback.Schemas, "FeedbackVO")
	fbPage := findSchema(feedback.Schemas, "FeedbackVO 分页")
	if fbItem == nil || fbPage == nil {
		t.Fatalf("13.2 缺 FeedbackVO / FeedbackVO 分页 两套结构: %v", schemaNames(feedback.Schemas))
	}
	if len(fbItem.Fields) != 8 || len(fbPage.Fields) != 5 {
		t.Fatalf("FeedbackVO 字段数 = %d, 分页 = %d, want 8 / 5", len(fbItem.Fields), len(fbPage.Fields))
	}
	for _, s := range []*SchemaDef{fbItem, fbPage, item, page, again} {
		for i := range doc.Schemas {
			if doc.Schemas[i].Name == s.Name && doc.Schemas[i].Ambiguous {
				t.Fatalf("registry 里 %s 被标 ambiguous（撞名残留）: %+v", s.Name, doc.Schemas[i])
			}
		}
	}
}

func schemaNames(schemas []SchemaDef) []string {
	out := make([]string, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, s.Name)
	}
	return out
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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

// ===================== 11-01：非接口块误阻断 + 示例 JSON 误告警 =====================

// testMetaIncremental 内联回归用例的规范 meta 头（scope: incremental → strict）。
const testMetaIncremental = fence3 + "apidoc-meta\nspec: 1\nproject: p\nscope: incremental\nchange_type: fix\nchange_title: t\nchange_note: n\n" + fence3 + "\n\n"

// TestParseChangeOverviewFixture 仿用户真实文档结构（本地参考文档 .doc/paperpilot-change-overview.md，不入仓库）：
// 非领域章节「变更总览」的两个无 URL ### 摘要块（表格型 + 列表型）不再误报 E 整批阻断，
// 领域章节的规范接口块（请求行 / multipart / 请求行+JSON body 示例）不再误告警 W-EXAMPLE-JSON。
// 变异依据：把 adjudicateNoURLBlocks 改回 passBullets 当场 E → 本 fixture 出 2 条 E-URL-FORMAT；
// 去掉 routeExample 的 multipart 排除或 stripRequestLine → W-EXAMPLE-JSON 重新出现。
func TestParseChangeOverviewFixture(t *testing.T) {
	src := readLocalFixture(t, "paperpilot-change-overview.md")
	doc := Parse(src, Options{})
	if doc.Mode != ModeIncremental {
		t.Fatalf("mode = %s, want incremental", doc.Mode)
	}
	// AC1：0 个 E（非接口摘要块不再整批阻断）
	if doc.HasErrors() {
		for _, is := range doc.Errors() {
			t.Logf("E %s line=%d snippet=%s", is.Code, is.Line, is.Snippet)
		}
		t.Fatalf("fixture 导入应 0 阻断，实际 %d 条 E", len(doc.Errors()))
	}
	// 3 条真接口全部入库；变更总览不产生领域（结构规则：无 URL bullet 的章节非领域）
	if len(doc.Endpoints) != 3 {
		t.Fatalf("接口数 = %d, want 3", len(doc.Endpoints))
	}
	if len(doc.Domains) != 1 || doc.Domains[0].Name != "用户与认证" {
		t.Fatalf("领域 = %+v, want 仅「用户与认证」（变更总览非领域）", doc.Domains)
	}
	// 非领域 ### 摘要块 → Unparsed 保留原文 + W-UNPARSED-BLOCK（不是 E）
	for _, heading := range []string{"需要改代码的", "后端已修"} {
		var u *UnparsedBlock
		for i := range doc.Unparsed {
			if doc.Unparsed[i].Heading == heading {
				u = &doc.Unparsed[i]
			}
		}
		if u == nil {
			t.Fatalf("非接口块 %q 应进 Unparsed: %+v", heading, doc.Unparsed)
		}
		if !strings.Contains(u.RawMarkdown, "### "+heading) {
			t.Fatalf("Unparsed 应保留 %q 原文: %.80s", heading, u.RawMarkdown)
		}
		// W-UNPARSED-BLOCK 行号 = 块标题行，hint 为「按未解析块保留原文」同款
		var warned bool
		for _, is := range doc.Warnings() {
			if is.Code == WarnUnparsedBlock && is.Line == u.Line && strings.Contains(is.Hint, "按未解析块保留原文") {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("非接口块 %q 应报 W-UNPARSED-BLOCK（行 %d）: %+v", heading, u.Line, doc.Warnings())
		}
	}
	// AC1：W-EXAMPLE-JSON 应为 0（multipart / 请求行 / 合法 body 都不再误告警）
	if doc.HasCode(WarnExampleJSON) {
		for _, is := range doc.Warnings() {
			if is.Code == WarnExampleJSON {
				t.Logf("W-EXAMPLE-JSON line=%d snippet=%s", is.Line, is.Snippet)
			}
		}
		t.Fatal("fixture 不应有 W-EXAMPLE-JSON（multipart/请求行示例已排除、body 合法）")
	}
	// multipart 报文示例：Kind 正确（ExampleMultipart）且原文保真
	parse := doc.FindEndpoint("POST|/api/v1.0/paper/parse")
	if parse == nil {
		t.Fatal("1.2 解析论文PDF 应入库")
	}
	var mp *Example
	for i := range parse.Examples {
		if parse.Examples[i].Kind == ExampleMultipart {
			mp = &parse.Examples[i]
		}
	}
	if mp == nil || !strings.Contains(mp.Content, "Content-Disposition:") {
		t.Fatalf("multipart 报文应存为 ExampleMultipart: %+v", parse.Examples)
	}
	// 纯请求行示例：Kind=ExampleRequest（不是 multipart），无告警
	byName := doc.FindEndpoint("GET|/api/v1.0/user")
	if byName == nil || len(byName.Examples) == 0 || byName.Examples[0].Kind != ExampleRequest {
		t.Fatalf("1.1 纯请求行示例应为 ExampleRequest: %+v", byName)
	}
	// 请求行 + 合法 JSON body 示例：仍存为 ExampleRequest，body 合法不告警
	sub := doc.FindEndpoint("POST|/api/v1.0/user/subscription")
	if sub == nil || len(sub.Examples) == 0 || sub.Examples[0].Kind != ExampleRequest {
		t.Fatalf("1.3 请求行+JSON body 示例应为 ExampleRequest: %+v", sub)
	}
	// 确定性：两次解析字节一致（推迟裁决按候选块序补发，不得引入顺序随机）
	b1, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal 1: %v", err)
	}
	b2, err := json.Marshal(Parse(src, Options{}))
	if err != nil {
		t.Fatalf("marshal 2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatal("fixture 两次解析结果不一致（推迟裁决破坏确定性）")
	}
}

// TestParseNoURLBlockInDomainStillError 回归保护：领域章节内缺 URL 的 ### 是真接口缺 URL，
// 必须仍报 E-URL-FORMAT 阻断（防静默丢接口）。推迟裁决不得因「推迟」而把它降成 W。
// 变异依据：把 adjudicateNoURLBlocks 的 inDomain 分支改成一律 W → 本用例红。
func TestParseNoURLBlockInDomainStillError(t *testing.T) {
	src := testMetaIncremental +
		"## 一、用户接口\n\n" +
		"### 1.1 正常接口\n\n- **URL**：" + q + "GET /api/v1.0/user" + q + "\n- **变更**：未变更\n\n" +
		"### 1.2 缺URL的接口\n\n这个块没有 URL 行。\n"
	d := Parse(src, Options{})
	if !d.HasCode(ErrURLFormat) {
		t.Fatalf("领域章节缺 URL 接口应仍报 E-URL-FORMAT: %+v", d.Issues)
	}
	// E 落在缺 URL 块的标题行（带行号），而不是正常接口
	var sawE bool
	for _, is := range d.Errors() {
		if is.Code == ErrURLFormat && strings.Contains(is.Snippet, "缺URL的接口") {
			sawE = true
			if is.Line <= 0 {
				t.Fatalf("E-URL-FORMAT 行号必给: %+v", is)
			}
		}
	}
	if !sawE {
		t.Fatalf("E-URL-FORMAT 应落在缺 URL 块上: %+v", d.Errors())
	}
	if d.FindEndpoint("GET|/api/v1.0/user") == nil {
		t.Fatal("正常接口 1.1 应仍入库")
	}
	// 缺 URL 块仍进 Unparsed（原文保真，与推迟前一致）
	var inUnparsed bool
	for _, u := range d.Unparsed {
		if u.Heading == "1.2 缺URL的接口" {
			inUnparsed = true
		}
	}
	if !inUnparsed {
		t.Fatalf("缺 URL 块应进 Unparsed 保留原文: %+v", d.Unparsed)
	}
}

// TestParseDialectURLStillErrorAfterAdjudication 回归保护：方言 **URL:**（半角）是 near-miss，
// urlish=true → 不属于「无 URL 候选」，E-URL-FORMAT 由 parseBulletNearMiss 当场发出，
// 不被推迟裁决降级；章节仍是领域（0 可入库 → W-EMPTY-DOMAIN）。
// 变异依据：把 urlish 块也误收进候选并降 W → 本用例红（与 TestParseHalfWidthColonBullet 互补：
// 那个用例无 meta、覆盖两模式，本用例带规范 meta 钉住 strict 下的领域判定不被推迟裁决改变）。
func TestParseDialectURLStillErrorAfterAdjudication(t *testing.T) {
	src := testMetaIncremental +
		"## 一、用户接口\n\n" +
		"### 1.1 方言URL\n\n- **URL:** " + q + "GET /api/v1.0/user" + q + "\n- **变更**：未变更\n"
	d := Parse(src, Options{})
	if !d.HasCode(ErrURLFormat) {
		t.Fatalf("方言 **URL:** 必须仍报 E-URL-FORMAT: %+v", d.Issues)
	}
	if len(d.Endpoints) != 0 {
		t.Fatalf("方言 URL 接口不能入库: %+v", d.Endpoints)
	}
	if !d.HasCode(WarnEmptyDomain) {
		t.Fatalf("方言块 urlish=true → 章节仍是领域（0 可入库应报 W-EMPTY-DOMAIN）: %+v", d.Issues)
	}
}

// TestParseNoURLAdjudicationInitMode init 模式：无 URL 块无论所在章节是否领域都只 W（不阻断），
// 与推迟前行为一致（init 原本就是 W）。
func TestParseNoURLAdjudicationInitMode(t *testing.T) {
	metaInit := fence3 + "apidoc-meta\nspec: 1\nproject: p\nscope: init\n" + fence3 + "\n\n"
	src := metaInit +
		"## 一、用户接口\n\n" +
		"### 1.1 正常接口\n\n- **URL**：" + q + "GET /api/v1.0/user" + q + "\n\n" +
		"### 1.2 缺URL\n\n没有 URL 行。\n\n" +
		"## 变更总览\n\n### 摘要块\n\n- 一条说明\n"
	d := Parse(src, Options{})
	if d.Mode != ModeInit {
		t.Fatalf("mode = %s, want init", d.Mode)
	}
	if d.HasErrors() || d.HasCode(ErrURLFormat) {
		t.Fatalf("init 模式无 URL 块不应阻断/报 E-URL-FORMAT: %+v", d.Issues)
	}
	// 领域内缺 URL 块（1.2）与非领域摘要块都 → W-UNPARSED-BLOCK（「按未解析块保留原文」同款）
	var count int
	for _, is := range d.Warnings() {
		if is.Code == WarnUnparsedBlock && strings.Contains(is.Hint, "按未解析块保留原文") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("init 模式两个无 URL 块应各报一条 W-UNPARSED-BLOCK，实际 %d: %+v", count, d.Warnings())
	}
}

// TestParseNoURLOrphanTopLevelBlock 顶层孤儿 ###（不在任何 ## 之下、无 URL）：
// chapIdx=-1 → adjudicateNoURLBlocks 按非领域处理 → W-UNPARSED-BLOCK（不 panic、不阻断），原文进 Unparsed。
// 这是裁决推迟后新增的边界（推迟前 passBullets 对 strict 一律 E，孤儿块也会被误 E）；
// 变异依据：把 inDomain 的 chapIdx<0 分支误判为领域 → 本用例出 E（红）。
func TestParseNoURLOrphanTopLevelBlock(t *testing.T) {
	src := testMetaIncremental + "### 顶层摘要块\n\n- 一条没有 URL 的说明\n"
	d := Parse(src, Options{})
	if d.HasErrors() {
		t.Fatalf("顶层孤儿无 URL ### 不应阻断: %+v", d.Errors())
	}
	if !d.HasCode(WarnUnparsedBlock) {
		t.Fatalf("顶层孤儿无 URL ### 应报 W-UNPARSED-BLOCK: %+v", d.Issues)
	}
	var found bool
	for _, u := range d.Unparsed {
		if u.Heading == "顶层摘要块" {
			found = true
		}
	}
	if !found {
		t.Fatalf("顶层孤儿块应进 Unparsed 保留原文: %+v", d.Unparsed)
	}
}

// exampleDoc 构造「一个 POST 接口 + 一个请求示例 fence」的规范文档，供示例 JSON 检查粒度用例。
func exampleDoc(fenceInfo, content string) string {
	return testMetaIncremental +
		"## 一、用户接口\n\n" +
		"### 1.1 新增订阅\n\n- **URL**：" + q + "POST /api/v1.0/user/subscription" + q + "\n- **变更**：未变更\n\n" +
		"#### 请求示例\n\n" + fence3 + fenceInfo + "\n" + content + "\n" + fence3 + "\n"
}

// TestParseExampleJSONRefined 钉住示例 JSON 检查的细化粒度（PRD Decision 2/3）：
// multipart 报文与纯请求行/纯头不告警；请求行+JSON body 仍校验 body（非法告警、合法不告警）；
// 普通非法 JSON（无请求行前缀）照样告警——回归保护，检查强度不降。
// 11-01 check B6：请求行后「无空行分隔」直接跟 body 时同样校验 body——头行剥离不得把
// 非法 body 当头吞掉（漏检方向）。变异依据：去掉 stripRequestLine 的「非头行收口」分支 → 无空行非法 body 用例红。
func TestParseExampleJSONRefined(t *testing.T) {
	cases := []struct {
		name     string
		info     string
		content  string
		wantWarn bool
		wantKind string
	}{
		{"纯请求行", "", "POST /api/v1.0/user/subscription", false, ExampleRequest},
		{"请求行+头无body", "", "POST /api/v1.0/user/subscription\nContent-Type: application/json", false, ExampleRequest},
		{"请求行+合法JSON body", "", "POST /api/v1.0/user/subscription\nContent-Type: application/json\n\n{\"topCategory\":\"cs\"}", false, ExampleRequest},
		{"请求行+非法JSON body", "", "POST /api/v1.0/user/subscription\nContent-Type: application/json\n\n{bad json}", true, ExampleRequest},
		{"请求行+合法JSON无空行", "", "POST /a\n{\"a\":1}", false, ExampleRequest},
		{"请求行+非法JSON无空行", "", "POST /a\n{bad}", true, ExampleRequest},
		{"头行后非法JSON无空行", "", "POST /a\nContent-Type: application/json\n{bad}", true, ExampleRequest},
		{"multipart报文", "", "POST /api/v1.0/paper/parse\nContent-Type: multipart/form-data; boundary=X\n\n--X\nContent-Disposition: form-data; name=\"pdfFile\"\n\n[binary]\n--X--", false, ExampleMultipart},
		{"json标记合法", "json", "{\"a\":1}", false, ExampleRequest},
		{"json标记非法", "json", "{bad}", true, ExampleRequest},
		{"无标记非法JSON", "", "{bad}", true, ExampleRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Parse(exampleDoc(c.info, c.content), Options{})
			if d.HasErrors() {
				t.Fatalf("不应有 E: %+v", d.Errors())
			}
			ep := d.FindEndpoint("POST|/api/v1.0/user/subscription")
			if ep == nil || len(ep.Examples) != 1 {
				t.Fatalf("应恰有一个示例: %+v", ep)
			}
			if ep.Examples[0].Kind != c.wantKind {
				t.Fatalf("Kind = %s, want %s", ep.Examples[0].Kind, c.wantKind)
			}
			if got := d.HasCode(WarnExampleJSON); got != c.wantWarn {
				t.Fatalf("W-EXAMPLE-JSON = %v, want %v: %+v", got, c.wantWarn, d.Issues)
			}
		})
	}
}

// TestStripRequestLine 请求行剥离小函数的独立单测（PRD Decision 3 要求独立函数便于单测）。
// 钉住：只在首个非空行是请求行时剥离请求行+连续「头形态」行（到首个空行或首个非头行收口），剩余 body 返回；
// 纯请求行/纯头返回空串；非请求行原样返回（大小写敏感，方法必须大写）。
// 无空行分隔用例（11-01 check B6）：body 直接跟在请求行/头行后时，从首个非头行起返回 body，不得吞掉。
func TestStripRequestLine(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"纯请求行", "GET /api/v1.0/user?x=1", ""},
		{"请求行+头无body", "POST /a\nContent-Type: application/json", ""},
		{"请求行+头+空行+body", "POST /a\nContent-Type: application/json\n\n{\"x\":1}", "{\"x\":1}"},
		{"请求行+空行+body", "GET /a\n\n{\"x\":1}", "{\"x\":1}"},
		{"前导空行+请求行", "\n\nGET /a\n\n{\"x\":1}", "{\"x\":1}"},
		{"请求行+非法body", "POST /a\n\n{bad}", "{bad}"},
		{"请求行直接跟JSON无空行", "POST /a\n{\"x\":1}", "{\"x\":1}"},
		{"请求行直接跟非法JSON无空行", "POST /a\n{bad}", "{bad}"},
		{"头行后直接跟body无空行", "POST /a\nContent-Type: application/json\n{bad}", "{bad}"},
		{"非请求行JSON原样", "{\"x\":1}", "{\"x\":1}"},
		{"非请求行非法JSON原样", "{bad}", "{bad}"},
		{"小写方法不算请求行", "get /a\n\n{\"x\":1}", "get /a\n\n{\"x\":1}"},
		{"空内容", "", ""},
		{"纯空白", "\n\n", "\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripRequestLine(c.content); got != c.want {
				t.Fatalf("stripRequestLine(%q) = %q, want %q", c.content, got, c.want)
			}
		})
	}
}

// ===================== 11-01-req-section-absent：请求参数小节缺失豁免 =====================

// reqSectionDoc 构造「单接口 + 返回参数表齐全」的规范文档，请求参数小节由 reqSection 传入
// （空串 = 整体缺失）。path 带 {id} 时用于路径参数真缺失分支。
func reqSectionDoc(meta, url, changeLine, reqSection, pathSection string) string {
	return meta +
		"## 一、用户接口\n\n" +
		"### 1.1 查询用户\n\n- **URL**：" + q + url + q + "\n" + changeLine + "\n" +
		reqSection + pathSection +
		"#### 返回参数\n\n| 参数名 | 类型 | 说明 |\n|---|---|---|\n| message | String | 响应消息 |\n"
}

const reqTableFull = "#### 请求参数\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| name | String | 否 | Query | 用户名 |\n\n"
const pathTableFull = "#### 路径参数\n\n| 参数名 | 类型 | 必填 | 位置 | 说明 |\n|---|---|---|---|---|\n| id | Long | 是 | Path | 用户ID |\n\n"

// countCode 数某码在报告里出现几次。
func countCode(d *Document, code string) int {
	n := 0
	for _, is := range d.Issues {
		if is.Code == code {
			n++
		}
	}
	return n
}

// TestParseReqSectionAbsentExempt 钉住 Decision 1/3：请求参数小节整体缺失 → 按无请求参数处理
// （parse_state=ok、不进 Unresolved），每接口一条 W-REQ-SECTION-ABSENT、行号=接口标题行，
// init/incremental 都发；ReqParamsState 仍诚实记 absent。
// 变异依据：把 classifyProse 的 SectionAbsent 豁免去掉（恢复旧「缺失→Unresolved」）→ 本用例红
// （ok 变 partial、W 消失）；把 W 的发放也去掉 → 警告计数断言红。
func TestParseReqSectionAbsentExempt(t *testing.T) {
	for _, mode := range []string{ModeIncremental, ModeInit} {
		t.Run(mode, func(t *testing.T) {
			meta := testMetaIncremental
			changeLine := "- **变更**：新增\n"
			if mode == ModeInit {
				meta = fence3 + "apidoc-meta\nspec: 1\nproject: p\nscope: init\n" + fence3 + "\n\n"
				changeLine = "" // init 允许缺变更行
			}
			d := Parse(reqSectionDoc(meta, "GET /api/v1.0/user", changeLine, "", ""), Options{})
			if d.HasErrors() {
				t.Fatalf("缺请求小节不应阻断: %+v", d.Errors())
			}
			ep := d.FindEndpoint("GET|/api/v1.0/user")
			if ep == nil {
				t.Fatal("接口应入库")
			}
			if ep.ParseState != ParseOK || len(ep.Unresolved) != 0 {
				t.Fatalf("请求小节整体缺失应 ok 且零 Unresolved: state=%s unresolved=%v", ep.ParseState, ep.Unresolved)
			}
			if ep.ReqParamsState != SectionAbsent {
				t.Fatalf("ReqParamsState 必须诚实记 absent（状态字段不撒谎）: %s", ep.ReqParamsState)
			}
			if n := countCode(d, WarnReqSectionAbsent); n != 1 {
				t.Fatalf("W-REQ-SECTION-ABSENT 应每接口一条，实际 %d: %+v", n, d.Issues)
			}
			for _, is := range d.Warnings() {
				if is.Code == WarnReqSectionAbsent {
					if is.Line != ep.Line {
						t.Fatalf("W 行号应=接口标题行 %d，实际 %d", ep.Line, is.Line)
					}
					if !strings.Contains(is.Hint, "请求参数小节缺失，已按无请求参数处理") {
						t.Fatalf("W 文案不符: %+v", is)
					}
				}
			}
		})
	}
}

// TestParseReqSectionTrueGapsStillPartial 钉住 Decision 2「保留不放松」：
// 路径含 {} 缺路径参数表 / 缺返回参数表 / 小节存在但空（SectionProse）→ 仍 Unresolved/partial。
// 变异依据：把豁免扩大到 path_params/response_params 或 SectionProse → 对应用例红。
func TestParseReqSectionTrueGapsStillPartial(t *testing.T) {
	t.Run("路径占位符缺路径参数表", func(t *testing.T) {
		d := Parse(reqSectionDoc(testMetaIncremental, "GET /api/v1.0/user/{id}", "- **变更**：新增\n", "", ""), Options{})
		ep := d.FindEndpoint("GET|/api/v1.0/user/{}")
		if ep == nil {
			t.Fatal("接口应入库")
		}
		if ep.ParseState != ParsePartial {
			t.Fatalf("路径含 {} 缺路径参数表必须仍 partial: %s", ep.ParseState)
		}
		if !equalNames(ep.Unresolved, []string{"path_params"}) {
			t.Fatalf("Unresolved 应恰为 [path_params]（request_params 已豁免）: %v", ep.Unresolved)
		}
		// 请求小节豁免照常生效：真缺失场景里仍发且只发一条 W
		if n := countCode(d, WarnReqSectionAbsent); n != 1 {
			t.Fatalf("请求小节缺失的 W 照发，实际 %d", n)
		}
	})
	t.Run("缺返回参数表", func(t *testing.T) {
		src := testMetaIncremental +
			"## 一、用户接口\n\n### 1.1 查询用户\n\n- **URL**：" + q + "GET /api/v1.0/user" + q + "\n- **变更**：新增\n\n" + reqTableFull
		d := Parse(src, Options{})
		ep := d.FindEndpoint("GET|/api/v1.0/user")
		if ep.ParseState != ParsePartial || !equalNames(ep.Unresolved, []string{"response_params"}) {
			t.Fatalf("缺返回参数表必须仍 partial 且槽位=response_params: state=%s unresolved=%v", ep.ParseState, ep.Unresolved)
		}
		if countCode(d, WarnReqSectionAbsent) != 0 {
			t.Fatal("请求参数表齐全时不得发 W-REQ-SECTION-ABSENT")
		}
	})
	t.Run("请求小节存在但空", func(t *testing.T) {
		d := Parse(reqSectionDoc(testMetaIncremental, "GET /api/v1.0/user", "- **变更**：新增\n", "#### 请求参数\n\n", ""), Options{})
		ep := d.FindEndpoint("GET|/api/v1.0/user")
		if ep.ParseState != ParsePartial || !equalNames(ep.Unresolved, []string{"request_params"}) {
			t.Fatalf("小节存在但空（写了一半更可疑）必须仍 partial: state=%s unresolved=%v", ep.ParseState, ep.Unresolved)
		}
		if ep.ReqParamsState != SectionProse {
			t.Fatalf("空小节应判 SectionProse: %s", ep.ReqParamsState)
		}
		if countCode(d, WarnReqSectionAbsent) != 0 {
			t.Fatal("小节存在（只是空）不属于「整体缺失」，不得发 W-REQ-SECTION-ABSENT")
		}
	})
	t.Run("路径参数表齐全则不报路径缺失", func(t *testing.T) {
		d := Parse(reqSectionDoc(testMetaIncremental, "GET /api/v1.0/user/{id}", "- **变更**：新增\n", "", pathTableFull), Options{})
		ep := d.FindEndpoint("GET|/api/v1.0/user/{}")
		if ep.ParseState != ParseOK || len(ep.Unresolved) != 0 {
			t.Fatalf("路径表齐全 + 请求小节缺失豁免 → ok: state=%s unresolved=%v", ep.ParseState, ep.Unresolved)
		}
	})
}

// TestParseReqSectionRefAndEmptyUnchanged 引用式与「无。」的行为一字不变（Decision 2）：
// 引用式 strict E-CROSS-REF / init W-CROSS-REF + partial + Unresolved request_params；
// 「无。」= SectionEmpty → ok、零 W-REQ-SECTION-ABSENT。
func TestParseReqSectionRefAndEmptyUnchanged(t *testing.T) {
	t.Run("引用式strict", func(t *testing.T) {
		d := Parse(reqSectionDoc(testMetaIncremental, "GET /api/v1.0/user", "- **变更**：新增\n", "#### 请求参数\n\n同 1.2。\n\n", ""), Options{})
		if !d.HasCode(ErrCrossRef) {
			t.Fatalf("引用式 strict 必须仍 E-CROSS-REF: %+v", d.Issues)
		}
		ep := d.FindEndpoint("GET|/api/v1.0/user")
		if ep.ParseState != ParsePartial || ep.ReqParamsState != SectionCrossRef || !equalNames(ep.Unresolved, []string{"request_params"}) {
			t.Fatalf("引用式仍 partial + Unresolved request_params: %+v", ep)
		}
		if countCode(d, WarnReqSectionAbsent) != 0 {
			t.Fatal("引用式不是「整体缺失」，不得发 W-REQ-SECTION-ABSENT")
		}
	})
	t.Run("引用式init", func(t *testing.T) {
		meta := fence3 + "apidoc-meta\nspec: 1\nproject: p\nscope: init\n" + fence3 + "\n\n"
		d := Parse(reqSectionDoc(meta, "GET /api/v1.0/user", "", "#### 请求参数\n\n同 1.2。\n\n", ""), Options{})
		if d.HasErrors() || !d.HasCode(WarnCrossRef) {
			t.Fatalf("init 引用式应降级 W-CROSS-REF 不阻断: %+v", d.Issues)
		}
		ep := d.FindEndpoint("GET|/api/v1.0/user")
		if ep.ParseState != ParsePartial || !equalNames(ep.Unresolved, []string{"request_params"}) {
			t.Fatalf("init 引用式仍 partial: %+v", ep)
		}
	})
	t.Run("明确无参数", func(t *testing.T) {
		d := Parse(reqSectionDoc(testMetaIncremental, "GET /api/v1.0/user", "- **变更**：新增\n", "#### 请求参数\n\n无。\n\n", ""), Options{})
		ep := d.FindEndpoint("GET|/api/v1.0/user")
		if ep.ParseState != ParseOK || ep.ReqParamsState != SectionEmpty || len(ep.Unresolved) != 0 {
			t.Fatalf("「无。」应 ok + SectionEmpty: %+v", ep)
		}
		if countCode(d, WarnReqSectionAbsent) != 0 {
			t.Fatal("写了「无。」不得发 W-REQ-SECTION-ABSENT")
		}
	})
}

// TestParseReqSectionAbsentRemovedDeclaration 删除声明允许省略参数/响应表（§8.2）：
// 不发 W-REQ-SECTION-ABSENT（删除块本就不要求小节，发了是噪音）、不进 Unresolved。
func TestParseReqSectionAbsentRemovedDeclaration(t *testing.T) {
	src := testMetaIncremental +
		"## 一、用户接口\n\n### 1.1 删除用户\n\n- **URL**：" + q + "DELETE /api/v1.0/user/{id}" + q + "\n" +
		"- **变更**：删除\n- **变更点**：合规下线\n"
	d := Parse(src, Options{})
	if d.HasErrors() {
		t.Fatalf("删除声明不应阻断: %+v", d.Errors())
	}
	ep := d.FindEndpoint("DELETE|/api/v1.0/user/{}")
	if ep == nil || len(ep.Unresolved) != 0 || ep.ParseState != ParseOK {
		t.Fatalf("删除声明豁免一切参数小节: %+v", ep)
	}
	if countCode(d, WarnReqSectionAbsent) != 0 {
		t.Fatalf("删除声明不得发 W-REQ-SECTION-ABSENT: %+v", d.Issues)
	}
}
