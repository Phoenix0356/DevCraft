package appsvc

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"DevCraft/internal/agent"
	"DevCraft/internal/dockerx"
	"DevCraft/internal/secrets"
	"DevCraft/internal/skill"
	"DevCraft/internal/store"
)

// capturedEvent 记录一条聊天流帧（事件名 + 载荷）。
type capturedEvent struct {
	event   string
	payload map[string]any
}

// newDeployTestService 组装部署测试用 Service：临时 SQLite + 全量事件捕获。
func newDeployTestService(t *testing.T) (*Service, func() []capturedEvent) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := secrets.NewBox(dir)
	if err != nil {
		t.Fatalf("secrets box: %v", err)
	}
	skills := skill.NewRegistry()
	agents := agent.NewRegistry(st, skills)

	var mu sync.Mutex
	var events []capturedEvent
	svc := New(st, agents, skills, dockerx.NewManager(), box, func(event string, payload map[string]any) {
		mu.Lock()
		events = append(events, capturedEvent{event: event, payload: payload})
		mu.Unlock()
	})
	get := func() []capturedEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedEvent(nil), events...)
	}
	return svc, get
}

// withApprovalTTL 临时调小审批单有效期（生产值 10 分钟）。
func withApprovalTTL(t *testing.T, d time.Duration) {
	t.Helper()
	old := approvalTTL
	approvalTTL = d
	t.Cleanup(func() { approvalTTL = old })
}

// submitTestApproval 模拟部署技能生成审批单（3 步命令）。
func submitTestApproval(t *testing.T, svc *Service, sessionID string, commands []string) string {
	t.Helper()
	flow := store.DeployFlow{ID: 7, Name: "web-deploy", Description: "测试流程", Target: store.TargetLocal}
	if commands == nil {
		commands = []string{"step-1", "step-2", "step-3"}
	}
	id, err := svc.SubmitDeployApproval(sessionID, flow, map[string]string{"version": "1.0"}, commands)
	if err != nil {
		t.Fatalf("submit approval: %v", err)
	}
	return id
}

// waitForDeployDone 轮询等待 deploy:done 帧（异步执行的收尾信号）。
func waitForDeployDone(t *testing.T, get func() []capturedEvent) capturedEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range get() {
			if ev.event == EventDeployDone {
				return ev
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timeout waiting for deploy:done frame")
	return capturedEvent{}
}

// fakeRunnerFactory 构造记录调用并按需失败的执行器工厂。
// 命令中含 "FAIL" 的返回错误；含 "BLOCK" 的阻塞到 ctx 结束。
func fakeRunnerFactory(ran *[]string, mu *sync.Mutex) func(string) (StepRunner, string, error) {
	return func(string) (StepRunner, string, error) {
		return func(ctx context.Context, cmd string) (string, error) {
			mu.Lock()
			*ran = append(*ran, cmd)
			mu.Unlock()
			switch {
			case strings.Contains(cmd, "BLOCK"):
				<-ctx.Done()
				return "", ctx.Err()
			case strings.Contains(cmd, "FAIL"):
				return "partial output", errors.New("exit status 1")
			default:
				return "ok: " + cmd, nil
			}
		}, "fake-target", nil
	}
}

// TestApprovalGate 审批门禁（DoD 硬要求）：
// 未批准/拒绝后/未知单号都进不了执行路径，假执行器绝不被调用。
func TestApprovalGate(t *testing.T) {
	svc, _ := newDeployTestService(t)
	var mu sync.Mutex
	var ran []string
	svc.deployRunnerFor = fakeRunnerFactory(&ran, &mu)

	// ① 未知单号直接拒绝
	if err := svc.ApproveDeployment("no-such-id"); err == nil {
		t.Fatal("approving unknown id should fail")
	}

	// ② 已拒绝的单子不能再批准
	id := submitTestApproval(t, svc, "sess-1", nil)
	if err := svc.RejectDeployment(id); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := svc.ApproveDeployment(id); err == nil {
		t.Fatal("approving rejected approval should fail")
	}

	// ③ 重复拒绝也失败（单次消费）
	if err := svc.RejectDeployment(id); err == nil {
		t.Fatal("double reject should fail")
	}

	// ④ 新单子不批准、只放着：没有任何执行
	_ = submitTestApproval(t, svc, "sess-1", nil)
	mu.Lock()
	n := len(ran)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("runner called %d times without approval", n)
	}
}

// TestApprovalExpired 审批过期：超过 approvalTTL 未批准，批准动作被拒绝。
func TestApprovalExpired(t *testing.T) {
	withApprovalTTL(t, 5*time.Millisecond)
	svc, _ := newDeployTestService(t)
	var mu sync.Mutex
	var ran []string
	svc.deployRunnerFor = fakeRunnerFactory(&ran, &mu)

	id := submitTestApproval(t, svc, "sess-1", nil)
	time.Sleep(20 * time.Millisecond)
	err := svc.ApproveDeployment(id)
	if err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("approve expired = %v, want 已过期 error", err)
	}
	mu.Lock()
	n := len(ran)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("runner called after expiry: %d", n)
	}
}

// TestApprovalLazyPurge 过期单惰性回收：从不被批准/拒绝的过期单
// 在新审批单登记时被清理，注册表不无限累积。
func TestApprovalLazyPurge(t *testing.T) {
	withApprovalTTL(t, 5*time.Millisecond)
	svc, _ := newDeployTestService(t)

	stale := submitTestApproval(t, svc, "sess-1", nil)
	time.Sleep(20 * time.Millisecond)
	submitTestApproval(t, svc, "sess-1", nil) // 登记新单触发惰性清理

	svc.approvalsMu.Lock()
	n := len(svc.approvals)
	_, stalePresent := svc.approvals[stale]
	svc.approvalsMu.Unlock()
	if n != 1 || stalePresent {
		t.Fatalf("expired approval not purged: count=%d stalePresent=%v", n, stalePresent)
	}
}

// TestApproveAndExecuteSuccess 正常路径：批准 → 逐步执行 → 历史成功 → 帧齐全。
func TestApproveAndExecuteSuccess(t *testing.T) {
	svc, get := newDeployTestService(t)
	var mu sync.Mutex
	var ran []string
	svc.deployRunnerFor = fakeRunnerFactory(&ran, &mu)

	sess, err := svc.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	commands := []string{"cmd-a", "cmd-b"}
	id := submitTestApproval(t, svc, sess.ID, commands)

	// 批准前：审批卡片帧已推（含完整命令清单）
	foundApproval := false
	for _, ev := range get() {
		if ev.event == EventDeployApproval && ev.payload["approvalId"] == id {
			foundApproval = true
			if ev.payload["sessionId"] != sess.ID {
				t.Fatalf("approval frame sessionId = %v", ev.payload["sessionId"])
			}
			cmds, _ := ev.payload["commands"].([]string)
			if len(cmds) != 2 || cmds[0] != "cmd-a" {
				t.Fatalf("approval frame commands = %v", ev.payload["commands"])
			}
		}
	}
	if !foundApproval {
		t.Fatal("deploy:approval frame not emitted")
	}

	if err := svc.ApproveDeployment(id); err != nil {
		t.Fatalf("approve: %v", err)
	}
	done := waitForDeployDone(t, get)
	if done.payload["status"] != store.DeploySuccess {
		t.Fatalf("done status = %v", done.payload["status"])
	}
	if s, _ := done.payload["summary"].(string); !strings.Contains(s, "执行完成") || !strings.Contains(s, "2/2") {
		t.Fatalf("summary = %q", s)
	}

	mu.Lock()
	if len(ran) != 2 || ran[0] != "cmd-a" || ran[1] != "cmd-b" {
		t.Fatalf("executed commands = %v", ran)
	}
	mu.Unlock()

	// 进度帧：每步 start + done 各一条
	starts, dones := 0, 0
	for _, ev := range get() {
		if ev.event != EventDeployProgress {
			continue
		}
		switch ev.payload["status"] {
		case "start":
			starts++
		case "done":
			dones++
		}
	}
	if starts != 2 || dones != 2 {
		t.Fatalf("progress frames starts=%d dones=%d", starts, dones)
	}

	// 历史落库：成功 + 详情含命令
	msgs, _ := svc.Messages(sess.ID)
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content, "执行完成") {
		t.Fatalf("summary message not persisted: %+v", msgs)
	}
}

// TestExecuteStopsOnFailure 失败即中断：第 2 步失败 → 第 3 步绝不执行，
// 历史状态=失败，详情含已完成步骤与错误。
func TestExecuteStopsOnFailure(t *testing.T) {
	svc, get := newDeployTestService(t)
	var mu sync.Mutex
	var ran []string
	svc.deployRunnerFor = fakeRunnerFactory(&ran, &mu)

	sess, _ := svc.NewSession()
	id := submitTestApproval(t, svc, sess.ID, []string{"ok-1", "FAIL-2", "never-3"})
	if err := svc.ApproveDeployment(id); err != nil {
		t.Fatalf("approve: %v", err)
	}
	done := waitForDeployDone(t, get)
	if done.payload["status"] != store.DeployFailed {
		t.Fatalf("done status = %v", done.payload["status"])
	}

	mu.Lock()
	if len(ran) != 2 || ran[0] != "ok-1" || ran[1] != "FAIL-2" {
		t.Fatalf("executed = %v, want [ok-1 FAIL-2] (never-3 must not run)", ran)
	}
	mu.Unlock()

	// 失败步骤的进度帧带 failed 状态
	failedStep := false
	for _, ev := range get() {
		if ev.event == EventDeployProgress && ev.payload["status"] == "failed" {
			failedStep = true
			if out, _ := ev.payload["output"].(string); !strings.Contains(out, "partial output") {
				t.Fatalf("failed step output = %q", out)
			}
		}
	}
	if !failedStep {
		t.Fatal("no failed progress frame")
	}

	// 历史：失败 + 详情包含两步痕迹
	hist := lastHistory(t, svc)
	if hist.Status != store.DeployFailed {
		t.Fatalf("history status = %q", hist.Status)
	}
	if !strings.Contains(hist.Detail, "步骤 1/3 成功") || !strings.Contains(hist.Detail, "步骤 2/3 失败") {
		t.Fatalf("history detail = %q", hist.Detail)
	}
	if hist.FlowName != "web-deploy" || hist.Target != "fake-target" {
		t.Fatalf("history snapshot = %+v", hist)
	}
}

// lastHistory 取最近一条执行历史（新库自增 id 从 1 开始，顺序探测到缺失为止）。
func lastHistory(t *testing.T, svc *Service) store.DeployHistory {
	t.Helper()
	var last store.DeployHistory
	found := false
	for id := int64(1); ; id++ {
		h, err := svc.store.GetDeployHistory(id)
		if err != nil {
			break
		}
		last, found = h, true
	}
	if !found {
		t.Fatal("no history rows found")
	}
	return last
}

// TestCancelDeployViaCancelTurn 停止按钮可取消部署执行：
// 执行中调 CancelTurn → 状态 canceled、历史落库、帧收尾。
func TestCancelDeployViaCancelTurn(t *testing.T) {
	svc, get := newDeployTestService(t)
	var mu sync.Mutex
	var ran []string
	svc.deployRunnerFor = fakeRunnerFactory(&ran, &mu)

	sess, _ := svc.NewSession()
	id := submitTestApproval(t, svc, sess.ID, []string{"BLOCK-forever"})
	if err := svc.ApproveDeployment(id); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// 轮询等待执行登记进 turns 注册表（同会话键）
	deadline := time.Now().Add(2 * time.Second)
	for svc.activeTurns() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("deploy execution never registered in turns registry")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := svc.CancelTurn(sess.ID); err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}
	done := waitForDeployDone(t, get)
	if done.payload["status"] != store.DeployCanceled {
		t.Fatalf("done status = %v, want canceled", done.payload["status"])
	}
	if hist := lastHistory(t, svc); hist.Status != store.DeployCanceled {
		t.Fatalf("history status = %q", hist.Status)
	}
	// 执行结束必须清注册表（不泄漏取消句柄）
	deadline = time.Now().Add(2 * time.Second)
	for svc.activeTurns() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("turns registry not cleaned after deploy cancel")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestDeployExecTimeout 执行总超时：慢步骤超过 deployExecTimeout → failed。
func TestDeployExecTimeout(t *testing.T) {
	old := deployExecTimeout
	deployExecTimeout = 30 * time.Millisecond
	t.Cleanup(func() { deployExecTimeout = old })

	svc, get := newDeployTestService(t)
	var mu sync.Mutex
	var ran []string
	svc.deployRunnerFor = fakeRunnerFactory(&ran, &mu)

	sess, _ := svc.NewSession()
	id := submitTestApproval(t, svc, sess.ID, []string{"BLOCK-forever"})
	if err := svc.ApproveDeployment(id); err != nil {
		t.Fatalf("approve: %v", err)
	}
	done := waitForDeployDone(t, get)
	if done.payload["status"] != store.DeployFailed {
		t.Fatalf("done status = %v, want failed (timeout)", done.payload["status"])
	}
	if s, _ := done.payload["summary"].(string); !strings.Contains(s, "超时") {
		t.Fatalf("summary = %q, want containing 超时", s)
	}
}

// TestSSHTargetRequiresConfig SSH 目标未配置远程主机：批准时快速失败。
func TestSSHTargetRequiresConfig(t *testing.T) {
	svc, get := newDeployTestService(t)
	flow := store.DeployFlow{ID: 9, Name: "remote-deploy", Target: store.TargetSSH}
	id, err := svc.SubmitDeployApproval("sess-1", flow, nil, []string{"echo hi"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	err = svc.ApproveDeployment(id)
	if err == nil || !strings.Contains(err.Error(), "尚未配置远程主机") {
		t.Fatalf("approve without ssh config = %v", err)
	}
	done := waitForDeployDone(t, get)
	if done.payload["status"] != store.DeployFailed {
		t.Fatalf("done status = %v", done.payload["status"])
	}
}

// TestLocalShellInvocation 本机通道命令构造（两平台，纯函数可测）。
func TestLocalShellInvocation(t *testing.T) {
	// POSIX：/bin/sh -c 直传命令行
	name, args, script := localShellInvocation("linux", "docker restart web", "")
	if name != "/bin/sh" || len(args) != 2 || args[0] != "-c" || args[1] != "docker restart web" || script != "" {
		t.Fatalf("linux invocation = %s %v %q", name, args, script)
	}
	// Windows：临时批处理（@echo off + UTF-8 码页 + 命令行），退出码透传
	name, args, script = localShellInvocation("windows", "deploy.bat 1.2.3", `C:\tmp\d.bat`)
	if name != "cmd" || strings.Join(args, " ") != `/d /c C:\tmp\d.bat` {
		t.Fatalf("windows invocation = %s %v", name, args)
	}
	if !strings.HasPrefix(script, "@echo off\r\n") || !strings.Contains(script, "chcp 65001") {
		t.Fatalf("windows script header missing: %q", script)
	}
	if !strings.Contains(script, "deploy.bat 1.2.3\r\n") {
		t.Fatalf("windows script missing command: %q", script)
	}
}

// TestRunLocalStep 本机通道真实执行（开发机冒烟）：成功取输出、失败报错。
func TestRunLocalStep(t *testing.T) {
	svc, _ := newDeployTestService(t)
	out, err := svc.runLocalStep(context.Background(), "echo DEVCRAFT_LOCAL_OK")
	if err != nil {
		t.Fatalf("runLocalStep(echo): %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "DEVCRAFT_LOCAL_OK") {
		t.Fatalf("output missing marker: %q", out)
	}
	// 失败命令：非零退出码 → 错误（输出仍返回，供详情使用）
	_, err = svc.runLocalStep(context.Background(), "exit 3")
	if err == nil {
		t.Fatal("runLocalStep(exit 3) should fail")
	}
	// ctx 取消：阻塞命令被中断（无超时的阻塞调用是本项目明令禁止的）
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = svc.runLocalStep(ctx, blockingSleepCmd())
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("runLocalStep cancel: err=%v elapsed=%v", err, time.Since(start))
	}
}

// blockingSleepCmd 按平台返回一条阻塞约 5 秒的命令（测取消用；
// 命令本身会被 ctx 超时打断，残留子进程秒级自行结束，无泄漏）。
func blockingSleepCmd() string {
	if runtime.GOOS == "windows" {
		return "ping -n 5 127.0.0.1 >nul"
	}
	return "sleep 5"
}

// TestValidateDeployFlow 流程表单校验：名称/目标/步骤/参数名/正则可编译。
func TestValidateDeployFlow(t *testing.T) {
	ok := store.DeployFlow{
		Name:   "web",
		Target: store.TargetLocal,
		Params: []store.FlowParam{{Name: "version", Pattern: `\d+`}, {Name: "  "}}, // 空行被丢弃
		Steps:  []string{" echo hi ", "", "echo done"},                              // 去空白、丢空行
	}
	if err := validateDeployFlow(&ok); err != nil {
		t.Fatalf("valid flow rejected: %v", err)
	}
	if len(ok.Params) != 1 || len(ok.Steps) != 2 || ok.Steps[0] != "echo hi" {
		t.Fatalf("normalization failed: %+v", ok)
	}

	cases := []struct {
		name string
		flow store.DeployFlow
		want string
	}{
		{"empty name", store.DeployFlow{Target: store.TargetLocal, Steps: []string{"x"}}, "流程名称"},
		{"bad target", store.DeployFlow{Name: "a", Target: "k8s", Steps: []string{"x"}}, "部署目标"},
		{"no steps", store.DeployFlow{Name: "a", Target: store.TargetLocal}, "命令步骤"},
		{"bad param name", store.DeployFlow{Name: "a", Target: store.TargetLocal, Steps: []string{"x"}, Params: []store.FlowParam{{Name: "1abc"}}}, "参数名"},
		{"dup param", store.DeployFlow{Name: "a", Target: store.TargetLocal, Steps: []string{"x"}, Params: []store.FlowParam{{Name: "p"}, {Name: "p"}}}, "重复"},
		{"bad regex", store.DeployFlow{Name: "a", Target: store.TargetLocal, Steps: []string{"x"}, Params: []store.FlowParam{{Name: "p", Pattern: "("}}}, "正则"},
	}
	for _, c := range cases {
		err := validateDeployFlow(&c.flow)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

// TestSanitizeStepOutput 步骤输出先脱敏再截断（密钥绝不进帧/历史）。
func TestSanitizeStepOutput(t *testing.T) {
	out := sanitizeStepOutput("deploy ok\napi_key: supersecretvalue123")
	if strings.Contains(out, "supersecretvalue123") {
		t.Fatalf("secret leaked: %q", out)
	}
	long := sanitizeStepOutput(strings.Repeat("x", maxStepOutputChars+100))
	if len([]rune(long)) > maxStepOutputChars+30 { // 截断提示文案有少量富余
		t.Fatalf("output not capped: %d runes", len([]rune(long)))
	}
}
