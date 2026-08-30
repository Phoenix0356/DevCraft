// executor.go 定义 Docker 操作的统一执行层。
//
// 背景：Go Docker SDK 只支持直连 daemon（本机 unix/npipe、tcp://），不支持 ssh://。
// 因此引入 Executor 接口，两条实现路线：
//   - sdkExecutor：SDK 直连 daemon（本机、tcp://）
//   - sshExecutor：SSH 登录远端执行 docker CLI 命令（ssh://，见 ssh.go）
//
// 技能层只依赖 Executor，不关心底层是哪条路线（策略模式）。
package dockerx

import (
	"context"
	"strings"

	"github.com/docker/docker/client"
)

// Executor 是运维技能依赖的四类只读操作抽象。
type Executor interface {
	ListContainers(ctx context.Context) ([]ContainerInfo, error)
	Stats(ctx context.Context, nameOrID string) (*StatsInfo, error)
	Logs(ctx context.Context, nameOrID string, tail int) (string, error)
	Ping(ctx context.Context) error
}

// Executor 按 host 协议选择执行路线：
//   - ssh://user@host → SSH 命令执行模式（sshPassword 为登录密码，可为空走私钥）
//   - 其他（空/ tcp://）→ SDK 直连
func (m *Manager) Executor(host, sshPassword string) (Executor, error) {
	if strings.HasPrefix(host, "ssh://") {
		return m.sshExecutorFor(host, sshPassword)
	}
	cli, err := m.Client(host)
	if err != nil {
		return nil, err
	}
	return &sdkExecutor{cli: cli}, nil
}

// sdkExecutor SDK 直连实现：把四个操作委托给已有的 SDK 封装函数。
type sdkExecutor struct {
	cli *client.Client
}

func (e *sdkExecutor) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	return ListContainers(ctx, e.cli)
}

func (e *sdkExecutor) Stats(ctx context.Context, nameOrID string) (*StatsInfo, error) {
	return Stats(ctx, e.cli, nameOrID)
}

func (e *sdkExecutor) Logs(ctx context.Context, nameOrID string, tail int) (string, error) {
	return Logs(ctx, e.cli, nameOrID, tail)
}

func (e *sdkExecutor) Ping(ctx context.Context) error {
	return Ping(ctx, e.cli)
}
