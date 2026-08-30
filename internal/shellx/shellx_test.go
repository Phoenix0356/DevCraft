package shellx

import (
	"strings"
	"testing"
)

// TestQuotePOSIXInjection 单引号转义防注入：任何特殊字符进入单引号后都是字面量。
// 断言策略：① 结果以单引号包裹 ② 内部单引号按 '\'' 转义 ③ 不含裸的（引号外的）
// 危险字符——即把结果按未转义单引号切开，奇偶段落在引号内外交替。
func TestQuotePOSIXInjection(t *testing.T) {
	attacks := []string{
		`'; rm -rf /; echo '`, // 单引号逃逸 + 分号
		`"double"`,             // 双引号
		"line1\nline2",         // 换行
		"$(whoami)",            // 命令替换
		"`id`",                 // 反引号
		"a && b | c > d",       // 元字符组合
		`-rf /`,                // 前导连字符（保持为独立词，不会被吞掉）
		``,                     // 空串 → ''
		"$HOME",                // 变量展开
	}
	for _, a := range attacks {
		q := QuotePOSIX(a)
		if !strings.HasPrefix(q, "'") || !strings.HasSuffix(q, "'") {
			t.Fatalf("QuotePOSIX(%q) = %q, not single-quote wrapped", a, q)
		}
		// 还原验证：把 '\'' 转义序列替换回单引号、去掉首尾引号，必须得到原文
		inner := q[1 : len(q)-1]
		if got := strings.ReplaceAll(inner, `'\''`, "'"); got != a {
			t.Fatalf("QuotePOSIX(%q) = %q, cannot round-trip to original", a, q)
		}
		// 结构校验：按未转义单引号切段，段数必为偶数（内容全在引号对内），
		// 即不存在能闭合引号的裸单引号 → 无法逃逸出引用上下文
		if n := strings.Count(inner, "'"); n%3 != 0 {
			t.Fatalf("QuotePOSIX(%q) = %q has unbalanced quotes", a, q)
		}
	}
	// 显式断言经典转义形态
	if q := QuotePOSIX("it's"); q != `'it'\''s'` {
		t.Fatalf(`QuotePOSIX("it's") = %q, want 'it'\''s'`, q)
	}
	if q := QuotePOSIX(""); q != "''" {
		t.Fatalf("QuotePOSIX(empty) = %q, want ''", q)
	}
}

// TestQuoteWindowsInjection cmd 双引号引用：引号内 & | < > ; ^ 与换行全部字面化；
// 内嵌引号/反斜杠按 CommandLineToArgvW 规则转义，保证词边界不被打破。
func TestQuoteWindowsInjection(t *testing.T) {
	attacks := []string{
		`" & calc & "`,          // 双引号逃逸 + &
		`a; rm -rf /`,           // 分号
		"`whoami`",              // 反引号（cmd 不识别，但仍须在词内）
		"$(calc)",               // POSIX 命令替换（须保持字面）
		"line1 & line2\nline3",  // 元字符 + 换行
		`a|b>c<d^e`,             // cmd 元字符全家桶
		``,                      // 空串 → ""
		`trailing backslash\`,   // 结尾反斜杠不得转义收尾引号
		`"`,                     // 纯引号
	}
	for _, a := range attacks {
		q := QuoteWindows(a)
		if !strings.HasPrefix(q, `"`) || !strings.HasSuffix(q, `"`) {
			t.Fatalf("QuoteWindows(%q) = %q, not double-quote wrapped", a, q)
		}
		// 词边界校验：收尾引号前的反斜杠个数必须是偶数
		// （奇数个会把收尾引号转义掉，词边界被打破 = 注入可能）
		body := q[:len(q)-1]
		trailing := len(body) - len(strings.TrimRight(body, `\`))
		if trailing%2 != 0 {
			t.Fatalf("QuoteWindows(%q) = %q: odd trailing backslashes break the closing quote", a, q)
		}
		// 还原验证：按 CommandLineToArgvW 规则解析回原文
		if got := parseArgvW(t, q); got != a {
			t.Fatalf("QuoteWindows(%q) = %q, round-trip = %q", a, q, got)
		}
	}
}

// parseArgvW 按 CommandLineToArgvW 的核心规则解析一个带引号的词
// （测试辅助：独立实现一遍解析规则，与被测的转义函数互为镜像）。
// 规则：引号内切换 in/out；2n+1 个反斜杠后跟引号 → n 个反斜杠 + 字面引号；
// 2n 个反斜杠后跟引号 → n 个反斜杠 + 切换 in/out。
func parseArgvW(t *testing.T, q string) string {
	t.Helper()
	var b strings.Builder
	runes := []rune(q)
	inQuotes := false
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '\\':
			n := 0
			for i < len(runes) && runes[i] == '\\' {
				n++
				i++
			}
			if i < len(runes) && runes[i] == '"' {
				b.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					b.WriteByte('"') // 奇数个：引号是字面量
				} else {
					inQuotes = !inQuotes // 偶数个：引号切换引用态
				}
			} else {
				b.WriteString(strings.Repeat(`\`, n))
				if i < len(runes) {
					b.WriteRune(runes[i])
				}
			}
		case runes[i] == '"':
			inQuotes = !inQuotes
		default:
			b.WriteRune(runes[i])
		}
	}
	if inQuotes {
		t.Fatalf("unterminated quotes in %q", q)
	}
	return b.String()
}

// TestValidateWindowsValue cmd 通道值门禁：能破裂 cmd 引用区的值必须被拒绝
// （QuoteWindows 防护成立的强制前提），其余值放行。
func TestValidateWindowsValue(t *testing.T) {
	// 拒绝：双引号（cmd 只认裸 "，会闭合引用态放出元字符）、
	// 换行/回车（cmd 按行解析，走私独立命令）、百分号（%VAR% 展开先于引号解析：
	// 孤立 % 被吞掉实测 100%→100；%CMDCMDLINE% 恒有定义，路径含空格时展开自带
	// 引号可破裂引用态）、控制字符（无合法表示）
	reject := []string{
		`"`,
		`x" & calc & "y`,   // 引号逃逸实证向量
		"foo\necho pwned",  // 换行走私实证向量
		"foo\rbar",
		"100%",              // 孤立 % 被批处理吞掉（实证往返 100% → 100）
		"%CMDCMDLINE%",      // 内置变量展开自带引号 → 可放出预埋元字符
		"a\x00b",
		"a\x1b[31mb",
		"a\tb",
	}
	for _, v := range reject {
		if err := ValidateWindowsValue(v); err == nil {
			t.Fatalf("ValidateWindowsValue(%q) should reject", v)
		}
	}
	// 放行：不含上述字符时，双引号区域在 cmd 视角下完整，引号内元字符全部字面化
	accept := []string{
		"",
		"1.2.3",
		"feature/x_y-z",
		"v1.0 rc1 with spaces",
		"中文参数值",
		`it's`,         // 单引号对 cmd 无意义
		"^weird & | <>", // 引号内全部字面量
		`C:\Program Files\app`,
	}
	for _, v := range accept {
		if err := ValidateWindowsValue(v); err != nil {
			t.Fatalf("ValidateWindowsValue(%q) = %v, should accept", v, err)
		}
	}
}

// TestRender 占位符替换：正常替换、重复占位符、缺参报错、
// 非占位符花括号（{{.Server.Version}}）原样保留。
func TestRender(t *testing.T) {
	values := map[string]string{"version": "1.2.3", "app": "web"}

	out, err := Render("deploy.sh {{version}} --app {{app}} --tag {{version}}", values, QuotePOSIX)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "deploy.sh '1.2.3' --app 'web' --tag '1.2.3'"
	if out != want {
		t.Fatalf("Render = %q, want %q", out, want)
	}

	// 空白容错：{{ version }} 等价 {{version}}
	out, err = Render("echo {{ version }}", values, QuotePOSIX)
	if err != nil || out != "echo '1.2.3'" {
		t.Fatalf("Render(spaced) = %q err=%v", out, err)
	}

	// 缺参必须报错（整条命令拒绝）
	if _, err := Render("echo {{missing}}", values, QuotePOSIX); err == nil {
		t.Fatal("Render with missing param should fail")
	}

	// docker --format 风格的双花括号不是占位符，原样保留
	out, err = Render("docker version --format '{{.Server.Version}}'", values, QuotePOSIX)
	if err != nil || out != "docker version --format '{{.Server.Version}}'" {
		t.Fatalf("Render(docker format) = %q err=%v", out, err)
	}

	// 无占位符的命令原样通过
	out, err = Render("systemctl restart nginx", values, QuotePOSIX)
	if err != nil || out != "systemctl restart nginx" {
		t.Fatalf("Render(plain) = %q err=%v", out, err)
	}
}

// TestRenderInjectionValues 注入值替换后仍被完整引用：
// 单引号/双引号/分号/反引号/换行/$() 全部向量在两种 shell 下都不可逃逸。
func TestRenderInjectionValues(t *testing.T) {
	payloads := []string{
		`'; touch /tmp/pwned; echo '`,
		`" ; calc ; "`,
		"a; rm -rf /",
		"`reboot`",
		"$(shutdown -h now)",
		"x\nrm -rf /",
	}
	for _, p := range payloads {
		values := map[string]string{"v": p}
		posix, err := Render("run {{v}}", values, QuotePOSIX)
		if err != nil {
			t.Fatalf("Render posix: %v", err)
		}
		// POSIX：替换结果必须是单个完整的单引号词（可无损还原）
		if !strings.HasPrefix(posix, "run '") {
			t.Fatalf("posix render = %q, value not quoted", posix)
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(posix, "run '"), "'")
		if got := strings.ReplaceAll(inner, `'\''`, "'"); got != p {
			t.Fatalf("posix round-trip mismatch: %q", posix)
		}

		win, err := Render("run {{v}}", values, QuoteWindows)
		if err != nil {
			t.Fatalf("Render windows: %v", err)
		}
		if got := parseArgvW(t, strings.TrimPrefix(win, "run ")); got != p {
			t.Fatalf("windows round-trip mismatch: %q", win)
		}
	}
}
