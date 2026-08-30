package sanitize

import (
	"strings"
	"testing"
)

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd"
	if got := TailLines(in, 2); got != "c\nd" {
		t.Fatalf("TailLines = %q", got)
	}
	if got := TailLines(in, 10); got != in {
		t.Fatalf("TailLines keep-all = %q", got)
	}
}

func TestCapChars(t *testing.T) {
	long := strings.Repeat("日", 100)
	got := CapChars(long, 10)
	if !strings.HasSuffix(got, "已截断]") {
		t.Fatalf("expected truncation notice, got %q", got)
	}
	if r := []rune(got); len(r) > 10+len("...[内容过长已截断]") {
		t.Fatalf("too long: %d runes", len(r))
	}
}

func TestMaskSecrets(t *testing.T) {
	cases := []struct{ in, want string }{
		{"key is sk-AbCdEf1234567890XYZ", "key is sk-***"},
		{"Authorization: Bearer abc.DEF-123_456789012345", "Authorization: Bearer ***"},
		{"password=hunter22", "password=***"},
		{"API_KEY: mySecretValue123", "API_KEY: ***"},
		{"no secrets here", "no secrets here"},
	}
	for _, c := range cases {
		if got := MaskSecrets(c.in); got != c.want {
			t.Errorf("MaskSecrets(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
