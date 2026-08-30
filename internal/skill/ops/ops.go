// Package ops 实现运维域的 3 个内置技能（MVP P0）：
// ops.list_containers / ops.container_stats / ops.analyze_logs。
//
// 每个技能就是一个实现了 skill.Skill 接口的 struct（≈ 策略模式实现类）。
// 新增技能三步走：① 写 struct 实现 4 个方法 ② 在 Register() 里挂上
// ③ 给某个 Agent 的装配列表加上技能名。
package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp" // 标准库正则（RE2 语法，不支持前瞻后顾）
	"strings"

	"DevCraft/internal/dockerx"  // Docker 操作封装
	"DevCraft/internal/sanitize" // 日志截断与脱敏
	"DevCraft/internal/skill"    // Skill 接口与注册表
)

// 日志分析的防护参数：控制送进 LLM 上下文的日志体量 = 控制 token 成本与响应速度。
const (
	DefaultLogLines = 500   // 默认拉最近 500 行
	MaxLogLines     = 2000  // 用户指定行数上限
	MaxLogChars     = 20000 // 字符总量上限（兜底）
)

// EndpointFn 是一个"函数类型"（≈ Java 的 Supplier 函数式接口）。
// 技能执行时调用它拿"当前"配置的 Docker Host 与 SSH 密码——用回调而不是存快照，
// 保证用户在设置页改了配置之后立即生效。
type EndpointFn func() (host string, sshPassword string)

// Register 把 3 个运维技能批量挂载到注册表。
// docker 与 endpoint 依赖通过参数注入（构造注入），技能本身不关心来源。
func Register(reg *skill.Registry, docker *dockerx.Manager, endpoint EndpointFn) error {
	for _, s := range []skill.Skill{
		&listContainers{docker: docker, endpoint: endpoint}, // &struct{...} = 创建并取指针（≈ new）
		&containerStats{docker: docker, endpoint: endpoint},
		&analyzeLogs{docker: docker, endpoint: endpoint},
	} {
		if err := reg.Register(s); err != nil {
			return err
		}
	}
	return nil
}

// ==================== ops.list_containers ====================

// listContainers 技能一：容器状态查看。
type listContainers struct {
	docker   *dockerx.Manager
	endpoint EndpointFn
}

func (s *listContainers) Name() string { return "ops_list_containers" }

// Description 直接决定 LLM 的路由判断，写清"什么场景用我"。
func (s *listContainers) Description() string {
	return "列出 Docker 中所有容器（含已停止的）的名称、状态、镜像和端口映射。" +
		"当用户想查看容器列表、容器状态、部署了哪些服务时使用。"
}

// Parameters 返回 JSON Schema：本技能无参数。
func (s *listContainers) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// Execute 查容器列表并格式化为易读文本（LLM 再基于它组织回答）。
// 参数 _ 表示"声明但不用"（本技能无参数）——Go 用下划线占位表达忽略。
func (s *listContainers) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	host, pass := s.endpoint()
	exe, err := s.docker.Executor(host, pass) // 按当前配置取执行器（SDK 直连或 SSH 命令模式）
	if err != nil {
		return "", err
	}
	infos, err := exe.ListContainers(ctx)
	if err != nil {
		return "", fmt.Errorf("获取容器列表失败: %w", err)
	}
	if len(infos) == 0 {
		return "当前没有任何容器。", nil
	}
	var b strings.Builder // ≈ StringBuilder
	fmt.Fprintf(&b, "共 %d 个容器:\n", len(infos))
	for _, c := range infos {
		ports := "-"
		if len(c.Ports) > 0 {
			ports = strings.Join(c.Ports, ", ") // 多端口逗号拼接
		}
		fmt.Fprintf(&b, "- %s | 状态: %s (%s) | 镜像: %s | 端口: %s\n", c.Name, c.State, c.Status, c.Image, ports)
	}
	return b.String(), nil
}

// ==================== ops.container_stats ====================

// containerStats 技能二：容器性能监控（CPU/内存/网络）。
type containerStats struct {
	docker   *dockerx.Manager
	endpoint EndpointFn
}

func (s *containerStats) Name() string { return "ops_container_stats" }

func (s *containerStats) Description() string {
	return "查看指定容器的实时 CPU、内存、网络 I/O 占用。" +
		"当用户问某个容器的 CPU/内存/性能/资源占用时使用，参数为容器名或容器 ID。"
}

// Parameters 声明一个必填字符串参数 container。
// LLM 会严格按此 Schema 生成 {"container": "xxx"}。
func (s *containerStats) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"container":{"type":"string","description":"容器名称或容器 ID"}},
		"required":["container"],
		"additionalProperties":false
	}`)
}

func (s *containerStats) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// 匿名 struct 临时定义"参数 DTO"，json.Unmarshal 把参数 JSON 反序列化进来
	// （≈ new ObjectMapper().readValue(args, Params.class)）
	var in struct {
		Container string `json:"container"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Container == "" {
		return "", fmt.Errorf("参数错误: 需要提供 container（容器名或 ID）")
	}
	host, pass := s.endpoint()
	exe, err := s.docker.Executor(host, pass)
	if err != nil {
		return "", err
	}
	st, err := exe.Stats(ctx, in.Container)
	if err != nil {
		return "", fmt.Errorf("获取容器 %s 资源占用失败: %w", in.Container, err)
	}
	// %.2f%% = 两位小数 + 字面百分号（%% 转义）
	return fmt.Sprintf(
		"容器 %s 实时资源占用:\n- CPU: %.2f%%\n- 内存: %.1f MB / %.1f MB (%.2f%%)\n- 网络: RX %.2f MB / TX %.2f MB（累计）",
		in.Container, st.CPUPercent, st.MemoryUsageMB, st.MemoryLimitMB, st.MemPercent, st.NetRxMB, st.NetTxMB,
	), nil
}

// ==================== ops.analyze_logs ====================

// analyzeLogs 技能三：智能日志分析。
// 注意：技能本身只做"取日志 + 提取疑似错误行"，真正的根因分析由 LLM
// 基于回填的日志完成——技能是数据提供者，LLM 是分析者。
type analyzeLogs struct {
	docker   *dockerx.Manager
	endpoint EndpointFn
}

func (s *analyzeLogs) Name() string { return "ops_analyze_logs" }

func (s *analyzeLogs) Description() string {
	return "拉取指定容器最近的日志并提取错误堆栈。" +
		"当用户想排查容器报错、异常、查日志、分析为什么报错时使用，拿到日志后请给出根因分析与建议。"
}

func (s *analyzeLogs) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"container":{"type":"string","description":"容器名称或容器 ID"},
			"lines":{"type":"integer","description":"拉取最近多少行日志，默认 500，最大 2000"}
		},
		"required":["container"],
		"additionalProperties":false
	}`)
}

func (s *analyzeLogs) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Container string `json:"container"`
		Lines     int    `json:"lines"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Container == "" {
		return "", fmt.Errorf("参数错误: 需要提供 container（容器名或 ID）")
	}
	// 行数归一化：缺省 500，封顶 2000（防御模型乱传大数字）
	if in.Lines <= 0 {
		in.Lines = DefaultLogLines
	}
	if in.Lines > MaxLogLines {
		in.Lines = MaxLogLines
	}
	host, pass := s.endpoint()
	exe, err := s.docker.Executor(host, pass)
	if err != nil {
		return "", err
	}
	raw, err := exe.Logs(ctx, in.Container, in.Lines)
	if err != nil {
		return "", fmt.Errorf("获取容器 %s 日志失败: %w", in.Container, err)
	}
	// 安全三连（顺序有讲究）：
	// ① TailLines 兜底截行 → ② MaskSecrets 脱敏（密钥绝不进 LLM 上下文）
	// → ③ CapChars 字符总量封顶
	raw = sanitize.MaskSecrets(sanitize.TailLines(raw, in.Lines))
	raw = sanitize.CapChars(raw, MaxLogChars)
	if strings.TrimSpace(raw) == "" {
		return fmt.Sprintf("容器 %s 最近 %d 行日志为空。", in.Container, in.Lines), nil
	}
	// 预提取疑似错误行，帮 LLM 聚焦重点（也省它自己扫全文的 token）
	errLines := extractErrors(raw)
	var b strings.Builder
	fmt.Fprintf(&b, "容器 %s 最近日志（已脱敏）:\n```\n%s\n```\n", in.Container, raw)
	if len(errLines) > 0 {
		b.WriteString("\n提取到的疑似错误行:\n```\n")
		b.WriteString(strings.Join(errLines, "\n"))
		b.WriteString("\n```\n")
	} else {
		b.WriteString("\n未匹配到明显错误关键字。\n")
	}
	return b.String(), nil
}

// reError 匹配常见错误特征：关键字（error/fatal/panic/exception/traceback/caused by）
// 以及结构化日志的 level 字段。(?i) = 忽略大小写，\b = 单词边界。
// 包级 var + regexp.MustCompile：正则只编译一次，全局复用（编译失败会 panic，
// 因为正则写死在代码里，编译期就该暴露错误）。
var reError = regexp.MustCompile(`(?i)\b(error|fatal|panic|exception|traceback|caused by|stacktrace)\b|level=error|"level":"error"`)

// extractErrors 扫描日志，收集最多 30 行"疑似错误行"。
func extractErrors(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if reError.MatchString(line) {
			out = append(out, line)
			if len(out) >= 30 { // 上限防止刷屏
				break
			}
		}
	}
	return out
}
