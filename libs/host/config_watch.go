package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/mcpx"
)

// 服务配置热加载：设备所有者改 ~/.aic/config.yaml 的 mcp.servers 段后，运行期
// 按差异重装配服务——姿态（no_sandbox）、命令与环境、内置项禁用都生效。
//
// 边界（刻意收窄）：只覆盖服务段。权限域（exec/fs/net/ssh）与其它配置域参与
// permissionState 编译，仍需重启后端才生效——那是安全边界，不是功能缺口。
//
// 提权边界：配置文件在设备所有者的 fs 规则之外（AI 的 fs 工具读不到它），
// 因此这里的每次变更都是 owner 的显式动作，不是工具参数提权。

const serviceConfigPollInterval = 2 * time.Second

// watchServiceConfig 轮询配置文件指纹，变化时重装配服务段。
func (c *Client) watchServiceConfig(ctx context.Context) {
	path, err := cfg.Path()
	if err != nil {
		return
	}
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(serviceConfigPollInterval):
		}
		fingerprint, err := fileFingerprint(path)
		if err != nil || fingerprint == last {
			continue
		}
		if err := c.reloadServiceConfig(path); err != nil {
			// 坏配置不该把正在跑的服务掀了：保留旧状态，下轮再试。
			c.logf("service config reload failed: %v", err)
			continue
		}
		last = fingerprint
		c.logf("service config reloaded: %s", path)
	}
}

// fileFingerprint 取配置文件内容指纹；文件不存在时返回空串（不算变化）。
func fileFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// reloadServiceConfig 读配置文件的 mcp.servers 段并对齐到运行态。
func (c *Client) reloadServiceConfig(path string) error {
	unlock := cfg.LockUpdate()
	defer unlock()
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	if err := c.applyServiceSet(mcpServers(fileCfg.MCP.Servers)); err != nil {
		return err
	}
	if c.cmds != nil {
		c.setCommandOptions(fileCfg.Commands, fileCfg.CommandConfig)
		if err := c.cmds.reload(); err != nil {
			return fmt.Errorf("commands reload: %w", err)
		}
	}
	return nil
}

// applyServiceSet 把文件来源的服务集合对齐到 desired：增删改生效；
// 进程级（未落盘）注册的服务不在集合里，不受影响。
func (c *Client) applyServiceSet(desired map[string]mcpx.Config) error {
	c.svc.registrationMu.Lock()
	defer c.svc.registrationMu.Unlock()
	current := c.fileServices()
	for alias, old := range current {
		want, ok := desired[alias]
		if ok && !want.Disabled && reflect.DeepEqual(old, want) {
			continue
		}
		// 配置删除、配置禁用或配置变更：先摘除，再由下面的循环按新配置登记。
		if err := c.removeServiceRuntime(context.Background(), alias); err != nil {
			return fmt.Errorf("remove %s: %w", alias, err)
		}
	}
	for alias, want := range desired {
		if want.Disabled {
			continue
		}
		if old, ok := current[alias]; ok && reflect.DeepEqual(old, want) {
			continue
		}
		// 运行态已有同名服务（mcp add 刚登记、配置刚落盘）：配置一致就不重启，
		// 不一致才以文件为准重装。
		if running, ok := c.ownerService(alias); ok && reflect.DeepEqual(running, want) {
			continue
		}
		if c.svc.registry != nil && c.svc.registry.Has(alias) {
			if err := c.removeServiceRuntime(context.Background(), alias); err != nil {
				return fmt.Errorf("replace %s: %w", alias, err)
			}
		}
		if err := c.addServiceRuntime(alias, want); err != nil {
			return fmt.Errorf("add %s: %w", alias, err)
		}
	}
	c.svc.mu.Lock()
	c.svc.fileServices = desired
	c.svc.mu.Unlock()
	return nil
}

// ownerService 返回运行态里该别名的有效配置（所有者配置优先）。
func (c *Client) ownerService(alias string) (mcpx.Config, bool) {
	c.svc.mu.RLock()
	defer c.svc.mu.RUnlock()
	if cfg, ok := c.svc.owner[alias]; ok {
		return cfg, true
	}
	return mcpx.Config{}, false
}
