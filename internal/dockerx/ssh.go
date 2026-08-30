// ssh.go 实现 ssh:// 路线的 Executor：通过 SSH 登录远端执行 docker CLI 命令。
//
// 为什么需要它：Go Docker SDK 不支持 ssh://（只支持直连 daemon），而让远端
// daemon 开 tcp 端口有安全风险。SSH 命令执行模式零远端配置、复用 SSH 认证，
// 用 docker CLI 的 --format json 拿结构化输出。
//
// 依赖：远端装有 docker CLI。
package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"DevCraft/internal/shellx" // shell 引号转义（与部署流程参数替换共用一套逻辑）
)

const sshDialTimeout = 15 * time.Second

// sshExecutor SSH 命令执行器：持有一条长连接，每次操作开一个 Session。
type sshExecutor struct {
	conn *ssh.Client
}

// sshTarget 解析后的 SSH 连接目标。
type sshTarget struct {
	user string
	host string
	port string
}

// parseSSHURL 解析 ssh://user@host[:port]。用户名必填（SSH 登录必需）。
func parseSSHURL(raw string) (*sshTarget, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("无法解析 SSH 地址 %q: %w", raw, err)
	}
	if u.Scheme != "ssh" {
		return nil, fmt.Errorf("不是 ssh:// 地址: %q", raw)
	}
	user := u.User.Username()
	if user == "" {
		return nil, fmt.Errorf("SSH 地址缺少用户名，应为 ssh://user@host[:port]")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("SSH 地址缺少主机名: %q", raw)
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	return &sshTarget{user: user, host: host, port: port}, nil
}

// buildAuthMethods 组装认证方式：密码优先，随后是 ~/.ssh 下的免密私钥。
// 带 passphrase 的私钥解析会失败，直接跳过（MVP 不支持 passphrase）。
func buildAuthMethods(password string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if password != "" {
		methods = append(methods, ssh.Password(password))
	}
	if home, err := os.UserHomeDir(); err == nil {
		// 按常见程度依次探测私钥文件
		for _, name := range []string{"id_ed25199", "id_ecdsa", "id_rsa", "id_dsa"} {
			data, err := os.ReadFile(filepath.Join(home, ".ssh", name))
			if err != nil {
				continue
			}
			signer, err := ssh.ParsePrivateKey(data) // 需要 passphrase 时这里报错 → 跳过
			if err != nil {
				continue
			}
			methods = append(methods, ssh.PublicKeys(signer))
		}
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("无可用 SSH 认证方式：请在设置中填写 SSH 密码，或配置免密私钥")
	}
	return methods, nil
}

// sshExecutorFor 取（或新建）指定 host 的 SSH 执行器（Executor 接口视角）。
func (m *Manager) sshExecutorFor(host, password string) (Executor, error) {
	return m.sshConnFor(host, password)
}

// sshConnFor 取（或新建）指定 host 的 SSH 执行器（连接池条目本身）。
// 缓存 key 包含密码：密码一改就自动建新连接，旧连接留在缓存里等 GC（条目极少，可接受）。
func (m *Manager) sshConnFor(host, password string) (*sshExecutor, error) {
	key := host + "\x00" + password
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.sshExes[key]; ok {
		return e, nil
	}
	tgt, err := parseSSHURL(host)
	if err != nil {
		return nil, err
	}
	methods, err := buildAuthMethods(password)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            tgt.user,
		Auth:            methods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // MVP：不校验 host key（无 known_hosts 管理 UI）
		Timeout:         sshDialTimeout,
	}
	conn, err := ssh.Dial("tcp", net.JoinHostPort(tgt.host, tgt.port), cfg)
	if err != nil {
		slog.Error("SSH 拨号失败", "host", host, "user", tgt.user, "err", err, "stack", string(debug.Stack()))
		return nil, fmt.Errorf("SSH 连接失败（%s）: %w", host, err)
	}
	e := &sshExecutor{conn: conn}
	m.sshExes[key] = e
	return e, nil
}

// RunSSHCommand 在远端执行任意一条命令并返回合并输出（通用命令执行能力）。
// 与 docker 命令同一套连接管理、同一套 ctx 取消语义（超时/取消向远端会话发
// SIGKILL）。部署流程的 SSH 通道用它执行替换好参数的完整命令行。
// 命令失败时同时返回已产生的输出（第一返回值）和错误，便于上层把输出写进详情。
func (m *Manager) RunSSHCommand(ctx context.Context, host, password, cmd string) (string, error) {
	e, err := m.sshConnFor(host, password)
	if err != nil {
		return "", err
	}
	return e.run(ctx, cmd)
}

// ==================== 命令执行 ====================

// run 在远端执行一条命令并返回合并输出（stdout+stderr）。
// 支持 ctx 取消：超时/取消时向远端会话发 SIGKILL。
func (e *sshExecutor) run(ctx context.Context, cmd string) (string, error) {
	sess, err := e.conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("SSH 会话创建失败（连接可能已断开，请重试）: %w", err)
	}
	defer sess.Close()

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := sess.CombinedOutput(cmd)
		ch <- result{out, err}
	}()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		return "", ctx.Err()
	case r := <-ch:
		out := strings.TrimSpace(string(r.out))
		if r.err != nil {
			// 把远端输出带上：docker 的错误信息（如 "No such container"）在里面
			return out, fmt.Errorf("远端命令失败: %w | 输出: %s", r.err, capChars(out, 300))
		}
		return out, nil
	}
}

func capChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ==================== 参数安全 ====================

// reContainerName 合法的容器名/ID 字符集（docker 命名规则 + 十六进制 ID）。
// 容器名来自 LLM，是外部输入：先校验再引用，双保险防 shell 注入。
var reContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

// quoteArg 校验容器名/ID 并做 shell 单引号引用。
// 校验失败直接拒绝（不试图"修复"危险输入）。
// 引号转义复用 shellx.QuotePOSIX（部署流程的参数替换同用一套转义逻辑）。
func quoteArg(nameOrID string) (string, error) {
	if !reContainerName.MatchString(nameOrID) {
		return "", fmt.Errorf("非法的容器名/ID: %q", nameOrID)
	}
	// 单引号包裹，内部单引号按 '\'' 转义（POSIX shell 标准做法）
	return shellx.QuotePOSIX(nameOrID), nil
}

// ==================== Executor 接口实现 ====================

// psLine 是 docker ps --format json 的单行输出（JSON Lines 格式，每行一个对象）。
type psLine struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
}

func (e *sshExecutor) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	out, err := e.run(ctx, "docker ps -a --format json")
	if err != nil {
		return nil, fmt.Errorf("获取容器列表失败: %w", err)
	}
	var infos []ContainerInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p psLine
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			continue // 个别坏行跳过，不影响整体
		}
		info := ContainerInfo{
			ID:     p.ID,
			Name:   strings.TrimPrefix(p.Names, "/"), // 部分版本名字带 / 前缀
			Image:  p.Image,
			State:  p.State,
			Status: p.Status,
		}
		if p.Ports != "" {
			info.Ports = strings.Split(p.Ports, ", ")
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// statsLine 是 docker stats --no-stream --format json 的单行输出。
// 数值都是人类可读字符串（"0.50%"、"1.2MiB / 15GiB"），需要解析。
type statsLine struct {
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	MemPerc  string `json:"MemPerc"`
	NetIO    string `json:"NetIO"`
}

func (e *sshExecutor) Stats(ctx context.Context, nameOrID string) (*StatsInfo, error) {
	q, err := quoteArg(nameOrID)
	if err != nil {
		return nil, err
	}
	out, err := e.run(ctx, "docker stats --no-stream --format json "+q)
	if err != nil {
		return nil, fmt.Errorf("获取容器 %s 资源占用失败: %w", nameOrID, err)
	}
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]) // 只取第一行
	var s statsLine
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		return nil, fmt.Errorf("解析 stats 输出失败: %w", err)
	}
	info := &StatsInfo{}
	info.CPUPercent, _ = parsePercent(s.CPUPerc)
	info.MemPercent, _ = parsePercent(s.MemPerc)
	if parts := strings.Split(s.MemUsage, " / "); len(parts) == 2 {
		usage, _ := parseSizeBytes(parts[0])
		limit, _ := parseSizeBytes(parts[1])
		info.MemoryUsageMB = float64(usage) / 1024 / 1024
		info.MemoryLimitMB = float64(limit) / 1024 / 1024
	}
	if parts := strings.Split(s.NetIO, " / "); len(parts) == 2 {
		rx, _ := parseSizeBytes(parts[0])
		tx, _ := parseSizeBytes(parts[1])
		info.NetRxMB = float64(rx) / 1024 / 1024
		info.NetTxMB = float64(tx) / 1024 / 1024
	}
	return info, nil
}

func (e *sshExecutor) Logs(ctx context.Context, nameOrID string, tail int) (string, error) {
	q, err := quoteArg(nameOrID)
	if err != nil {
		return "", err
	}
	if tail <= 0 {
		tail = 500
	}
	// 2>&1 合并 stderr（容器错误日志多在 stderr）；tail 是程序生成的整数，无注入风险
	out, err := e.run(ctx, fmt.Sprintf("docker logs --tail %d %s 2>&1", tail, q))
	if err != nil {
		return "", fmt.Errorf("获取容器 %s 日志失败: %w", nameOrID, err)
	}
	return out, nil
}

func (e *sshExecutor) Ping(ctx context.Context) error {
	// 能取到 daemon 版本即连通；远端无 docker/daemon 未启动都会在此报错
	_, err := e.run(ctx, "docker version --format '{{.Server.Version}}'")
	if err != nil {
		return fmt.Errorf("远端 Docker 不可用: %w", err)
	}
	return nil
}

// ==================== 输出解析辅助 ====================

// parsePercent 解析 "0.50%" → 0.5。
func parsePercent(s string) (float64, error) {
	s = strings.TrimSpace(strings.TrimSuffix(s, "%"))
	return strconv.ParseFloat(s, 64)
}

// sizeUnits 支持的容量单位：SI（1000 进制）与二进制（1024 进制）都覆盖，
// 因为不同 docker 版本的人类可读格式不一致。
var sizeUnits = map[string]float64{
	"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
	"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
}

// parseSizeBytes 解析 "1.205MiB" / "936B" / "1.1kB" → 字节数。
func parseSizeBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	// 从尾部切出最长匹配的单位后缀
	upper := strings.ToUpper(s)
	for _, suffix := range []string{"TIB", "GIB", "MIB", "KIB", "TB", "GB", "MB", "KB", "B"} {
		if strings.HasSuffix(upper, suffix) {
			num := strings.TrimSpace(s[:len(s)-len(suffix)])
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, err
			}
			return int64(v * sizeUnits[suffix]), nil
		}
	}
	return 0, fmt.Errorf("无法解析容量: %q", s)
}
