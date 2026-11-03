package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/veypi/aic-pod/cfg"
)

const (
	maxCommandPackageContent = 128 << 20
	maxCommandPackageFile    = 32 << 20
	maxCommandPackageFiles   = 4096
)

type commandPackageRecord struct {
	SkillID  string `json:"skill_id"`
	Order    uint64 `json:"order"`
	Revision uint64 `json:"revision"`
	// Builtin = 启动定版的官方预装包；不可 install/update/uninstall。
	Builtin bool `json:"builtin,omitempty"`
}

type commandPackageState struct {
	NextOrder uint64                          `json:"next_order"`
	Packages  map[string]commandPackageRecord `json:"packages"`
}

var skillIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func (c *Client) commandPackageRoot() (string, error) {
	if c.commandPackages != "" {
		return c.commandPackages, nil
	}
	dir, err := cfg.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "packages"), nil
}

func (c *Client) loadCommandPackageState() (commandPackageState, error) {
	state := commandPackageState{NextOrder: 1, Packages: map[string]commandPackageRecord{}}
	root, err := c.commandPackageRoot()
	if err != nil {
		return state, err
	}
	data, err := os.ReadFile(filepath.Join(root, "state.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("command package state: %w", err)
	}
	if state.Packages == nil {
		state.Packages = map[string]commandPackageRecord{}
	}
	if state.NextOrder == 0 {
		state.NextOrder = 1
		for _, pkg := range state.Packages {
			if pkg.Order >= state.NextOrder {
				state.NextOrder = pkg.Order + 1
			}
		}
	}
	return state, nil
}

func (c *Client) saveCommandPackageState(state commandPackageState) error {
	root, err := c.commandPackageRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(root, ".state-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(root, "state.json"))
}

func (c *Client) installCommandPackage(ctx context.Context, skillID string) error {
	c.packageMu.Lock()
	defer c.packageMu.Unlock()
	if !skillIDPattern.MatchString(skillID) {
		return fmt.Errorf("invalid skill_id %q", skillID)
	}
	if preinstalledCommandPackage(skillID) {
		return fmt.Errorf("skill package %q is preinstalled and managed by the pod release", skillID)
	}
	root, err := c.commandPackageRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	cloud, err := newCloudFS(c.options().Host, c.authToken)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// 取包走 /fs/cloud 面（设备唯一授权的文件面 + 服务端 skill 门）：包目录即 /skills/{id}。
	if err := fetchCommandPackage(ctx, cloud, skillID, tmp); err != nil {
		return err
	}
	manifest, err := parseCommandManifestFile(tmp)
	if err != nil {
		return err
	}
	if _, err := skillCommands(skillID, tmp, 0, manifest); err != nil {
		return fmt.Errorf("manifest.json: %w", err)
	}
	// 更新不冲掉设备侧构建产物：bin/ 是设备编译出来的（平台包里只有源码），
	// 整目录换血会让命令在更新后直接失联（run 指向 bin/xxx 却没了）。
	preserveCommandPackageArtifacts(filepath.Join(root, skillID), tmp)

	state, err := c.loadCommandPackageState()
	if err != nil {
		return err
	}
	record, exists := state.Packages[skillID]
	if !exists {
		record = commandPackageRecord{SkillID: skillID, Order: state.NextOrder, Revision: 1}
		state.NextOrder++
	} else {
		record.Revision++
	}
	final := filepath.Join(root, skillID)
	backup := ""
	if _, err := os.Stat(final); err == nil {
		placeholder, err := os.MkdirTemp(root, ".backup-*")
		if err != nil {
			return err
		}
		if err := os.Remove(placeholder); err != nil {
			return err
		}
		backup = placeholder
		if err := os.Rename(final, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		if backup != "" {
			_ = os.Rename(backup, final)
		}
		return err
	}
	state.Packages[skillID] = record
	if err := c.saveCommandPackageState(state); err != nil {
		_ = os.RemoveAll(final)
		if backup != "" {
			_ = os.Rename(backup, final)
		}
		return err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	c.packageMu.Unlock()
	defer c.packageMu.Lock()
	return c.cmds.reload()
}

// preserveCommandPackageArtifacts 把旧包里设备侧生成的构建产物带进新包。
// 平台包只带源码，bin/ 由设备自己编译；安装/更新换目录时把它留下来。
func preserveCommandPackageArtifacts(oldRoot, newRoot string) {
	oldBin := filepath.Join(oldRoot, "bin")
	info, err := os.Stat(oldBin)
	if err != nil || !info.IsDir() {
		return
	}
	newBin := filepath.Join(newRoot, "bin")
	if _, err := os.Stat(newBin); err == nil {
		return
	}
	_ = os.CopyFS(newBin, os.DirFS(oldBin))
}

func (c *Client) uninstallCommandPackage(skillID string) error {
	c.packageMu.Lock()
	defer c.packageMu.Unlock()
	state, err := c.loadCommandPackageState()
	if err != nil {
		return err
	}
	record, ok := state.Packages[skillID]
	if !ok {
		return fmt.Errorf("skill package %q is not installed", skillID)
	}
	if record.Builtin {
		return fmt.Errorf("skill package %q is preinstalled and managed by the pod release", skillID)
	}
	delete(state.Packages, skillID)
	if err := c.saveCommandPackageState(state); err != nil {
		return err
	}
	root, err := c.commandPackageRoot()
	if err != nil {
		return err
	}
	removeErr := os.RemoveAll(filepath.Join(root, skillID))
	// 状态删除后的所有收尾都必须重建注册表；否则重试会因记录已消失而
	// 提前返回，旧命令和常驻进程却继续存活。清理错误仍显式返回。
	c.packageMu.Unlock()
	reloadErr := c.cmds.reload()
	configErr := c.clearCommandPackageConfig(skillID)
	c.packageMu.Lock()
	return errors.Join(removeErr, reloadErr, configErr)
}

func parseCommandManifestFile(root string) (*commandManifest, error) {
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("skill has no device manifest.json")
	}
	if err != nil {
		return nil, err
	}
	m, err := parseCommandManifest(data)
	if err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	return m, nil
}

func (c *Client) collectHostCommands(runtime map[string]*hostCommand) (map[string]*hostCommand, error) {
	c.packageMu.RLock()
	defer c.packageMu.RUnlock()
	defs := map[string]*hostCommand{}
	state, err := c.loadCommandPackageState()
	if err != nil {
		return nil, err
	}
	root, err := c.commandPackageRoot()
	if err != nil {
		return nil, err
	}
	records := make([]commandPackageRecord, 0, len(state.Packages))
	for _, record := range state.Packages {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Order == records[j].Order {
			return records[i].SkillID < records[j].SkillID
		}
		return records[i].Order < records[j].Order
	})
	for _, record := range records {
		packageRoot := filepath.Join(root, record.SkillID)
		manifest, err := parseCommandManifestFile(packageRoot)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", record.SkillID, err)
		}
		packageDefs, err := skillCommands(record.SkillID, packageRoot, record.Order, manifest)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", record.SkillID, err)
		}
		for id, def := range packageDefs {
			def.Revision = record.Revision
			def.Local = c.options().CommandConfig[id]
			defs[id] = def
		}
	}
	for alias, config := range c.options().Commands {
		id := "config:" + alias
		defs[id] = &hostCommand{
			ID: id, Alias: alias, Source: commandSourceConfig, Config: config,
			Local: c.options().CommandConfig[id],
		}
	}
	for id, def := range runtime {
		next := def.clone()
		next.Local = c.options().CommandConfig[id]
		defs[id] = next
	}
	return defs, nil
}

func (c *Client) clearCommandPackageConfig(skillID string) error {
	return c.mutateConfigFile(func(fileCfg *cfg.Options) error {
		prefix := "skill:" + skillID + ":"
		for id := range fileCfg.CommandConfig {
			if strings.HasPrefix(id, prefix) {
				delete(fileCfg.CommandConfig, id)
			}
		}
		return nil
	})
}

func (c *Client) setCommandOptions(commands map[string]cfg.CommandConfig, configs map[string]cfg.CommandDefConfig) {
	c.optsMu.Lock()
	c.opts.Commands = commands
	c.opts.CommandConfig = configs
	c.optsMu.Unlock()
}

func (c *Client) persistCommandConfig(alias string, config *cfg.CommandConfig) error {
	return c.mutateConfigFile(func(fileCfg *cfg.Options) error {
		if fileCfg.Commands == nil {
			fileCfg.Commands = map[string]cfg.CommandConfig{}
		}
		if config == nil {
			delete(fileCfg.Commands, alias)
		} else {
			fileCfg.Commands[alias] = *config
		}
		return nil
	})
}
