package appsvc

// 解析档位权威闸的「通道 × 文档声明」矩阵测试（spec-draft §13.26，任务 10-20 批次 ⓪）。
//
// 契约（§13.26，取代 §13.18 的分工说法）：
//   - 文档声明 init 而 req.InitMode == false      → 拒收（此前只有附件通道守住，向导漏着）
//   - req.InitMode == true 而文档声明 incremental/full → 同样拒收（对称面，否则勾选形同虚设）
//   - 文档没有 meta.scope（现存散文文档）          → 由 req.InitMode 决定档位，不拒
//   解析器 apidoc.resolveMode 的「meta 优先」一个字都不改（它不该知道调用方是谁），
//   冲突判定全在请求层 —— 所以本文件同时断言 pv.Mode 的**回显值**：拒收前解析器仍按
//   meta 走，闸只做「拒」，从不做「静默放行/静默改档」。
//
// 仓库约定：newTestService 真 SQLite（t.TempDir）+ 手写 t.Fatalf，无断言库。

import (
	"strings"
	"testing"

	"DevCraft/internal/apidoc"
	skillapidoc "DevCraft/internal/skill/apidoc"
)

// apiGateDenyMarker 拒绝理由的机器可核对前缀（两条中文文案共用，见 apiDocInitAuthority）。
const apiGateDenyMarker = "解析档位冲突"

// ===================== 三种文档声明变体（除 scope 外内容完全一致）=====================

// gateInitDoc meta 自声明 scope: init（缺 位置 列在 init 档只是 W，严格档是 E）。
func gateInitDoc() string {
	return "# 演示服务存量文档\n\n```apidoc-meta\nspec: 1\nproject: gate-init\nservice: 演示服务\nscope: init\n```\n\n" +
		"## 一、用户管理\n\n" +
		"### 1.1 查询用户\n\n按 id 查询。\n\n" +
		"- **URL**：`GET /api/v1/demo/user`\n\n" +
		"#### 请求参数\n\n| 参数名 | 类型 | 必填 | 说明 |\n|---|---|---|---|\n| id | Long | 是 | 用户ID |\n"
}

// gateIncrDoc meta 自声明 scope: incremental（严格档全绿，可直接入库）。
func gateIncrDoc() string {
	return "# 演示服务接口变更文档\n\n```apidoc-meta\nspec: 1\nproject: gate-incr\nservice: 演示服务\n" +
		"base_path: /api/v1/demo\nscope: incremental\nchange_type: feature\nchange_title: 档位闸用例\n" +
		"change_note: 验证请求标志是唯一权威\nversion_tag: REQ-GATE\n```\n\n" +
		"## 一、用户管理\n\n" +
		"### 1.1 查询用户\n\n按 id 查询。\n\n" +
		"- **URL**：`GET /api/v1/demo/user`\n" +
		"- **变更**：新增\n\n" +
		apiParamSection("| id | Long | 是 | Query | | 用户ID |") +
		apiRespSection("| message | String | 响应消息 |")
}

// gateFullDoc meta 自声明 scope: full（MVP 未实现，D17：解析器出 E-SCOPE-FULL-NOT-SUPPORTED）。
func gateFullDoc() string {
	return strings.Replace(gateIncrDoc(), "scope: incremental", "scope: full", 1)
}

// gateProseDoc 无 ```apidoc-meta 块（存量散文文档）：档位只能由请求标志决定。
func gateProseDoc() string {
	return "# 演示服务散文档\n\n## 一、用户管理\n\n### 1.9 散装接口\n\n" +
		"- **URL**：`GET /api/v1/demo/loose`\n"
}

// ===================== 三条通道（差别只在 AutoFix 与勾选位是否真进请求）=====================

type apiGateChannel struct {
	name string
	// honoursCheckbox 勾选位是否真的写进 req.InitMode：技能通道不写（恒 false），
	// 于是矩阵里「勾选」那一格对它是空操作——正好证明 §13.18 的意图由 §13.26 兑现。
	honoursCheckbox bool
	build           func(doc, projectName string, initMode bool) ApiDocImportRequest
}

func apiGateChannels() []apiGateChannel {
	return []apiGateChannel{
		{"界面向导（AutoFix=false）", true, func(doc, projectName string, initMode bool) ApiDocImportRequest {
			return ApiDocImportRequest{DocText: doc, ProjectName: projectName, InitMode: initMode}
		}},
		{"聊天附件（AutoFix=true）", true, func(doc, projectName string, initMode bool) ApiDocImportRequest {
			return ApiDocImportRequest{DocText: doc, ProjectName: projectName, InitMode: initMode, AutoFix: true}
		}},
		{"模型驱动技能（apiDocSkillRequest）", false, func(doc, projectName string, _ bool) ApiDocImportRequest {
			return apiDocSkillRequest(skillapidoc.ImportInput{DocText: doc, ProjectName: projectName}, false)
		}},
	}
}

type apiGateDecl struct {
	name  string
	doc   string
	scope string // 文档自声明的 scope（'' = 无 meta）
}

func apiGateDecls() []apiGateDecl {
	return []apiGateDecl{
		{"meta 声明 scope: init", gateInitDoc(), apidoc.ModeInit},
		{"meta 声明 scope: incremental", gateIncrDoc(), apidoc.ModeIncremental},
		{"无 meta（现存散文文档）", gateProseDoc(), ""},
	}
}

// gateEffectiveInit 该通道该格真正生效的勾选位。
func gateEffectiveInit(ch apiGateChannel, checkbox bool) bool {
	return checkbox && ch.honoursCheckbox
}

// gateWantDeny §13.26 三条规则的期望判定。
func gateWantDeny(effInit bool, scope string) bool {
	switch scope {
	case apidoc.ModeInit:
		return !effInit // 文档说 init、没勾选 → 拒
	case "":
		return false // 无 meta：勾选说了算，永不冲突
	default:
		return effInit // 文档说 incremental/full、勾了选 → 拒（对称面）
	}
}

// gateWantMode 期望回显档位（解析器 meta 优先不变；无 meta 时才由请求标志决定）。
func gateWantMode(effInit bool, scope string) string {
	if scope != "" {
		return scope
	}
	if effInit {
		return apidoc.ModeInit
	}
	return apidoc.ModeIncremental
}

func gatePreviewDenied(pv ApiDocImportPreview) bool {
	for _, n := range pv.Notes {
		if strings.Contains(n, apiGateDenyMarker) {
			return true
		}
	}
	return false
}

// ===================== 矩阵本体：3 通道 × 3 声明 × 勾选位两态 =====================

func TestApiDocInitAuthorityMatrix(t *testing.T) {
	for _, ch := range apiGateChannels() {
		for _, decl := range apiGateDecls() {
			for _, checkbox := range []bool{false, true} {
				eff := gateEffectiveInit(ch, checkbox)
				wantDeny := gateWantDeny(eff, decl.scope)
				wantMode := gateWantMode(eff, decl.scope)
				cn, dn := ch.name, decl.name
				svc := newTestService(t)

				// 项目名统一由请求给出（散文文档那一列没有 meta.project，缺它预览会走
				// 「无法确定项目名」的基础错误，与档位闸无关，会掩盖本用例的判据）
				req := ch.build(decl.doc, "gate-proj", checkbox)
				if !ch.honoursCheckbox && (req.InitMode || req.AllowInitOnNonEmpty || req.AutoFix) {
					t.Fatalf("%s：技能通道三个键必须恒 false（§13.26 由此自动获得聊天拒绝 init）: %+v", cn, req)
				}

				// ① 预览：被拒/放行 + 档位回显 + 指路文案
				pv, err := svc.PreviewApiDocImport(req)
				if err != nil {
					t.Fatalf("%s × %s × 勾选=%v：预览抛基础设施错误: %v", cn, dn, checkbox, err)
				}
				if got := gatePreviewDenied(pv); got != wantDeny {
					t.Fatalf("%s × %s × 勾选=%v：预览被拒判定应为 %v，实际 %v（notes=%v msg=%q）",
						cn, dn, checkbox, wantDeny, got, pv.Notes, pv.Message)
				}
				if pv.Mode != wantMode {
					t.Fatalf("%s × %s × 勾选=%v：档位回显应为 %s，实际 %s", cn, dn, checkbox, wantMode, pv.Mode)
				}
				if wantDeny {
					if pv.OK {
						t.Fatalf("%s × %s × 勾选=%v：档位冲突一律拒收，预览不得放行: %+v", cn, dn, checkbox, pv)
					}
					if !strings.Contains(pv.Message, "基线导入") {
						t.Fatalf("%s：拒绝时汇总文案要让用户看懂是哪一道闸: %q", cn, pv.Message)
					}
					note := pv.Notes[len(pv.Notes)-1]
					if !strings.Contains(note, "scope: "+decl.scope) || !strings.Contains(note, "勾选") {
						t.Fatalf("%s：拒绝理由必须回显冲突的 scope 并指路怎么放行: %q", cn, note)
					}
				}

				// ② 提交：同一条闸（不能只拦预览不拦提交）
				_, err = svc.CommitApiDocImport(req)
				deniedByCommit := err != nil && strings.Contains(err.Error(), apiGateDenyMarker)
				if deniedByCommit != wantDeny {
					t.Fatalf("%s × %s × 勾选=%v：提交被档位闸拒收判定应为 %v，实际 %v（err=%v）",
						cn, dn, checkbox, wantDeny, deniedByCommit, err)
				}
			}
		}
	}
}

// ===================== 实测探针场景固化为回归测试（§13.26 的漏判本身）=====================

// 修复前的实测结论：
//
//	AutoFix=false（界面向导 / 模型驱动技能） mode=init  ok=true   ← 文档自声明 scope: init
//	                                          就拿到宽松解析，严格校验被绕过
//	AutoFix=true （聊天附件通道）             mode=init  ok=false ← 只有附件通道守住了
//
// 本用例把第一行钉成「必须被拒」，并证明解析器没被改动（meta 仍然优先，闸在请求层）。
func TestApiDocInitAuthorityProbeScenarioMustBeDenied(t *testing.T) {
	// 解析器纪律：不改它。meta.scope 仍然覆盖 opts.Mode（§13.26 的活儿由请求层干）
	doc := apidoc.Parse(gateInitDoc(), apidoc.Options{Mode: apidoc.ModeIncremental})
	if doc.Mode != apidoc.ModeInit {
		t.Fatalf("解析器不该被改动：meta.scope: init 仍应优先于 opts.Mode，实际 %s", doc.Mode)
	}

	svc := newTestService(t)
	before := apiRawTableCounts(t)
	_, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: gateInitDoc()}) // 向导、未勾选
	if err == nil || !strings.Contains(err.Error(), apiGateDenyMarker) {
		t.Fatalf("未勾选而文档自声明 init 必须被拒（修复前这里是 ok=true 绕过严格校验）: %v", err)
	}
	if !strings.Contains(err.Error(), "文档声明 scope: init，但界面未勾选初始化/基线导入模式") {
		t.Fatalf("拒绝文案要按 §13.26 的口径说明冲突与放行方式: %v", err)
	}
	for table, n := range apiRawTableCounts(t) {
		if n != before[table] || n != 0 {
			t.Fatalf("被档位闸拒收后 %s 残留 %d 行（应为 0）", table, n)
		}
	}

	// 放行路径确实存在（闸不是一刀切禁 init）：同一份文档勾选初始化 + 空项目 → 灌库成功
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: gateInitDoc(), InitMode: true})
	if res.Mode != apidoc.ModeInit || res.Stats.Added == 0 || res.EventID != 0 {
		t.Fatalf("勾选后应合法灌入且不生成事件: %+v", res)
	}
}

// ===================== 对称面：勾选了而文档声明增量/全量 =====================

func TestApiDocInitAuthoritySymmetricFace(t *testing.T) {
	svc := newTestService(t)
	// incremental：勾选 = 冲突（否则勾选形同虚设）
	if _, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: gateIncrDoc(), InitMode: true}); err == nil ||
		!strings.Contains(err.Error(), apiGateDenyMarker) || !strings.Contains(err.Error(), "scope: incremental") {
		t.Fatalf("勾选 + 文档声明 incremental 必须同样被拒并回显 scope: %v", err)
	}
	// 未勾选 = 正常放行入库（对照，证明拒收只由冲突触发）
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: gateIncrDoc()})
	if res.Mode != apidoc.ModeIncremental || res.Seq != 1 {
		t.Fatalf("未勾选且文档声明 incremental 应正常导入并生成 v1 事件: %+v", res)
	}

	// full：勾选 = 档位冲突；未勾选 = 由解析器的 E-SCOPE-FULL-NOT-SUPPORTED 拒（两道闸各管各的）
	if _, err := svc.CommitApiDocImport(ApiDocImportRequest{DocText: gateFullDoc(), InitMode: true}); err == nil ||
		!strings.Contains(err.Error(), apiGateDenyMarker) {
		t.Fatalf("勾选 + 文档声明 full 必须被档位闸拒收: %v", err)
	}
	pv, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: gateFullDoc()})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if gatePreviewDenied(pv) {
		t.Fatalf("未勾选的 scope: full 不该由档位闸拒（该由 E-SCOPE-FULL 拒）: %+v", pv.Notes)
	}
	if pv.OK {
		t.Fatalf("scope: full 在 MVP 未实现，预览不得放行: %+v", pv)
	}
}

// ===================== 无 meta 的散文文档：勾选位真决定档位（保持现状）=====================

func TestApiDocInitAuthorityNoMetaProseUsesRequestFlag(t *testing.T) {
	svc := newTestService(t)
	prose := gateProseDoc()

	// 勾选 → init 档灌入（严格校验的 E-SPEC-BLOCK-MISSING 不该出现）
	pvInit, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: prose, ProjectName: "prose-init", InitMode: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if pvInit.Mode != apidoc.ModeInit || gatePreviewDenied(pvInit) {
		t.Fatalf("无 meta + 勾选应按 init 解析且不拒收: %+v", pvInit)
	}
	res := commitApiDoc(t, svc, ApiDocImportRequest{DocText: prose, ProjectName: "prose-init", InitMode: true})
	if res.Mode != apidoc.ModeInit || res.Stats.Added == 0 {
		t.Fatalf("无 meta 的散文文档勾选后应灌入: %+v", res)
	}

	// 未勾选 → 严格档（档位闸不拒，但严格校验会报缺 meta：不猜、不静默降级）
	pvStrict, err := svc.PreviewApiDocImport(ApiDocImportRequest{DocText: prose, ProjectName: "prose-strict"})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if pvStrict.Mode != apidoc.ModeIncremental || gatePreviewDenied(pvStrict) {
		t.Fatalf("无 meta + 未勾选应按 incremental 解析且不由档位闸拒收: %+v", pvStrict)
	}
	var hasSpecErr bool
	for _, is := range pvStrict.Issues {
		if is.Code == apidoc.ErrSpecBlockMissing {
			hasSpecErr = true
		}
	}
	if !hasSpecErr || pvStrict.OK {
		t.Fatalf("严格档缺 meta 块应报 E-SPEC-BLOCK-MISSING 并拦住: %+v", pvStrict.Issues)
	}
}
