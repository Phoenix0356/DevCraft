package apidoc

import (
	"html"
	"strings"
	"unicode"
)

// 合法 HTTP 方法（大写，spec-draft §3.1：URL 行是唯一硬规则）。
var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true,
}

// ValidMethod 报告 method 是否在合法大写集合内。
func ValidMethod(m string) bool { return validMethods[m] }

// IdentityKey 接口身份键（D18）：upper(METHOD) + "|" + normPath。
//
// 明确不做（都是实测踩过坑的方向）：
//   - 不剥离 base_path：GET /api/v1.0/user 与 base_path 逐字相等，剥离会退化成空串，
//     这条真实接口将永久无法入库。base_path 只做断言（W-BASEPATH-ASSERT-FAIL）。
//   - 不做大小写归一：/arxiv/save 与 /arxiv/saveBatch 是两个真实接口，
//     lowercase 会把它们静默合并——接口库少一条对外契约，事件详情还会显示「无变化」。
func IdentityKey(method, path string) string {
	return strings.ToUpper(strings.TrimSpace(method)) + "|" + NormPath(path)
}

// NormPath 路径归一，只做两件事：
//  1. 占位符折叠：{任意} → {}（所以 {id} 与 {paperId} 同 key，占位符改名是 cosmetic）；
//  2. 折叠重复斜杠、去尾部斜杠（根路径 "/" 保留）。
func NormPath(p string) string {
	p = strings.TrimSpace(p)
	// 占位符折叠（不用 regexp，手写扫描：{ 到最近的 } 之间替换为 {}）
	var b strings.Builder
	for i := 0; i < len(p); {
		if p[i] == '{' {
			if j := strings.IndexByte(p[i:], '}'); j >= 0 {
				b.WriteString("{}")
				i += j + 1
				continue
			}
		}
		b.WriteByte(p[i])
		i++
	}
	s := b.String()
	// 折叠重复斜杠
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	// 去尾部斜杠（根路径除外）
	if len(s) > 1 {
		s = strings.TrimRight(s, "/")
	}
	return s
}

// TypeKey 类型比较归一（双字段之一；type_raw 保留显示原文）：
//   - 消除泛型转义与 HTML 实体：List\<T\> / List&lt;T&gt; / List<T> → 同一 key；
//   - 数组写法归一：T[] → List<T>；
//   - 标识符首字母大写、其余小写：Boolean/boolean/BOOLEAN → Boolean，
//     long/Long → Long（「raw 各按原样保留、归一时统一」）；
//   - 去空白。
//
// 结果形如 List<Long> / Boolean / Int / Multipartfile（仅用于等值比较，展示请用 type_raw）。
func TypeKey(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "`") // 整格被 inline code 包裹的容错
	s = strings.ReplaceAll(s, "\\<", "<")
	s = strings.ReplaceAll(s, "\\>", ">")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\t", "")
	// T[] → List<T>（只处理最外层后缀写法，语料实测只有这一种数组形态）
	if strings.HasSuffix(s, "[]") {
		s = "List<" + s[:len(s)-2] + ">"
	}
	return normalizeTypeIdents(s)
}

// normalizeTypeIdents 对每个字母开头的标识符 run 做首字母大写 + 其余小写。
func normalizeTypeIdents(s string) string {
	var b strings.Builder
	inIdent := false
	for _, r := range s {
		if isIdentRune(r) {
			if !inIdent {
				b.WriteRune(unicode.ToUpper(r))
				inIdent = true
			} else {
				b.WriteRune(unicode.ToLower(r))
			}
			continue
		}
		inIdent = false
		b.WriteRune(r)
	}
	return b.String()
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// UnescapeText 反转义散文文本（HTML 实体 + \< \> 反斜杠转义）。
// ⚠️ 必须跳过 inline code（`...`）区间，否则会改坏正文
// （spec-draft §4.2：代码跨度里的 &lt;、\< 是字面内容，不是转义）。
func UnescapeText(s string) string {
	var b strings.Builder
	inCode := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '`' {
			inCode = !inCode
			b.WriteByte(c)
			continue
		}
		if inCode {
			b.WriteByte(c)
			continue
		}
		if c == '\\' && i+1 < len(s) && (s[i+1] == '<' || s[i+1] == '>' || s[i+1] == '\\' || s[i+1] == '`' || s[i+1] == '*' || s[i+1] == '_') {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		if c == '&' {
			if j := strings.IndexByte(s[i:], ';'); j > 0 && j <= 8 {
				if ent := s[i : i+j+1]; isHTMLEntity(ent) {
					b.WriteString(html.UnescapeString(ent))
					i += j
					continue
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// isHTMLEntity 只认解析场景常见的几个实体，避免把 & 开头的普通文本误伤。
func isHTMLEntity(ent string) bool {
	switch ent {
	case "&lt;", "&gt;", "&amp;", "&quot;", "&#39;", "&nbsp;":
		return true
	}
	return false
}
