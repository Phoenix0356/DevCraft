// Package dockerx 封装官方 Docker SDK（github.com/docker/docker/client）：
// ① 连接管理（本机 + 远程 host，按 host 缓存客户端）
// ② 运维技能需要的三个只读操作：列容器 / 资源占用 / 日志。
// 技能层不直接碰 SDK，全部经由本包，SDK 升级只影响这里。
package dockerx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/docker/docker/api/types/container" // 容器相关的请求选项类型
	"github.com/docker/docker/client"              // Docker API 客户端
	"github.com/docker/docker/pkg/stdcopy"         // 解码 Docker 日志流的复用协议（见 Logs 注释）
)

// Manager 按 host 字符串缓存 Docker 客户端（≈ 连接池/对象缓存）。
// 避免每次技能执行都重新建客户端。
type Manager struct {
	mu      sync.Mutex                // 互斥锁，保护 map 的并发读写
	clients map[string]*client.Client // SDK 直连客户端缓存（本机/tcp://）
	sshExes map[string]*sshExecutor   // SSH 执行器缓存（ssh://；key 含密码，改密码自动重建连接）
}

// NewManager 创建空的连接管理器。
func NewManager() *Manager {
	return &Manager{clients: map[string]*client.Client{}, sshExes: map[string]*sshExecutor{}}
}

// Client 返回指定 host 的 Docker API 客户端（缓存命中则直接复用）。
//   - host 为空 = 本机 daemon：Windows 走 npipe（命名管道），macOS/Linux 走 unix socket，
//     具体由 client.FromEnv 读取环境变量 DOCKER_HOST 等决定
//   - 远程 host 用 DOCKER_HOST 语法：tcp://1.2.3.4:2375 或 ssh://user@host
func (m *Manager) Client(host string) (*client.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[host]; ok { // 缓存命中
		return c, nil
	}
	// 函数式选项模式（Functional Options）：用一串 Opt 函数组装配置，
	// 是 Go 生态替代"多参数构造器/Builder"的惯用法。
	opts := []client.Opt{
		client.FromEnv,                     // 先读环境变量（DOCKER_HOST/TLS 证书等）作为基础配置
		client.WithAPIVersionNegotiation(), // 自动协商 API 版本，兼容新旧 daemon
	}
	if host != "" {
		opts = append(opts, client.WithHost(host)) // 显式覆盖连接地址（远程 host 场景）
	}
	c, err := client.NewClientWithOpts(opts...) // opts... 展开切片逐个传入
	if err != nil {
		return nil, fmt.Errorf("docker client for %q: %w", host, err)
	}
	m.clients[host] = c
	return c, nil
}

// ContainerInfo 是我们自己定义的容器信息视图（只保留展示需要的字段）。
type ContainerInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	State   string   `json:"state"`  // running / exited ...
	Status  string   `json:"status"` // 人类可读状态，如 "Up 2 hours"
	Ports   []string `json:"ports"`
	Created int64    `json:"created"`
}

// ListContainers 列出所有容器（All:true 含已停止的），并转成我们的视图。
func ListContainers(ctx context.Context, cli *client.Client) ([]ContainerInfo, error) {
	items, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(items))
	for _, c := range items {
		info := ContainerInfo{
			ID:      c.ID[:min(12, len(c.ID))], // 容器 ID 只保留前 12 位（docker ps 惯例）
			Image:   c.Image,
			State:   c.State,
			Status:  c.Status,
			Created: c.Created,
		}
		if len(c.Names) > 0 {
			info.Name = c.Names[0]
			// Docker API 返回的容器名带 "/" 前缀（如 "/web-app"），去掉
			if len(info.Name) > 0 && info.Name[0] == '/' {
				info.Name = info.Name[1:] // 切片截取语法 s[1:] ≈ substring(1)
			}
		}
		// 端口信息格式化：有宿主映射显示 "ip:宿主端口->容器端口/协议"，否则只显示容器端口
		for _, p := range c.Ports {
			if p.PublicPort > 0 {
				info.Ports = append(info.Ports, fmt.Sprintf("%s:%d->%d/%s", p.IP, p.PublicPort, p.PrivatePort, p.Type))
			} else {
				info.Ports = append(info.Ports, fmt.Sprintf("%d/%s", p.PrivatePort, p.Type))
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// statsJSON 是我们自己声明的 stats 响应结构体，只包含用到的稳定字段。
// 为什么不直接复用 SDK 的类型？Docker SDK 经常搬动类型位置（版本间 breaking change），
// 自己按 JSON 字段解析就不受 SDK 类型 churn 影响（防腐层思想）。
// 嵌套匿名字段写法：struct 里直接内嵌 struct 定义。
type statsJSON struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"` // 容器累计 CPU 纳秒数
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"` // 宿主机累计 CPU 纳秒数
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct { // 上一个采样点的数据，算增量用
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"` // 已用内存（字节）
		Limit uint64 `json:"limit"` // 内存上限（字节）
	} `json:"memory_stats"`
	Networks map[string]struct { // key = 网卡名（eth0 等）
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

// StatsInfo 计算后的资源占用结果（给技能层用）。
type StatsInfo struct {
	CPUPercent    float64 `json:"cpuPercent"`
	MemoryUsageMB float64 `json:"memoryUsageMB"`
	MemoryLimitMB float64 `json:"memoryLimitMB"`
	MemPercent    float64 `json:"memPercent"`
	NetRxMB       float64 `json:"netRxMB"` // 容器启动以来累计接收
	NetTxMB       float64 `json:"netTxMB"` // 累计发送
}

// Stats 取一次容器资源快照并计算 CPU 百分比。
func Stats(ctx context.Context, cli *client.Client, nameOrID string) (*StatsInfo, error) {
	// OneShot = 采样一次就结束（对应 docker stats --no-stream）
	resp, err := cli.ContainerStatsOneShot(ctx, nameOrID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() // resp.Body 是 io.ReadCloser（≈ InputStream），务必关闭
	var s statsJSON
	// json.NewDecoder(resp.Body).Decode：流式反序列化，直接把响应体解析进结构体
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}
	info := &StatsInfo{
		MemoryUsageMB: float64(s.MemoryStats.Usage) / 1024 / 1024, // 字节 → MB
		MemoryLimitMB: float64(s.MemoryStats.Limit) / 1024 / 1024,
	}
	// CPU 百分比官方算法（与 docker CLI 相同）：
	// (容器 CPU 增量 / 宿主 CPU 增量) × CPU 核数 × 100
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage - s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemCPUUsage - s.PreCPUStats.SystemCPUUsage)
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = 1 // 某些运行时不上报核数，兜底为 1
	}
	if sysDelta > 0 && cpuDelta >= 0 { // 防御除零和时钟回拨
		info.CPUPercent = cpuDelta / sysDelta * cpus * 100
	}
	if s.MemoryStats.Limit > 0 {
		info.MemPercent = float64(s.MemoryStats.Usage) / float64(s.MemoryStats.Limit) * 100
	}
	// 多网卡流量累加
	for _, n := range s.Networks {
		info.NetRxMB += float64(n.RxBytes) / 1024 / 1024
		info.NetTxMB += float64(n.TxBytes) / 1024 / 1024
	}
	return info, nil
}

// Logs 返回容器最近 tail 行的 stdout+stderr 合并文本。
//
// 技术要点：非 TTY 容器的日志流不是纯文本，而是带 8 字节头的"复用流"
// （每块前面标一个字节表明来自 stdout 还是 stderr），必须用 stdcopy.StdCopy
// 解码拆分，否则日志开头全是乱码二进制。
func Logs(ctx context.Context, cli *client.Client, nameOrID string, tail int) (string, error) {
	if tail <= 0 {
		tail = 500
	}
	rc, err := cli.ContainerLogs(ctx, nameOrID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail), // SDK 要字符串，int 转 string
	})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var stdout, stderr bytes.Buffer // bytes.Buffer ≈ ByteArrayOutputStream
	if _, err := stdcopy.StdCopy(&stdout, &stderr, rc); err != nil && err != io.EOF {
		return "", fmt.Errorf("read logs: %w", err) // EOF 是正常结束，其余错误才上报
	}
	out := stdout.String()
	if stderr.Len() > 0 { // 有 stderr 内容就追加在后面
		if out != "" {
			out += "\n"
		}
		out += stderr.String()
	}
	return out, nil
}

// Ping 探测 Docker daemon 是否可达（设置页"测试连接"用它）。
func Ping(ctx context.Context, cli *client.Client) error {
	_, err := cli.Ping(ctx)
	return err
}
