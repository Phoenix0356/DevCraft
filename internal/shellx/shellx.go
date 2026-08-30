// Package shellx 提供 shell 引号转义与命令模板占位符替换的通用工具。
//
// 背景：部署流程的命令模板里 {{参数}} 的值来自 LLM（外部输入），
// 替换进命令行之前必须按目标 shell 的规则做引号转义，否则就是注入漏洞。
// 本包把原来散落在 dockerx/ssh.go 的单引号转义泛化为两种 shell 的引用函数：
//   - QuotePOSIX：POSIX shell（远端 SSH 会话、/bin/sh -c），单引号引用，值任意
//   - QuoteWindows：Windows cmd.exe（本机部署通道），双引号引用；cmd 解析层无通用
//     转义，须先经 ValidateWindowsValue 拒绝双引号/换行/控制字符（见其注释）
//
// Java 类比：≈ 一个无状态的 StringUtils 工具类，纯函数、线程安全。
package shellx

import (
	"fmt"
	"regexp"
	"strings"
)

// QuotePOSIX 把任意字符串安全地引用成 POSIX shell 的一个词。
// 做法：整体用单引号包裹，内部单引号按 '\'' 转义（POSIX 标准做法）——
// 单引号内除单引号本身外一切字符都是字面量，分号/反引号/$()/换行全部失效。
// 空串得到 ''（合法的空参数），不会"消失"导致参数错位。
func QuotePOSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteWindows 把字符串按 CommandLineToArgvW 规则引用成 Windows 命令行中的一个词：
// 整体双引号包裹，内部双引号与反斜杠串按 2n/2n+1 规则转义，收尾反斜杠翻倍。
//
// 适用边界（重要）：该引用保证的是"目标进程用 CommandLineToArgvW 解析时还原出原值"。
// 但部署本机通道经过 cmd（cmd /d /c 批处理），cmd 在把命令行交给进程之前会自己解析一遍：
//   - cmd 的引用态只认裸 "（反斜杠不是它的转义符）——值里的 " 会在词内部闭合 cmd 的
//     引用态，其后的 & | < > 恢复元字符语义 → 代码执行；
//   - cmd/批处理按行解析——值里的换行直接另起一条命令行 → 代码执行；
//   - %VAR% 展开发生在引号解析之前且展开结果会被重新扫描——内置变量
//     %CMDCMDLINE% 恒有定义，路径含空格时其展开内容自带引号，可破裂引用态
//     放出值里预埋的元字符 → 代码执行；孤立 % 还会被批处理解析直接吞掉
//     （审批预览与实际执行不一致）。
//
// 因此 QuoteWindows 的安全前提是值先通过 ValidateWindowsValue（部署技能在 Windows
// 本机通道强制校验）：不含双引号/换行/百分号/控制字符时，双引号区域在 cmd 视角下
// 完整不破裂，引号内 & | < > ; ^ 全部字面化，两层解析才同时安全。
// cmd 没有通用转义机制，这是 cmd 的设计限制。对比 POSIX 路线（单引号内 100%
// 字面量），这是 cmd 设计限制的已知差距。
func QuoteWindows(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		// 数出当前位置连续反斜杠的个数
		n := 0
		for i < len(runes) && runes[i] == '\\' {
			n++
			i++
		}
		if i == len(runes) {
			// 结尾的反斜杠串：全部翻倍，避免转义掉收尾的引号
			b.WriteString(strings.Repeat(`\`, n*2))
			break
		}
		if runes[i] == '"' {
			// 反斜杠串后跟引号：反斜杠翻倍 + \"（字面引号）
			b.WriteString(strings.Repeat(`\`, n*2+1))
			b.WriteByte('"')
		} else {
			b.WriteString(strings.Repeat(`\`, n))
			b.WriteRune(runes[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ValidateWindowsValue 拒绝 Windows 本机（cmd）通道无法安全引用的参数值。
// QuoteWindows 防护成立的强制前提（部署技能在 Windows 本机通道替换前必调）：
//   - 双引号：cmd 引用态只认裸 "，值含 " 会闭合引用态放出元字符（实测可执行任意命令）；
//   - 换行/回车：cmd/批处理按行解析，换行会走私出一条独立命令（实测可执行任意命令）；
//   - 百分号：%VAR% 展开先于引号解析且结果被重新扫描——%CMDCMDLINE% 恒有定义，
//     路径含空格时展开自带引号，可破裂引用态放出预埋元字符（代码执行）；
//     孤立 % 则被批处理直接吞掉（实测 100% → 100，审批预览与实际执行不一致）；
//   - NUL 等控制字符：命令行/批处理中无合法表示。
//
// cmd 没有能同时骗过"cmd 解析 + CommandLineToArgvW"两层的通用转义，拒绝是唯一安全解。
// POSIX 通道不需要此校验（单引号内一切字面量，双引号/换行/% 都安全）。
func ValidateWindowsValue(s string) error {
	for _, r := range s {
		switch {
		case r == '"':
			return fmt.Errorf("含有双引号，Windows 命令行无法安全引用")
		case r == '\n' || r == '\r':
			return fmt.Errorf("含有换行符，Windows 命令行无法安全引用")
		case r == '%':
			return fmt.Errorf("含有百分号，Windows 命令行会做环境变量展开、无法安全引用")
		case r < 0x20: // NUL/ESC 等控制字符（含制表符）：部署参数无合法用途，从严拒绝
			return fmt.Errorf("含有控制字符，Windows 命令行无法安全引用")
		}
	}
	return nil
}

// rePlaceholder 匹配命令模板中的 {{参数名}} 占位符。
// 参数名字符集与流程保存时的校验规则一致（见 store.SaveDeployFlow）。
// 刻意不匹配 {{.Xxx}} / {{json .}} 这类形式——避免误伤 docker --format 等
// 用户命令里合法的双花括号语法（点号/空格使其不符合占位符文法，原样保留）。
var rePlaceholder = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_-]*)\s*\}\}`)

// Render 把命令模板中所有 {{参数}} 占位符替换为 quote(值) 的结果。
// quote 按目标 shell 选择（QuotePOSIX / QuoteWindows）——替换即转义，
// 这是参数注入防护的落点。引用了未提供值的占位符视为错误（拒绝执行整条命令）。
// 非占位符文本（模板本身是用户写的）原样保留，不做任何改动。
func Render(template string, values map[string]string, quote func(string) string) (string, error) {
	var firstErr error
	out := rePlaceholder.ReplaceAllStringFunc(template, func(m string) string {
		name := rePlaceholder.FindStringSubmatch(m)[1] // 取第一个捕获组（参数名）
		v, ok := values[name]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("命令引用了未提供的参数: {{%s}}", name)
			}
			return m // 保留原文，函数整体返回错误
		}
		return quote(v)
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}
