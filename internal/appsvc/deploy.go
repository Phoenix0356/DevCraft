// deploy.go 是一键部署的编排实现：待审批单注册表（审批状态机）+
// 批准后的异步执行器（本机 / SSH 双通道）+ 流程 CRUD 校验。
//
// 状态机（审批单）：
//
//	技能调用生成 ──► pending（内存注册表，approvalTTL 后过期）
//	pending ──批准──► executing（goroutine 异步，deployExecTimeout 兜底）──► success/failed/canceled（历史落库）
//	pending ──拒绝──► rejected（绝不执行）
//	pending ──超时──► expired（惰性清理，绝不执行）
//
// 安全门禁：执行入口只接受从注册表"取出即删除"的审批单（takeApproval），
// 未批准/重复批准/已过期/已拒绝都进不了执行路径——这是代码强制的，
// 不依赖调用方自觉。
//
// 长耗时防护（吸取"回合永久挂死"的教训）：执行 goroutine 挂独立超时
// （deployExecTimeout），并把 cancel 登记进会话级 turns 注册表——
// 前端"停止"按钮（CancelTurn）同样能取消进行中的部署执行。
package appsvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"DevCraft/internal/sanitize"
	"DevCraft/internal/store"
)

// 部署域的聊天流事件名（帧形与 chat:* 一致：{"event","payload"}，payload 带 sessionId）。
const (
	EventDeployApproval = "deploy:approval" // 审批卡片（流程名/参数/完整命令清单）
	EventDeployProgress = "deploy:progress" // 步骤进度（序号/状态/输出摘要）
	EventDeployDone     = "deploy:done"     // 执行结束（状态 + 总结文本）
)

// 审批单有效期与部署执行总上限。包级变量（非常量）便于单测调小。
var (
	approvalTTL       = 10 * time.Minute // 审批单未批准自动过期
	deployExecTimeout = 5 * time.Minute  // 单次部署执行的总超时
)

// 输出体量防护：步骤输出先脱敏再截断，才允许进帧/历史/会话。
const (
	maxStepOutputChars    = 2000 // 单步输出上限（历史详情里）
	progressOutputChars   = 300  // 进度帧里的输出摘要上限
	maxHistoryDetailChars = 8000 // 历史 detail 总上限
)

// maxFlowSteps 单个流程允许的命令步骤数上限（防御异常表单）。
const maxFlowSteps = 100

// Approval 待审批单（内存态，无需落库；会话绑定 + 过期时间）。
type Approval struct {
	ID        string            `json:"id"`
	SessionID string            `json:"sessionId"`
	FlowID    int64             `json:"flowId"`
	FlowName  string            `json:"flowName"`
	Target    string            `json:"target"` // store.TargetLocal | store.TargetSSH
	Params    map[string]string `json:"params"`
	Commands  []string          `json:"commands"` // 占位符已替换、已转义的最终命令清单
	CreatedAt int64             `json:"createdAt"`
	ExpiresAt int64             `json:"expiresAt"`
}

// expired 审批单是否已过期（惰性判定，不依赖后台定时器）。
func (a *Approval) expired() bool { return time.Now().UnixMilli() > a.ExpiresAt }

// ==================== 审批状态机 ====================

// SubmitDeployApproval 供部署技能调用：登记待审批单并向会话推送审批卡片帧。
// 实现 deploy.ApprovalSubmitter 接口（由 app.go 注入，避免循环依赖）。
func (s *Service) SubmitDeployApproval(sessionID string, flow store.DeployFlow, params map[string]string, commands []string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("内部错误: 缺少会话 ID，无法创建审批单")
	}
	if len(commands) == 0 {
		return "", fmt.Errorf("部署流程 %q 没有可执行的命令步骤", flow.Name)
	}
	now := time.Now().UnixMilli()
	ap := &Approval{
		ID:        uuid.NewString(),
		SessionID: sessionID,
		FlowID:    flow.ID,
		FlowName:  flow.Name,
		Target:    flow.Target,
		Params:    params,
		Commands:  commands,
		CreatedAt: now,
		ExpiresAt: now + approvalTTL.Milliseconds(),
	}
	s.approvalsMu.Lock()
	// 惰性清理：从不被批准/拒绝的过期单若不清理会永久滞留注册表
	// （消费路径的过期判定只是门禁，不承担回收职责）
	for id, a := range s.approvals {
		if now > a.ExpiresAt {
			delete(s.approvals, id)
		}
	}
	s.approvals[ap.ID] = ap
	s.approvalsMu.Unlock()

	s.emit(EventDeployApproval, map[string]any{
		"sessionId":   sessionID,
		"approvalId":  ap.ID,
		"flowId":      flow.ID,
		"flowName":    flow.Name,
		"description": flow.Description,
		"target":      flow.Target,
		"params":      params,
		"commands":    commands,
		"expiresAt":   ap.ExpiresAt,
	})
	slog.Info("部署审批单已创建", "approval", ap.ID, "flow", flow.Name, "session", sessionID, "steps", len(commands))
	return ap.ID, nil
}

// takeApproval 审批门禁：取出并原子删除审批单（单次消费）。
// 未找到（不存在/已处理）、已过期都返回错误——执行路径只可能从这里进入。
func (s *Service) takeApproval(id string) (*Approval, error) {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	ap := s.approvals[id]
	if ap == nil {
		return nil, fmt.Errorf("审批单不存在或已处理")
	}
	delete(s.approvals, id) // 取出即删除（单次消费）：重复批准/拒绝必然第二次失败
	if ap.expired() {
		return nil, fmt.Errorf("审批单已过期（有效期 %d 分钟），请在对话中重新发起部署", int(approvalTTL.Minutes()))
	}
	return ap, nil
}

// ApproveDeployment 批准部署（前端审批卡片"批准"按钮的落点）。
// 批准瞬间返回；真正的执行在 goroutine 中异步进行，进度与结果经聊天流帧回报。
func (s *Service) ApproveDeployment(id string) error {
	ap, err := s.takeApproval(id)
	if err != nil {
		return err
	}
	runner, targetDesc, err := s.deployRunnerFor(ap.Target)
	if err != nil {
		// 审批单已被消费：把失败原因作为终态帧回报，前端不会悬着
		s.finishDeploy(ap, nil, store.DeployFailed, fmt.Sprintf("部署准备失败: %v", err))
		return err
	}
	hist := &store.DeployHistory{
		FlowID:    ap.FlowID,
		FlowName:  ap.FlowName,
		Params:    ap.Params,
		SessionID: ap.SessionID,
		Target:    targetDesc,
	}
	if err := s.store.InsertDeployHistory(hist); err != nil {
		s.finishDeploy(ap, nil, store.DeployFailed, fmt.Sprintf("部署准备失败: 无法写入执行历史: %v", err))
		return err
	}
	slog.Info("部署已批准，开始异步执行", "approval", ap.ID, "flow", ap.FlowName, "target", targetDesc, "steps", len(ap.Commands))
	go s.executeApproval(ap, runner, hist)
	return nil
}

// RejectDeployment 拒绝部署（前端"拒绝"按钮）。拒绝的单子绝不执行，
// 也不落执行历史（历史只记录真正发起过的执行）。
func (s *Service) RejectDeployment(id string) error {
	ap, err := s.takeApproval(id)
	if err != nil {
		return err
	}
	slog.Info("部署已拒绝", "approval", ap.ID, "flow", ap.FlowName)
	// rejected 帧让所有打开该会话的标签页同步卡片状态（点击者之外的旁观页）
	s.emit(EventDeployDone, map[string]any{
		"sessionId":  ap.SessionID,
		"approvalId": ap.ID,
		"status":     "rejected",
		"summary":    "",
	})
	return nil
}

// ==================== 异步执行 ====================

// StepRunner 执行一条（已替换参数的）完整命令行，返回合并输出。
// 函数类型抽象执行通道：真实实现是本机 shell / SSH，测试注入假执行器。
type StepRunner func(ctx context.Context, cmd string) (string, error)

// executeApproval 逐步执行已批准的部署单（goroutine 入口）。
// 防护三件套：独立超时（deployExecTimeout）、turns 注册表接管取消（停止按钮）、
// 每步前检查 ctx——与回合超时同一套"绝不永久挂起"纪律。
func (s *Service) executeApproval(ap *Approval, runner StepRunner, hist *store.DeployHistory) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), deployExecTimeout)
	// 登记进会话级取消注册表（与回合同键）：CancelTurn 也能取消部署执行。
	// 结束按指针身份注销，避免误删同会话后续新回合/新部署的条目。
	entry := &turnEntry{cancel: cancel}
	s.turnsMu.Lock()
	s.turns[ap.SessionID] = entry
	s.turnsMu.Unlock()
	defer func() {
		s.turnsMu.Lock()
		if s.turns[ap.SessionID] == entry {
			delete(s.turns, ap.SessionID)
		}
		s.turnsMu.Unlock()
		cancel()
	}()
	// panic 兜底：goroutine 里的崩溃必须落终态，否则前端卡片永远悬着
	defer func() {
		if r := recover(); r != nil {
			slog.Error("部署执行 panic", "approval", ap.ID, "panic", r, "stack", string(debug.Stack()))
			s.finishDeploy(ap, hist, store.DeployFailed, fmt.Sprintf("部署流程「%s」执行遇到内部错误，已中止。", ap.FlowName))
		}
	}()

	total := len(ap.Commands)
	var logBuf strings.Builder // 步骤日志 → 历史 detail（输出已脱敏截断）
	status := store.DeploySuccess
	doneCount := 0
	failInfo := "" // 失败/中断时的错误说明（已含脱敏输出）

	for i, cmd := range ap.Commands {
		stepNo := i + 1
		// 每步开始前显式检查：已取消/超时立即终止，不再发起新命令
		if err := ctx.Err(); err != nil {
			status, failInfo = interruptOutcome(err, stepNo, total)
			break
		}
		s.emitDeployProgress(ap, stepNo, total, "start", cmd, "")
		out, runErr := runner(ctx, cmd)
		clean := sanitizeStepOutput(out)

		if runErr != nil && ctx.Err() != nil {
			// 取消/超时导致的失败：归类为中断而非普通命令失败
			status, failInfo = interruptOutcome(ctx.Err(), stepNo, total)
			fmt.Fprintf(&logBuf, "步骤 %d/%d 被中断: %s\n  命令: %s\n  输出: %s\n", stepNo, total, failInfo, cmd, clean)
			s.emitDeployProgress(ap, stepNo, total, "failed", cmd, truncate(clean, progressOutputChars))
			break
		}
		if runErr != nil {
			status = store.DeployFailed
			failInfo = fmt.Sprintf("%v\n输出: %s", runErr, clean)
			fmt.Fprintf(&logBuf, "步骤 %d/%d 失败: %v\n  命令: %s\n  输出: %s\n", stepNo, total, runErr, cmd, clean)
			s.emitDeployProgress(ap, stepNo, total, "failed", cmd, truncate(clean, progressOutputChars))
			break
		}
		doneCount = stepNo
		fmt.Fprintf(&logBuf, "步骤 %d/%d 成功\n  命令: %s\n  输出: %s\n", stepNo, total, cmd, clean)
		s.emitDeployProgress(ap, stepNo, total, "done", cmd, truncate(clean, progressOutputChars))
	}

	summary := buildDeploySummary(ap, status, doneCount, total, failInfo, time.Since(start))
	// 历史终态落库：detail = 逐步日志，整体再脱敏一次（命令里可能带敏感参数值）
	// 并按字符封顶，供审计与排错
	hist.Status = status
	hist.Detail = sanitize.CapChars(sanitize.MaskSecrets(logBuf.String()), maxHistoryDetailChars)
	_ = s.store.UpdateDeployHistory(*hist)
	// 总结作为 assistant 消息落库：重开会话能看到部署结果，也供后续对话的 LLM 引用
	_ = s.store.AppendMessage(store.Message{SessionID: ap.SessionID, Role: "assistant", Content: summary})
	s.emit(EventDeployDone, map[string]any{
		"sessionId":  ap.SessionID,
		"approvalId": ap.ID,
		"status":     status, // success | failed | canceled
		"summary":    summary,
	})
	slog.Info("部署执行结束", "approval", ap.ID, "flow", ap.FlowName, "status", status, "steps", fmt.Sprintf("%d/%d", doneCount, total), "elapsed", time.Since(start).String())
}

// finishDeploy 异常路径的兜底收尾（准备阶段失败 / panic）：
// 历史还停留在 started 状态时写入终态，并推 deploy:done 帧。
// hist 为 nil 表示尚未落历史（准备阶段失败），跳过历史更新。
func (s *Service) finishDeploy(ap *Approval, hist *store.DeployHistory, status, summary string) {
	if hist != nil && hist.Status == "" {
		hist.Status = status
		hist.Detail = summary
		_ = s.store.UpdateDeployHistory(*hist)
	}
	s.emit(EventDeployDone, map[string]any{
		"sessionId":  ap.SessionID,
		"approvalId": ap.ID,
		"status":     status, // failed | canceled | rejected
		"summary":    summary,
	})
}

// interruptOutcome 把 ctx 终态归类为部署状态：用户取消 = canceled，超时 = failed。
func interruptOutcome(err error, stepNo, total int) (status, info string) {
	if errors.Is(err, context.Canceled) {
		return store.DeployCanceled, fmt.Sprintf("第 %d/%d 步执行中被停止", stepNo, total)
	}
	return store.DeployFailed, fmt.Sprintf("第 %d/%d 步执行超时（超过 %s 总上限）", stepNo, total, deployExecTimeout)
}

// sanitizeStepOutput 步骤输出的安全三连（先脱敏再截断，顺序同日志分析技能）。
func sanitizeStepOutput(out string) string {
	return sanitize.CapChars(sanitize.MaskSecrets(strings.TrimSpace(out)), maxStepOutputChars)
}

// buildDeploySummary 生成回报会话的中文总结（也是历史排错的速览）。
func buildDeploySummary(ap *Approval, status string, doneCount, total int, failInfo string, elapsed time.Duration) string {
	target := "本机"
	if ap.Target == store.TargetSSH {
		target = "SSH 远程主机"
	}
	var b strings.Builder
	switch status {
	case store.DeploySuccess:
		fmt.Fprintf(&b, "部署流程「%s」执行完成：%d/%d 步全部成功（目标: %s，耗时 %s）。", ap.FlowName, doneCount, total, target, elapsed.Round(time.Second))
		for i, cmd := range ap.Commands {
			fmt.Fprintf(&b, "\n%d. %s", i+1, cmd)
		}
	case store.DeployCanceled:
		fmt.Fprintf(&b, "部署流程「%s」已停止：完成 %d/%d 步，其余步骤未执行（目标: %s）。", ap.FlowName, doneCount, total, target)
		if failInfo != "" {
			fmt.Fprintf(&b, "\n%s", failInfo)
		}
	default: // failed（含超时）
		fmt.Fprintf(&b, "部署流程「%s」执行失败（目标: %s）：已完成 %d/%d 步。\n失败原因: %s", ap.FlowName, target, doneCount, total, failInfo)
	}
	return b.String()
}

// emitDeployProgress 推送步骤进度帧。
func (s *Service) emitDeployProgress(ap *Approval, step, total int, status, cmd, output string) {
	s.emit(EventDeployProgress, map[string]any{
		"sessionId":  ap.SessionID,
		"approvalId": ap.ID,
		"step":       step,
		"total":      total,
		"status":     status, // start | done | failed
		"command":    cmd,
		"output":     output,
	})
}

// ==================== 执行通道 ====================

// deployStepRunner 按部署目标组装执行器。返回 (执行器, 通道描述, 错误)——
// 通道描述写进执行历史（如 "本机" / "ssh://root@10.0.0.5:22"）。
// SSH 通道复用设置页的连接配置（与"测试 SSH"同一套），并要求已配置远程主机。
func (s *Service) deployStepRunner(target string) (StepRunner, string, error) {
	switch target {
	case store.TargetLocal:
		return s.runLocalStep, "本机", nil
	case store.TargetSSH:
		host := s.dockerSSHHost()
		if host == "" {
			return nil, "", fmt.Errorf("部署目标为 SSH，但尚未配置远程主机：请先在设置中填写 Docker IP 与 SSH 凭据")
		}
		pass := s.sshPassword()
		return func(ctx context.Context, cmd string) (string, error) {
			return s.docker.RunSSHCommand(ctx, host, pass, cmd)
		}, host, nil
	default:
		return nil, "", fmt.Errorf("未知的部署目标: %q", target)
	}
}

// localShellInvocation 构造"经系统 shell 执行一条命令行"的进程参数。
// 纯函数（goos / scriptPath 参数化）：两平台构造逻辑都能在任何机器上测试。
//   - POSIX（Linux/macOS）：/bin/sh -c <命令行>；命令行经 argv 直传，
//     进程创建路径上无二次 shell 解析，注入防护全部落在参数替换的引号转义上。
//   - Windows：命令行写入临时批处理，经 cmd /d /c 执行。批处理首行 @echo off
//     关命令回显（批处理模式也没有交互式横幅），chcp 65001 切 UTF-8 码页
//     （批处理文件是 UTF-8 编码，中文命令不乱码）；批处理结束时 cmd 的退出码
//     即最后一条命令的退出码。不用 "cmd /c 命令行" 直传是为了避开 cmd 对
//     引号的特殊解析规则（/s 语义陷阱）。
//
// 返回 (进程名, 参数, 需要写入的批处理内容——POSIX 为空串)。
func localShellInvocation(goos, cmdLine, scriptPath string) (name string, args []string, script string) {
	if goos == "windows" {
		script = "@echo off\r\nchcp 65001 >nul\r\n" + cmdLine + "\r\n"
		return "cmd", []string{"/d", "/c", scriptPath}, script
	}
	return "/bin/sh", []string{"-c", cmdLine}, ""
}

// runLocalStep 本机通道：经系统 shell 执行一条命令，返回合并输出。
// ctx 取消/超时时进程被强制终止（注意：Windows 上批处理派生的子进程可能
// 残留，属 os/exec 已知限制）。
func (s *Service) runLocalStep(ctx context.Context, cmdLine string) (string, error) {
	name, args, script := localShellInvocation(runtime.GOOS, cmdLine, "")
	if script != "" {
		// Windows：临时批处理承载命令行（文件名随机，用后即删）
		f, err := os.CreateTemp("", "devcraft-deploy-*.bat")
		if err != nil {
			return "", fmt.Errorf("创建临时脚本失败: %w", err)
		}
		scriptPath := f.Name()
		defer os.Remove(scriptPath)
		if _, err := f.WriteString(script); err != nil {
			f.Close()
			return "", fmt.Errorf("写入临时脚本失败: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("写入临时脚本失败: %w", err)
		}
		name, args, _ = localShellInvocation(runtime.GOOS, cmdLine, scriptPath)
	}
	c := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	runErr := c.Run()
	out := strings.TrimSpace(buf.String())
	if ctx.Err() != nil {
		// 取消/超时：返回已产生的输出（便于写详情）+ ctx 错误
		return out, ctx.Err()
	}
	if runErr != nil {
		return out, fmt.Errorf("命令执行失败: %w", runErr)
	}
	return out, nil
}

// ==================== 流程 CRUD（含校验）=====================

// ListDeployFlows 流程列表（设置页管理区 + 技能动态描述共用）。
func (s *Service) ListDeployFlows() ([]store.DeployFlow, error) {
	return s.store.ListDeployFlows()
}

// SaveDeployFlow 保存流程（新建或更新），先做业务校验再落库。
func (s *Service) SaveDeployFlow(f store.DeployFlow) error {
	if err := validateDeployFlow(&f); err != nil {
		return err
	}
	slog.Info("保存部署流程", "name", f.Name, "target", f.Target, "params", len(f.Params), "steps", len(f.Steps))
	return s.store.SaveDeployFlow(&f)
}

// DeleteDeployFlow 删除流程（执行历史保留）。
func (s *Service) DeleteDeployFlow(id int64) error {
	return s.store.DeleteDeployFlow(id)
}

// reParamName 参数名合法字符集——必须与 shellx 占位符正则的字符集一致，
// 否则定义出来的占位符永远匹配不上。
var reParamName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// validateDeployFlow 表单校验：名称/目标/步骤/参数声明（含正则可编译性）。
// 空白参数行与空步骤行静默丢弃（动态表单常见残留）。
func validateDeployFlow(f *store.DeployFlow) error {
	f.Name = strings.TrimSpace(f.Name)
	if f.Name == "" {
		return fmt.Errorf("请填写部署流程名称")
	}
	if len([]rune(f.Name)) > 64 {
		return fmt.Errorf("流程名称过长（最多 64 个字符）")
	}
	if f.Target != store.TargetLocal && f.Target != store.TargetSSH {
		return fmt.Errorf("无效的部署目标: %q（可选值: local / ssh）", f.Target)
	}
	// 步骤：逐行去空白，丢空行
	steps := make([]string, 0, len(f.Steps))
	for _, st := range f.Steps {
		if st = strings.TrimSpace(st); st != "" {
			steps = append(steps, st)
		}
	}
	if len(steps) == 0 {
		return fmt.Errorf("请至少填写一条命令步骤（每行一条命令）")
	}
	if len(steps) > maxFlowSteps {
		return fmt.Errorf("命令步骤过多（最多 %d 条）", maxFlowSteps)
	}
	f.Steps = steps
	// 参数声明：名字合法且不重复，校验正则必须可编译（保存期 fail-fast，
	// 而不是等 LLM 触发部署时才暴露坏正则）
	seen := map[string]bool{}
	params := make([]store.FlowParam, 0, len(f.Params))
	for _, p := range f.Params {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			continue // 空行丢弃
		}
		if !reParamName.MatchString(p.Name) {
			return fmt.Errorf("参数名 %q 不合法：仅限字母/数字/下划线/连字符，且以字母或下划线开头", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("参数名重复: %s", p.Name)
		}
		seen[p.Name] = true
		if p.Pattern != "" {
			if _, err := regexp.Compile(`^(?:` + p.Pattern + `)$`); err != nil {
				return fmt.Errorf("参数 %s 的校验正则不合法: %v", p.Name, err)
			}
		}
		params = append(params, p)
	}
	f.Params = params
	return nil
}
