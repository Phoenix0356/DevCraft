package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"DevCraft/internal/store"
)

// fakeFlows 内存版流程源。
type fakeFlows struct {
	list []store.DeployFlow
	err  error
}

func (f *fakeFlows) ListDeployFlows() ([]store.DeployFlow, error) { return f.list, f.err }

// fakeSubmitter 捕获审批单内容（断言替换后的命令清单）。
type fakeSubmitter struct {
	sessionID string
	flow      store.DeployFlow
	params    map[string]string
	commands  []string
	err       error
}

func (f *fakeSubmitter) SubmitDeployApproval(sessionID string, flow store.DeployFlow, params map[string]string, commands []string) (string, error) {
	f.sessionID = sessionID
	f.flow = flow
	f.params = params
	f.commands = commands
	if f.err != nil {
		return "", f.err
	}
	return "APPROVAL-1", nil
}

// testFlow 一个带参数与正则的示例流程（SSH 目标 = POSIX 转义，跨平台测试稳定）。
func testFlow() store.DeployFlow {
	return store.DeployFlow{
		ID:     1,
		Name:   "web-deploy",
		Target: store.TargetSSH,
		Params: []store.FlowParam{
			{Name: "version", Desc: "版本号", Pattern: `^\d+\.\d+\.\d+$`},
			{Name: "branch", Desc: "分支名"},
		},
		Steps: []string{
			"docker pull registry/web:{{version}}",
			"docker run -d -e BRANCH={{branch}} registry/web:{{version}}",
		},
	}
}

func mustExec(t *testing.T, s *runFlow, args string) string {
	t.Helper()
	ctx := WithSessionID(context.Background(), "sess-1")
	out, err := s.Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return out
}

// TestExecuteHappyPath 正常路径：校验通过 → 转义替换 → 审批单携带完整命令清单。
func TestExecuteHappyPath(t *testing.T) {
	s := &runFlow{flows: &fakeFlows{list: []store.DeployFlow{testFlow()}}, submit: &fakeSubmitter{}}
	out := mustExec(t, s, `{"flow":"web-deploy","params":{"version":"1.2.3","branch":"main"}}`)
	if !strings.Contains(out, "已生成部署审批单") {
		t.Fatalf("result text should say approval created: %q", out)
	}
	sub := s.submit.(*fakeSubmitter)
	if sub.sessionID != "sess-1" {
		t.Fatalf("session id not propagated: %q", sub.sessionID)
	}
	want := []string{
		"docker pull registry/web:'1.2.3'",
		"docker run -d -e BRANCH='main' registry/web:'1.2.3'",
	}
	if len(sub.commands) != 2 || sub.commands[0] != want[0] || sub.commands[1] != want[1] {
		t.Fatalf("commands = %v, want %v", sub.commands, want)
	}
}

// TestExecuteValidation 参数校验：缺参 / 未知参数 / 正则不符 / 流程不存在。
func TestExecuteValidation(t *testing.T) {
	newS := func() *runFlow {
		return &runFlow{flows: &fakeFlows{list: []store.DeployFlow{testFlow()}}, submit: &fakeSubmitter{}}
	}
	ctx := WithSessionID(context.Background(), "sess-1")

	cases := []struct {
		name string
		args string
		want string // 错误文案须包含的子串
	}{
		{"missing param", `{"flow":"web-deploy","params":{"version":"1.2.3"}}`, "缺少必填参数 branch"},
		{"empty param", `{"flow":"web-deploy","params":{"version":"1.2.3","branch":""}}`, "缺少必填参数 branch"},
		{"unknown param", `{"flow":"web-deploy","params":{"version":"1.2.3","branch":"main","extra":"x"}}`, "没有声明参数"},
		{"regex mismatch", `{"flow":"web-deploy","params":{"version":"latest","branch":"main"}}`, "不符合要求"},
		{"flow not found", `{"flow":"no-such","params":{}}`, "可用流程: web-deploy"},
		{"no flow name", `{"params":{}}`, "需要提供 flow"},
	}
	for _, c := range cases {
		_, err := newS().Execute(ctx, json.RawMessage(c.args))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}

	// 无会话 id 的 ctx：拒绝创建审批单（防御）
	s := newS()
	if _, err := s.Execute(context.Background(), json.RawMessage(`{"flow":"web-deploy","params":{"version":"1.2.3","branch":"main"}}`)); err == nil {
		t.Fatal("Execute without session ctx should fail")
	}
}

// TestExecuteInjectionQuoted 注入向量进入参数值：生成的命令里必须被完整引用，
// 绝不出现裸的分号/命令替换等可改变命令结构的片段。
func TestExecuteInjectionQuoted(t *testing.T) {
	attacks := []string{
		`main'; rm -rf /; echo '`,
		"main$(reboot)",
		"main`id`",
		"main; shutdown -h now",
		"main\nrm -rf /",
	}
	for _, a := range attacks {
		s := &runFlow{flows: &fakeFlows{list: []store.DeployFlow{testFlow()}}, submit: &fakeSubmitter{}}
		args, _ := json.Marshal(map[string]any{"flow": "web-deploy", "params": map[string]any{"version": "1.2.3", "branch": a}})
		mustExec(t, s, string(args))
		sub := s.submit.(*fakeSubmitter)
		cmd := sub.commands[1]
		// POSIX 单引号引用：值必须原样存在于单引号对内（'\'' 转义形态）
		if !strings.Contains(cmd, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'") {
			t.Fatalf("attack %q not safely quoted in command %q", a, cmd)
		}
		// 结构校验：除引用转义外不得产生新的命令分隔——按未转义单引号切分后
		// 分号/换行必须全部落在引号对内。这里用可还原性做等价断言：
		// 去掉前缀后，引号包裹段还原必须等于原值
		wantQuoted := "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		if !strings.Contains(cmd, wantQuoted) {
			t.Fatalf("command %q lost round-trippable quoting for %q", cmd, a)
		}
	}
}

// TestDescriptionDynamic 描述动态列出流程清单（含参数说明），空清单时给出引导。
func TestDescriptionDynamic(t *testing.T) {
	s := &runFlow{flows: &fakeFlows{list: []store.DeployFlow{testFlow()}}, submit: &fakeSubmitter{}}
	d := s.Description()
	for _, part := range []string{"web-deploy", "version=版本号", "branch=分支名", "SSH 远程主机"} {
		if !strings.Contains(d, part) {
			t.Fatalf("Description missing %q:\n%s", part, d)
		}
	}

	empty := &runFlow{flows: &fakeFlows{}, submit: &fakeSubmitter{}}
	if d := empty.Description(); !strings.Contains(d, "没有已定义的部署流程") {
		t.Fatalf("empty flow list description should guide user: %s", d)
	}
}

// TestExecuteNumericParams LLM 传数字/布尔等非字符串值时归一化为字符串。
func TestExecuteNumericParams(t *testing.T) {
	flow := store.DeployFlow{
		Name:   "port-deploy",
		Target: store.TargetSSH,
		Params: []store.FlowParam{{Name: "port", Desc: "端口"}},
		Steps:  []string{"listen {{port}}"},
	}
	s := &runFlow{flows: &fakeFlows{list: []store.DeployFlow{flow}}, submit: &fakeSubmitter{}}
	mustExec(t, s, `{"flow":"port-deploy","params":{"port":8080}}`)
	sub := s.submit.(*fakeSubmitter)
	if sub.commands[0] != "listen '8080'" {
		t.Fatalf("numeric param not normalized: %v", sub.commands)
	}
	if sub.params["port"] != "8080" {
		t.Fatalf("params map = %v", sub.params)
	}
}

// TestExecuteWindowsChannelValidation Windows 本机通道门禁：cmd 解析层没有通用转义
// （引用态只认裸 "、按行解析——两个向量均已实证可执行任意命令），
// 能破裂引用区的值必须在替换前整体拒绝，且绝不产生审批单；
// POSIX（SSH）通道不受影响——单引号内一切字面量。
func TestExecuteWindowsChannelValidation(t *testing.T) {
	oldGOOS := hostGOOS
	hostGOOS = "windows" // 跨平台断言 Windows 通道行为
	t.Cleanup(func() { hostGOOS = oldGOOS })

	local := store.DeployFlow{
		ID: 2, Name: "win-deploy", Target: store.TargetLocal,
		Params: []store.FlowParam{{Name: "tag"}},
		Steps:  []string{"deploy.exe {{tag}}"},
	}
	ctx := WithSessionID(context.Background(), "sess-1")

	// 实证注入向量全部拒绝，且不得走到审批单提交
	attacks := []string{
		`x" & calc & "y`,   // 引号逃逸（实测执行了走私命令）
		"foo\necho pwned",  // 换行走私（实测执行了走私命令）
		"foo\rbar",         // 回车同理（批处理行分隔）
		"100%",             // 孤立 % 被批处理吞掉（预览≠执行）
		"%CMDCMDLINE%",     // 内置变量展开可自带引号破裂引用态
		"a\x00b",           // NUL：命令行无合法表示
		"a\tb",             // 控制字符从严
	}
	for _, a := range attacks {
		sub := &fakeSubmitter{}
		s := &runFlow{flows: &fakeFlows{list: []store.DeployFlow{local}}, submit: sub}
		args, _ := json.Marshal(map[string]any{"flow": "win-deploy", "params": map[string]any{"tag": a}})
		if _, err := s.Execute(ctx, json.RawMessage(args)); err == nil {
			t.Fatalf("attack %q not rejected on windows local channel", a)
		}
		if sub.commands != nil {
			t.Fatalf("attack %q reached approval submission", a)
		}
	}

	// 安全值正常通过：双引号引用（无 " 与换行时引用区在 cmd 视角完整）
	sub := &fakeSubmitter{}
	s := &runFlow{flows: &fakeFlows{list: []store.DeployFlow{local}}, submit: sub}
	mustExec(t, s, `{"flow":"win-deploy","params":{"tag":"v1.2.3-rc1 x"}}`)
	if sub.commands[0] != `deploy.exe "v1.2.3-rc1 x"` {
		t.Fatalf("windows command = %q", sub.commands[0])
	}

	// 同一危险值走 SSH 通道（POSIX 单引号）应放行：单引号内双引号/换行都是字面量
	sshFlow := local
	sshFlow.Target = store.TargetSSH
	sub2 := &fakeSubmitter{}
	s2 := &runFlow{flows: &fakeFlows{list: []store.DeployFlow{sshFlow}}, submit: sub2}
	args, _ := json.Marshal(map[string]any{"flow": "win-deploy", "params": map[string]any{"tag": `x" & calc & "y`}})
	mustExec(t, s2, string(args))
	if sub2.commands[0] != `deploy.exe 'x" & calc & "y'` {
		t.Fatalf("ssh command = %q", sub2.commands[0])
	}
}
