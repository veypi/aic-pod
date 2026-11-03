package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"time"

	"github.com/veypi/aic-pod/cfg"
)

// 命令配置热加载：设备所有者改 ~/.aic/config.yaml 的 commands/command_config
// 段后，统一 command hub 按定义差异重装配；未落盘的运行期命令保留。
//
// 边界（刻意收窄）：只覆盖命令段。权限域（exec/fs/net/ssh）与其它配置域参与
// permissionState 编译，仍需重启后端才生效——那是安全边界，不是功能缺口。
//
// 提权边界：配置文件在设备所有者的 fs 规则之外（AI 的 fs 工具读不到它），
// 因此这里的每次变更都是 owner 的显式动作，不是工具参数提权。

const (
	commandConfigPollInterval = 2 * time.Second
	// 同一份坏配置的重试间隔：包坏了可能一直坏，不能每 2 秒重试一次。
	commandConfigRetryInterval = 30 * time.Second
	// 同一份坏配置的日志间隔：坏配置会被反复重试，日志必须限频——
	// 否则一份坏配置能刷出几万行日志，真正的错误反而被淹没。
	commandConfigLogInterval = 60 * time.Second
)

// configReloadGate 决定「这份配置要不要再试」与「失败要不要再记日志」。
type configReloadGate struct {
	last    string    // 最后一次成功应用的配置指纹
	failed  string    // 最近一次失败的配置指纹
	lastTry time.Time // 最近一次尝试时刻
	lastLog time.Time // 最近一次记录失败的时刻
}

// shouldTry 判断这份配置现在要不要再装配一次（并记下尝试时刻）。
func (g *configReloadGate) shouldTry(fingerprint string, now time.Time) bool {
	if fingerprint == g.last {
		return false
	}
	if fingerprint == g.failed && now.Sub(g.lastTry) < commandConfigRetryInterval {
		return false
	}
	g.lastTry = now
	return true
}

// fail 记录一次失败，返回是否该写日志。
func (g *configReloadGate) fail(fingerprint string, now time.Time) bool {
	write := fingerprint != g.failed || now.Sub(g.lastLog) >= commandConfigLogInterval
	g.failed = fingerprint
	if write {
		g.lastLog = now
	}
	return write
}

// applied 记录一次成功装配。
func (g *configReloadGate) applied(fingerprint string) {
	g.last = fingerprint
	g.failed = ""
}

// watchCommandConfig 轮询配置文件指纹，变化时重装配统一命令段。
func (c *Client) watchCommandConfig(ctx context.Context) {
	path, err := cfg.Path()
	if err != nil {
		return
	}
	gate := &configReloadGate{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(commandConfigPollInterval):
		}
		fingerprint, err := fileFingerprint(path)
		if err != nil {
			continue
		}
		now := time.Now()
		if !gate.shouldTry(fingerprint, now) {
			continue
		}
		if err := c.reloadCommandConfig(); err != nil {
			// 坏配置不该把正在跑的命令掀了：保留旧状态，退避后再试。
			if gate.fail(fingerprint, now) {
				c.logf("command config reload failed: %v", err)
			}
			continue
		}
		gate.applied(fingerprint)
		c.logf("command config reloaded: %s", path)
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

// reloadCommandConfig 读配置文件的 commands/command_config 段并对齐到运行态。
func (c *Client) reloadCommandConfig() error {
	unlock := cfg.LockUpdate()
	defer unlock()
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	// options 与注册表必须同生同死：先换新 options 再重建（collectHostCommands
	// 读的就是它），重建失败立即回滚——否则注册表是旧定义、options 却是新的，
	// commands remove --persist 判定与 commands config 都会作用在错的对象上。
	previous := c.options()
	c.setCommandOptions(fileCfg.Commands, fileCfg.CommandConfig)
	if err := c.cmds.reload(); err != nil {
		c.setCommandOptions(previous.Commands, previous.CommandConfig)
		return err
	}
	return nil
}
