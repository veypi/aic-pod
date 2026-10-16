package host

// 内置工具包装载（技能包 = 插件单元，aic docs/skill.md）：pod 只读各包的
// cli/ 维度——cli/manifest.json 声明设备服务（协议 kind / 别名 / 程序解析 /
// 生命周期动词 / stream 通道），程序源码经 aicskills embed 随 pod 分发。
//
// 模型：内嵌集 = 预装（启动即注册）；所有者 mcp.servers.<alias> 同名配置
// 整体替换内置项（含 disabled:true 关闭）。用户第三方包的安装链路不在此层。

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/libs/mcpx"
	aicskills "github.com/veypi/aic-skills"
)

// ---- cli/manifest.json 契约 ----

type packService struct {
	Kind        string            `json:"kind"` // mcp-stdio | tcp
	URL         string            `json:"url,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	NoSandbox   bool              `json:"no_sandbox,omitempty"`
	IdleTimeout string            `json:"idle_timeout,omitempty"`
}

// packProgram 描述一个可执行文件的解析规则（按序：path_env → bundle → 默认名）。
type packProgram struct {
	Sets           string            `json:"sets,omitempty"`     // "command"：解析结果作为 Command
	SetsEnv        string            `json:"sets_env,omitempty"` // 解析成功注入该 env（如 browser 的 Chrome）
	DefaultCommand string            `json:"default_command,omitempty"`
	Optional       bool              `json:"optional,omitempty"` // 解析不到不报错（sets_env 类）
	PathEnv        string            `json:"path_env,omitempty"`
	BundleEnv      string            `json:"bundle_env,omitempty"`
	BundleDir      string            `json:"bundle_dir,omitempty"`    // 可执行文件旁的目录名
	BundleSubdir   string            `json:"bundle_subdir,omitempty"` // 支持 {os}/{arch}（os_map/arch_map 折算）
	OSMap          map[string]string `json:"os_map,omitempty"`
	ArchMap        map[string]string `json:"arch_map,omitempty"`
	Binary         map[string]string `json:"binary,omitempty"`           // darwin/windows/linux/"other"
	MacAppFallback string            `json:"mac_app_fallback,omitempty"` // darwin 且 PATH 查不到时的兜底
}

type packStream struct {
	Channel string `json:"channel"`
}

// packExeSearch 可执行文件探测（按 OS 取一段）。
type packExeSearch struct {
	Roots      []string `json:"roots"`       // 搜索根（自身可含 * 通配；支持 {home} 等占位）
	Glob       string   `json:"glob"`        // 根下二进制相对 glob（版本目录取最新）
	PathLookup string   `json:"path_lookup"` // PATH 兜底名
}

type packLifecycle struct {
	ExeEnv     string                   `json:"exe_env"`
	ExeSearch  map[string]packExeSearch `json:"exe_search"`
	PortEnv    string                   `json:"port_env"`
	BootAsset  string                   `json:"boot_asset"` // 包内 addon 源码路径（cli/...）
	AddonsDirs map[string]string        `json:"addons_dirs"`
}

type packManifest struct {
	Alias     string         `json:"alias"`
	Service   packService    `json:"service"`
	Programs  []packProgram  `json:"programs,omitempty"`
	Stream    *packStream    `json:"stream,omitempty"`
	Lifecycle *packLifecycle `json:"lifecycle,omitempty"`
	Verbs     []string       `json:"verbs,omitempty"`
}

var packsOnce struct {
	once sync.Once
	m    map[string]*packManifest
	err  error
}

// builtinPacks 读内嵌技能集的全部 cli/manifest.json（无清单的包跳过）。
func builtinPacks() (map[string]*packManifest, error) {
	packsOnce.once.Do(func() {
		packsOnce.m = map[string]*packManifest{}
		for _, name := range aicskills.List() {
			dir, err := aicskills.Open(name)
			if err != nil {
				packsOnce.err = err
				return
			}
			data, err := fs.ReadFile(dir, "cli/manifest.json")
			if err != nil {
				continue // 无 cli 维度 = 纯云端包
			}
			var m packManifest
			if err := json.Unmarshal(data, &m); err != nil {
				packsOnce.err = fmt.Errorf("pack %s: cli/manifest.json: %w", name, err)
				return
			}
			if m.Alias == "" {
				m.Alias = name
			}
			packsOnce.m[m.Alias] = &m
		}
	})
	return packsOnce.m, packsOnce.err
}

// packAsset 读包内文件（embed 只读；launch/install 时写到运行目录再用）。
func packAsset(pack, path string) ([]byte, error) {
	dir, err := aicskills.Open(pack)
	if err != nil {
		return nil, err
	}
	return fs.ReadFile(dir, path)
}

// ---- 占位符与程序解析 ----

// expandPackVars 展开清单占位符：{home} {workdir} {env:NAME}。
func expandPackVars(s, home, workDir string) string {
	s = strings.ReplaceAll(s, "{home}", home)
	s = strings.ReplaceAll(s, "{workdir}", workDir)
	for {
		i := strings.Index(s, "{env:")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			return s
		}
		name := s[i+5 : i+j]
		s = s[:i] + os.Getenv(name) + s[i+j+1:]
	}
}

func (p packProgram) binary() string {
	if p.Binary != nil {
		if b := p.Binary[runtime.GOOS]; b != "" {
			return b
		}
		return p.Binary["other"]
	}
	return p.DefaultCommand
}

// bundleRoot 解析 bundle 根：bundle_env 指向的目录，或可执行文件旁的同名目录；
// 目录必须存在（与桌面发行布局一致）。
func bundleRoot(env, dir string) string {
	if env != "" {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	if dir == "" {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	root := filepath.Join(filepath.Dir(exe), dir)
	if st, err := os.Stat(root); err == nil && st.IsDir() {
		return root
	}
	return ""
}

func (p packProgram) bundleSubdir() string {
	if p.BundleSubdir == "" {
		return ""
	}
	osName, arch := runtime.GOOS, runtime.GOARCH
	if m := p.OSMap[osName]; m != "" {
		osName = m
	}
	if m := p.ArchMap[arch]; m != "" {
		arch = m
	}
	s := strings.ReplaceAll(p.BundleSubdir, "{os}", osName)
	return strings.ReplaceAll(s, "{arch}", arch)
}

// resolveProgram 按序解析：path_env → bundle →（sets=command 时）默认名。
// 默认名在 darwin 且 PATH 查不到时可回退 mac_app（CuaDriver.app 布局）。
// optional 的程序解析不到返回 ""（不报错）。
func (p packProgram) resolveProgram() string {
	if p.PathEnv != "" {
		if v := os.Getenv(p.PathEnv); v != "" {
			return v
		}
	}
	if root := bundleRoot(p.BundleEnv, p.BundleDir); root != "" {
		return filepath.Join(root, p.bundleSubdir(), filepath.FromSlash(p.binary()))
	}
	if p.Sets == "command" {
		cmd := p.DefaultCommand
		if p.MacAppFallback != "" && runtime.GOOS == "darwin" {
			if _, err := exec.LookPath(cmd); err != nil {
				return p.MacAppFallback
			}
		}
		return cmd
	}
	return ""
}

// mcpConfig 由清单生成 mcpx.Config（stdio 类服务）。
func (m *packManifest) mcpConfig(home, workDir string) mcpx.Config {
	cfg := mcpx.Config{
		Args:        append([]string(nil), m.Service.Args...),
		NoSandbox:   m.Service.NoSandbox,
		IdleTimeout: m.Service.IdleTimeout,
	}
	if m.Service.Cwd != "" {
		cfg.Cwd = expandPackVars(m.Service.Cwd, home, workDir)
	}
	if len(m.Service.Env) > 0 {
		cfg.Env = make(map[string]string, len(m.Service.Env))
		for k, v := range m.Service.Env {
			cfg.Env[k] = expandPackVars(v, home, workDir)
		}
	}
	for _, p := range m.Programs {
		path := p.resolveProgram()
		if path == "" {
			continue
		}
		if p.Sets == "command" {
			cfg.Command = path
		}
		if p.SetsEnv != "" {
			if cfg.Env == nil {
				cfg.Env = map[string]string{}
			}
			cfg.Env[p.SetsEnv] = path
		}
	}
	return cfg
}

// mcpServers 生成 MCP 服务集：内嵌包清单默认项 + 所有者配置（同名整体替换，
// 含 disabled:true 关闭内置项）。
func mcpServers(configured map[string]mcpx.Config, workDir string) (map[string]mcpx.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	packs, err := builtinPacks()
	if err != nil {
		return nil, err
	}
	servers := make(map[string]mcpx.Config, len(packs)+len(configured))
	for _, m := range packs {
		if m.Service.Kind != "mcp-stdio" {
			continue // tcp 等形态不进 MCP 管理器（registerBuiltinPacks 接管）
		}
		servers[m.Alias] = m.mcpConfig(home, workDir)
	}
	for name, config := range configured {
		servers[name] = config
	}
	return servers, nil
}

// packExeCandidates 按当前 OS 探测程序：exe_env → 搜索根 glob（版本目录取
// 最新）→ PATH 兜底。返回 "" = 未安装。
func (lc *packLifecycle) findExecutable() string {
	if lc.ExeEnv != "" {
		if v := os.Getenv(lc.ExeEnv); v != "" {
			return v
		}
	}
	search, ok := lc.ExeSearch[runtime.GOOS]
	if !ok {
		return ""
	}
	home, _ := os.UserHomeDir()
	var found []string
	for _, root := range search.Roots {
		root = expandPackVars(root, home, "")
		roots, err := filepath.Glob(filepath.FromSlash(root))
		if err != nil {
			continue
		}
		for _, r := range roots {
			matches, err := filepath.Glob(filepath.Join(r, filepath.FromSlash(search.Glob)))
			if err != nil {
				continue
			}
			found = append(found, matches...)
		}
	}
	if len(found) > 0 {
		sort.Strings(found)
		return found[len(found)-1] // 版本目录序：最新
	}
	if search.PathLookup != "" {
		if p, err := exec.LookPath(search.PathLookup); err == nil {
			return p
		}
	}
	return ""
}
