package apidoc

import "testing"

// TestIdentityKey 身份键（D18）：不剥离 base_path、不做大小写归一、占位符折叠。
func TestIdentityKey(t *testing.T) {
	// GET /api/v1.0/user 与 base_path 逐字相等：绝不剥离（剥离会退化成空串）
	if got := IdentityKey("GET", "/api/v1.0/user"); got != "GET|/api/v1.0/user" {
		t.Fatalf("identity = %q", got)
	}
	// /arxiv/save 与 /arxiv/saveBatch 是两个真实接口：大小写不归一
	if IdentityKey("POST", "/arxiv/save") == IdentityKey("POST", "/arxiv/saveBatch") {
		t.Fatal("save 与 saveBatch 被静默合并——接口库会少一条对外契约")
	}
	// 占位符 {任意} → {}：{id} 与 {paperId} 同 key（占位符改名是 cosmetic）
	if IdentityKey("PUT", "/subscription/{id}") != IdentityKey("PUT", "/subscription/{paperId}") {
		t.Fatal("占位符变量名不应进身份")
	}
	if got := IdentityKey("put", "/a/{id}"); got != "PUT|/a/{}" {
		t.Fatalf("method 必须大写归一: %q", got)
	}
}

func TestNormPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/v1.0/user", "/api/v1.0/user"},
		{"/a/{id}/b/{paperId}", "/a/{}/b/{}"},
		{"/a//b/", "/a/b"}, // 折叠重复斜杠 + 去尾斜杠
		{"/", "/"},         // 根路径保留
		{"", ""},
		{"/a/{unclosed", "/a/{unclosed"},         // 未闭合花括号原样保留
		{"/arxiv/saveBatch", "/arxiv/saveBatch"}, // 大小写保留
	}
	for _, c := range cases {
		if got := NormPath(c.in); got != c.want {
			t.Fatalf("NormPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTypeKey(t *testing.T) {
	// List\<T\> / List<T> / T[] → 同一 key
	a, b, c := TypeKey(`List\<Long\>`), TypeKey("List<Long>"), TypeKey("Long[]")
	if a != b || b != c {
		t.Fatalf("List 归一失败: %q %q %q", a, b, c)
	}
	if a != "List<Long>" {
		t.Fatalf("归一形态应消除转义: %q", a)
	}
	// Boolean/boolean 各按原样保留在 raw、归一时统一
	if TypeKey("Boolean") != TypeKey("boolean") || TypeKey("boolean") != TypeKey("BOOLEAN") {
		t.Fatalf("Boolean/boolean 未统一: %q %q", TypeKey("Boolean"), TypeKey("boolean"))
	}
	if TypeKey("Boolean") != "Boolean" {
		t.Fatalf("Boolean 归一形态: %q", TypeKey("Boolean"))
	}
	// 语义不同的类型不合并
	if TypeKey("int") == TypeKey("Integer") {
		t.Fatal("int/Integer 语义不同，不应合并")
	}
	if TypeKey("long") != TypeKey("Long") || TypeKey("long") != TypeKey("Long ") {
		t.Fatal("long/Long 应统一（空白无关）")
	}
	// 整格 inline code 包裹的容错 + 实体反转义
	if TypeKey("`String`") != TypeKey("String") {
		t.Fatal("backtick 包裹应剥掉")
	}
	if TypeKey("List&lt;String&gt;") != TypeKey("List<String>") {
		t.Fatal("HTML 实体应反转义")
	}
	// 嵌套泛型
	if TypeKey(`IPage\<FavoriteVO\>`) != TypeKey("IPage<FavoriteVO>") {
		t.Fatal("嵌套泛型归一失败")
	}
}

// TestUnescapeTextSkipsInlineCode 反转义必须跳过 inline code 区间，否则改坏正文。
func TestUnescapeTextSkipsInlineCode(t *testing.T) {
	in := "取值 &lt;view&gt; 与 `code &lt;raw&gt; \\<x\\>` 以及 \\<esc\\>"
	want := "取值 <view> 与 `code &lt;raw&gt; \\<x\\>` 以及 <esc>"
	if got := UnescapeText(in); got != want {
		t.Fatalf("UnescapeText:\n got %q\nwant %q", got, want)
	}
	// 无实体/转义时原样
	if got := UnescapeText("子分类，多个用逗号分隔（如 cs.AI,cs.LG）"); got != "子分类，多个用逗号分隔（如 cs.AI,cs.LG）" {
		t.Fatalf("普通中文说明被改坏: %q", got)
	}
}
