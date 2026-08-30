// Package sanitize 提供日志的截断与敏感信息脱敏。
// 目的：原始日志送进 LLM 上下文之前，既要控制体量（省 token、提速度），
// 又要保证密钥/口令类内容绝不进入模型（安全底线）。
package sanitize

import (
	"regexp"
	"strings"
)

// TailLines 只保留字符串的最后 n 行（日志分析只关心最近的内容）。
func TailLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	lines := strings.Split(s, "\n") // ≈ String.split("\n")
	if len(lines) <= n {
		return s
	}
	// 切片截取 [起点:]：取倒数第 n 行到末尾，再用换行拼回
	return strings.Join(lines[len(lines)-n:], "\n")
}

// CapChars 按"字符数"（而非字节数）截断，超长时追加提示。
func CapChars(s string, max int) string {
	r := []rune(s) // string → []rune 转成 Unicode 码点切片：
	// Go 的 string 本质是字节序列，直接 s[:max] 可能把一个中文切成半个乱码，
	// 必须先转 rune（≈ Java 的 codePoint 视角）再截断。
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n...[内容过长已截断]"
}

// 三条脱敏正则。包级 var + MustCompile：启动时编译一次，全局复用。
// (?i) = 忽略大小写；{16,} = 至少 16 个字符；\s = 空白符。
var (
	// OpenAI 风格 API Key：sk- 开头 + 16 位以上字母数字
	reOpenAIKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)
	// Authorization 头：Bearer + 长 token
	reBearer = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	// 键值赋值型凭据：password=xxx / api_key: xxx / token=xxx 等。
	// 三个捕获组：①键名 ②分隔符(:/=及两侧空白) ③值
	reCredAssign = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|api[_-]?key|access[_-]?key|access[_-]?token|token|authorization)(\s*[:=]\s*)([^\s"']{4,})`)
)

// MaskSecrets 依次套用三条规则，把凭据值替换成 ***。
func MaskSecrets(s string) string {
	s = reOpenAIKey.ReplaceAllString(s, "sk-***")
	s = reBearer.ReplaceAllString(s, "Bearer ***")
	// 用 ReplaceAllStringFunc 自定义替换逻辑：
	// 特例处理——"Authorization: Bearer xxx" 里键值规则匹配到的"值"
	// 恰好是 Bearer 这个词本身（真 token 已被上一条规则掩掉），此时保留原文，
	// 避免把 "Bearer" 字样也替换掉造成 "Authorization: *** ***" 的怪结果。
	s = reCredAssign.ReplaceAllStringFunc(s, func(m string) string {
		parts := reCredAssign.FindStringSubmatch(m) // 取出三个捕获组
		if strings.EqualFold(parts[3], "bearer") {  // EqualFold = 忽略大小写比较
			return m // 原样返回
		}
		return parts[1] + parts[2] + "***" // 键名+分隔符保留，值掩掉
	})
	return s
}
