// Package deploy 实现部署域的通用技能 deploy_run_flow（一键部署）。
//
// 设计要点（与用户的决策对齐）：
//  1. 单一通用技能，不做动态注册——所有用户自定义流程共用这一个工具，
//     Description 动态拼接当前流程清单帮 LLM 选择；
//  2. 技能调用绝不直接执行：只做"查流程 → 校验参数 → 转义替换占位符 →
//     生成待审批单"，审批单由宿主（appsvc）托管并推审批卡片帧；
//  3. 参数来自 LLM（外部输入），替换时强制走 shell 引号转义（见 shellx），
//     声明了校验正则的参数不符即拒绝。
//
// Java 类比：本包 ≈ 一个 Controller——只做校验与组装"工单"，
// 真正的执行引擎（审批状态机 + 执行器）在 appsvc（Service 层）。
// 依赖方向上本包只定义两个小接口（FlowSource / ApprovalSubmitter），
// 由 app.go 注入 appsvc 的实现，避免 skill → appsvc 的循环依赖。
package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"DevCraft/internal/shellx" // 占位符替换与引号转义（防注入）
	"DevCraft/internal/skill"  // Skill 接口与注册表
	"DevCraft/internal/store"  // DeployFlow 等数据模型
)

// SkillName 技能名（带命名空间，下划线惯例）。
const SkillName = "deploy_run_flow"

// hostGOOS 宿主机 OS（runtime.GOOS 的可测替身：转义规则按执行通道选择，
// 测试需要跨平台断言 Windows 通道的校验行为）。
var hostGOOS = runtime.GOOS

// FlowSource 提供流程定义（appsvc 实现，透传 store）。
type FlowSource interface {
	ListDeployFlows() ([]store.DeployFlow, error)
}

// ApprovalSubmitter 待审批单的宿主（appsvc 实现：内存注册表 + 审批卡片帧推送）。
// 返回审批单 id；宿主负责把卡片推给对应会话的前端。
type ApprovalSubmitter interface {
	SubmitDeployApproval(sessionID string, flow store.DeployFlow, params map[string]string, commands []string) (string, error)
}

// Register 注册部署技能。
func Register(reg *skill.Registry, flows FlowSource, submit ApprovalSubmitter) error {
	return reg.Register(&runFlow{flows: flows, submit: submit})
}

// ctxKey 私有类型作 ctx 键（避免与其他包的键冲突）。
type ctxKey struct{}

// WithSessionID 把会话 id 注入 ctx——Skill 接口没有会话参数，
// 这是编排层（appsvc.SendMessage）向技能透传"当前会话"的通道。
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, sessionID)
}

// SessionIDFromCtx 取出会话 id（未注入时返回空串）。
func SessionIDFromCtx(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// runFlow deploy_run_flow 技能本体。
type runFlow struct {
	flows  FlowSource
	submit ApprovalSubmitter
}

func (s *runFlow) Name() string { return SkillName }

// Description 动态拼接当前所有流程清单（名称+描述+参数说明），帮 LLM 选流程。
// Description 在每次回合组装工具清单时调用（agent.Runner.toolSpecs），
// 因此流程的增删改下一回合即生效，无需重启。
func (s *runFlow) Description() string {
	base := "执行用户自定义的部署流程：校验参数后生成待审批的部署单（含替换参数后的完整命令预览），" +
		"用户在聊天中点击批准后才真正执行；未批准绝不执行。当用户要求部署、发布、上线、更新某个服务时使用。"
	flows, err := s.flows.ListDeployFlows()
	if err != nil {
		return base
	}
	if len(flows) == 0 {
		return base + "\n当前没有已定义的部署流程。若用户要求部署，请提示其先在设置页创建部署流程，不要编造流程名调用。"
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n可用流程：\n")
	for _, f := range flows {
		fmt.Fprintf(&b, "- %s", f.Name)
		if f.Description != "" {
			fmt.Fprintf(&b, "（%s）", f.Description)
		}
		target := "本机"
		if f.Target == store.TargetSSH {
			target = "SSH 远程主机"
		}
		fmt.Fprintf(&b, "，目标: %s", target)
		if len(f.Params) > 0 {
			parts := make([]string, 0, len(f.Params))
			for _, p := range f.Params {
				part := p.Name
				if p.Desc != "" {
					part += "=" + p.Desc
				}
				parts = append(parts, part)
			}
			fmt.Fprintf(&b, "，参数: %s", strings.Join(parts, ", "))
		} else {
			b.WriteString("，无需参数")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Parameters 参数 Schema：flow（流程名，必填）+ params（参数键值对）。
func (s *runFlow) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"flow":{"type":"string","description":"部署流程名称（必须是可用流程清单中存在的名称）"},
			"params":{"type":"object","description":"流程声明的参数键值对，如 {\"version\":\"1.2.3\"}；无参数流程可省略"}
		},
		"required":["flow"],
		"additionalProperties":false
	}`)
}

// Execute 查流程 → 校验参数（必填 + 可选正则）→ 转义替换占位符生成最终命令 →
// 创建待审批单 → 返回"等待批准"的说明文本（LLM 会转述给用户）。
// 本方法绝不执行任何命令——执行只发生在用户批准之后（appsvc 侧）。
func (s *runFlow) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Flow   string         `json:"flow"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数错误: 调用参数不是合法 JSON")
	}
	in.Flow = strings.TrimSpace(in.Flow)
	if in.Flow == "" {
		return "", fmt.Errorf("参数错误: 需要提供 flow（部署流程名称）")
	}
	sessionID := SessionIDFromCtx(ctx)
	if sessionID == "" {
		return "", fmt.Errorf("内部错误: 无法识别当前会话，不能创建部署审批单")
	}

	// --- 1. 按名查流程（未找到时列出可用名字，帮 LLM 自纠） ---
	flows, err := s.flows.ListDeployFlows()
	if err != nil {
		return "", fmt.Errorf("读取部署流程失败: %w", err)
	}
	var flow *store.DeployFlow
	names := make([]string, 0, len(flows))
	for i := range flows {
		names = append(names, flows[i].Name)
		if flows[i].Name == in.Flow {
			flow = &flows[i]
		}
	}
	if flow == nil {
		if len(names) == 0 {
			return "", fmt.Errorf("没有找到部署流程 %q：当前没有定义任何流程，请引导用户在设置页创建", in.Flow)
		}
		sort.Strings(names)
		return "", fmt.Errorf("没有找到部署流程 %q，可用流程: %s", in.Flow, strings.Join(names, ", "))
	}

	// --- 2. 参数归一化：LLM 可能传数字/布尔，统一转字符串 ---
	values := make(map[string]string, len(in.Params))
	for k, v := range in.Params {
		switch tv := v.(type) {
		case string:
			values[k] = strings.TrimSpace(tv)
		default:
			values[k] = fmt.Sprintf("%v", tv)
		}
	}

	// --- 3. 校验：声明的参数必须全部提供且非空；未声明的参数拒绝（防误用） ---
	// Windows 本机通道走 cmd：cmd 解析层没有通用转义（引用态只认裸 "、按行解析），
	// 能破裂引用区的值（双引号/换行/控制字符）必须在替换前整体拒绝（防注入硬门禁）。
	windowsMode := flow.Target == store.TargetLocal && hostGOOS == "windows"
	declared := make(map[string]store.FlowParam, len(flow.Params))
	for _, p := range flow.Params {
		declared[p.Name] = p
	}
	for k := range values {
		if _, ok := declared[k]; !ok {
			return "", fmt.Errorf("流程 %q 没有声明参数 %q，不能接受该参数", flow.Name, k)
		}
	}
	for _, p := range flow.Params {
		v, ok := values[p.Name]
		if !ok || v == "" {
			return "", s.missingParamErr(flow, p)
		}
		// 可选校验正则：按"整体匹配"语义包裹（用户写 \d+ 即要求整个值都是数字）
		if p.Pattern != "" {
			re, err := regexp.Compile(`^(?:` + p.Pattern + `)$`)
			if err != nil {
				return "", fmt.Errorf("流程 %q 的参数 %s 校验正则不合法: %v", flow.Name, p.Name, err)
			}
			if !re.MatchString(v) {
				return "", fmt.Errorf("参数 %s 的值 %q 不符合要求（%s），已拒绝生成部署单", p.Name, v, paramHint(p))
			}
		}
		if windowsMode {
			// cmd 无法引用的值（双引号/换行/控制字符）整体拒绝——绝不带病进入替换
			if err := shellx.ValidateWindowsValue(v); err != nil {
				return "", fmt.Errorf("参数 %s 的值无法在 Windows 本机通道安全执行：%v，已拒绝生成部署单", p.Name, err)
			}
		}
	}

	// --- 4. 安全替换占位符：按执行通道选转义规则（替换即转义，防注入核心） ---
	quote := shellx.QuotePOSIX // SSH 远端会话与 POSIX 本机 shell 都用单引号规则
	if windowsMode {
		quote = shellx.QuoteWindows // 本机 Windows 走 cmd 双引号规则（值已过 ValidateWindowsValue 门禁）
	}
	commands := make([]string, 0, len(flow.Steps))
	for i, step := range flow.Steps {
		cmd, err := shellx.Render(step, values, quote)
		if err != nil {
			return "", fmt.Errorf("流程 %q 第 %d 步命令生成失败: %w", flow.Name, i+1, err)
		}
		commands = append(commands, cmd)
	}

	// --- 5. 创建待审批单（宿主负责注册与推送审批卡片帧） ---
	approvalID, err := s.submit.SubmitDeployApproval(sessionID, *flow, values, commands)
	if err != nil {
		return "", fmt.Errorf("创建部署审批单失败: %w", err)
	}

	target := "本机"
	if flow.Target == store.TargetSSH {
		target = "SSH 远程主机"
	}
	return fmt.Sprintf(
		"已生成部署审批单（编号 %s）：流程「%s」，目标：%s，共 %d 条命令，审批卡片已推送给用户。"+
			"在用户点击批准之前绝不会执行任何命令。请转告用户：确认卡片中的命令清单后点击「批准」开始执行，"+
			"点击「拒绝」或不操作则不会执行。不要声称部署已经完成。",
		approvalID, flow.Name, target, len(commands)), nil
}

// missingParamErr 组装缺参错误（附带参数说明，帮 LLM 补齐后重试）。
func (s *runFlow) missingParamErr(flow *store.DeployFlow, p store.FlowParam) error {
	return fmt.Errorf("流程 %q 缺少必填参数 %s（%s），请向用户询问后重新调用", flow.Name, p.Name, paramHint(p))
}

// paramHint 参数的可读说明（描述 + 正则要求）。
func paramHint(p store.FlowParam) string {
	hint := p.Desc
	if hint == "" {
		hint = "部署参数"
	}
	if p.Pattern != "" {
		hint += "，要求格式: " + p.Pattern
	}
	return hint
}
