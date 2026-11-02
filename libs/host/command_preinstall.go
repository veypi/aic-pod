package host

// command_preinstall.go — 官方设备包预装（browser / cua）。
//
// 与 aic 云端 InitBuiltin 同语义：pod 内嵌 aic-skills 包内容，启动时无条件把
// 预装包刷成内嵌版本（整目录原子交换），用户无需 commands install。与早年
// "技能代码写进 pod" 的耦合无关：pod 仍只解释 manifest 的 host 段。
//
// 免 Go 工具链：发布流水线按平台预编译设备程序，Desktop 经 extraResources
// 附带并注入 AIC_SKILL_BIN_DIR；CLI 发行附带 skill-bin 包，默认可执行文件旁
// 的 skill-bin/。定版时把 <资源目录>/<包>/ 复制进包内 bin/，run 的相对路径
// 解析（command_model.go）不变。资源缺失时保留源码与旧构建产物，命令调用
// 会报明确的"程序不存在"，不静默伪装成功。
//
// 预装包不可 commands install/update/uninstall（由 pod 版本统一管理）。

import (
	"fmt"
	"os"
	"path/filepath"

	aicskills "github.com/veypi/aic-skills"
)

// preinstalledCommandPackages 是启动定版的官方包清单（只含带设备命令的包）。
var preinstalledCommandPackages = []string{"browser", "cua"}

func preinstalledCommandPackage(id string) bool {
	for _, name := range preinstalledCommandPackages {
		if name == id {
			return true
		}
	}
	return false
}

// preinstallBinaryRoot 返回随发行附带的设备程序资源目录。
// 优先级：AIC_SKILL_BIN_DIR（Desktop 注入）→ 可执行文件旁的 skill-bin/。
func preinstallBinaryRoot() string {
	if dir := os.Getenv("AIC_SKILL_BIN_DIR"); dir != "" {
		return dir
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "skill-bin")
}

// preinstallCommandPackages 启动定版：在首次命令面 reload 之前调用。
func (c *Client) preinstallCommandPackages() error {
	c.packageMu.Lock()
	defer c.packageMu.Unlock()
	root, err := c.commandPackageRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	state, err := c.loadCommandPackageState()
	if err != nil {
		return err
	}
	for _, id := range preinstalledCommandPackages {
		if err := c.preinstallCommandPackage(root, &state, id); err != nil {
			return fmt.Errorf("preinstall %s: %w", id, err)
		}
	}
	return c.saveCommandPackageState(state)
}

func (c *Client) preinstallCommandPackage(root string, state *commandPackageState, id string) error {
	dir, err := aicskills.Open(id)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(root, ".preinstall-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.CopyFS(tmp, dir); err != nil {
		return err
	}
	manifest, err := parseCommandManifestFile(tmp)
	if err != nil {
		return err
	}
	if _, err := skillCommands(id, tmp, 0, manifest); err != nil {
		return fmt.Errorf("manifest.json: %w", err)
	}
	final := filepath.Join(root, id)
	// 旧包里的设备构建产物先带过来，随后被随发行预编译的二进制覆盖；
	// 没有附带二进制的环境（如源码运行）仍可使用设备自行构建的 bin/。
	preserveCommandPackageArtifacts(final, tmp)
	if err := copyPreinstalledBinaries(tmp, id); err != nil {
		return err
	}

	record, exists := state.Packages[id]
	if !exists {
		record = commandPackageRecord{SkillID: id, Order: state.NextOrder}
		state.NextOrder++
	}
	record.Revision++
	record.Builtin = true

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
	state.Packages[id] = record
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

// copyPreinstalledBinaries 把发行附带的 <资源目录>/<包>/ 复制为包内 bin/。
// 资源目录不存在或该包没有附带程序时什么都不做（源码运行形态）。
func copyPreinstalledBinaries(packageRoot, id string) error {
	resourceRoot := preinstallBinaryRoot()
	if resourceRoot == "" {
		return nil
	}
	source := filepath.Join(resourceRoot, id)
	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("preinstalled binary source %s is not a directory", source)
	}
	bin := filepath.Join(packageRoot, "bin")
	if err := os.RemoveAll(bin); err != nil {
		return err
	}
	return os.CopyFS(bin, os.DirFS(source))
}
