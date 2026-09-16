package api

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/vigo"
	"github.com/veypi/vigo/logv"
)

// configView 是 get_config 的返回视图（含 key——设置窗口需显示当前凭证；
// 本地 API 受 code 校验保护）。
type configView struct {
	Host        string   `json:"host"`
	Key         string   `json:"key"`
	WorkDir     string   `json:"work_dir"`
	ExecTimeout string   `json:"exec_timeout"`
	HomePath    string   `json:"home_path"`
	ExecPolicy  string   `json:"exec_policy"`
	ExecDeny    []string `json:"exec_deny"`
	ExecAllow   []string `json:"exec_allow"`
	FsPolicy    string   `json:"fs_policy"`
	FsDeny      []string `json:"fs_deny"`
	FsAllow     []string `json:"fs_allow"`
	NetPolicy   string   `json:"net_policy"`
	NetDeny     []string `json:"net_deny"`
	NetAllow    []string `json:"net_allow"`
	SshPolicy   string   `json:"ssh_policy"`
	SshDeny     []string `json:"ssh_deny"`
	SshAllow    []string `json:"ssh_allow"`
}

// GetConfig 返回当前有效配置（cfg.Global：启动解析值 + 页面写操作同步）。
// 注：隐藏配置（no_sandbox 等）不在此视图暴露——仅配置文件/flag/env 可配。
func GetConfig(x *vigo.X) (*configView, error) {
	o := effective()
	a := cfg.AuthSnapshot()
	return &configView{Host: o.Host, Key: o.Key, WorkDir: o.WorkDir, ExecTimeout: o.ExecTimeout,
		HomePath:   o.NormalizedHomePath(),
		ExecPolicy: a.ExecPolicy, ExecDeny: a.ExecDeny, ExecAllow: a.ExecAllow,
		FsPolicy: a.FsPolicy, FsDeny: a.FsDeny, FsAllow: a.FsAllow,
		NetPolicy: a.NetPolicy, NetDeny: a.NetDeny, NetAllow: a.NetAllow,
		SshPolicy: a.SshPolicy, SshDeny: a.SshDeny, SshAllow: a.SshAllow}, nil
}

// SetConfigReq 是 set_config 的白名单参数（host/work_dir/exec_timeout/home_path 可写；
// key 不走 set_config——只走 Bind，body 中的 credential 不得被持久化）。
// 隐藏配置（no_sandbox 等）不可经 set_config 修改，只能改配置文件。
// 授权九键（policy/deny/allow × fs/net/ssh）：policy 空串 = 不改；列表 nil = 不改
// （保持现状），非 nil（含空数组）= 整体替换——空数组即清空，是 grant --permanent
// 的唯一回撤出口。
type SetConfigReq struct {
	Host        string    `json:"host" src:"json"`
	WorkDir     string    `json:"work_dir" src:"json"`
	ExecTimeout string    `json:"exec_timeout" src:"json"`
	HomePath    string    `json:"home_path" src:"json"`
	ExecPolicy  string    `json:"exec_policy" src:"json"`
	ExecDeny    *[]string `json:"exec_deny" src:"json"`
	ExecAllow   *[]string `json:"exec_allow" src:"json"`
	FsPolicy    string    `json:"fs_policy" src:"json"`
	FsDeny      *[]string `json:"fs_deny" src:"json"`
	FsAllow     *[]string `json:"fs_allow" src:"json"`
	NetPolicy   string    `json:"net_policy" src:"json"`
	NetDeny     *[]string `json:"net_deny" src:"json"`
	NetAllow    *[]string `json:"net_allow" src:"json"`
	SshPolicy   string    `json:"ssh_policy" src:"json"`
	SshDeny     *[]string `json:"ssh_deny" src:"json"`
	SshAllow    *[]string `json:"ssh_allow" src:"json"`
}

// validPolicy 校验 policy 取值（空串 = 不改，合法）。
func validPolicy(s string) bool {
	return s == "" || s == cfg.PolicyDeny || s == cfg.PolicyOpen
}

// SetConfig 持久化运行参数并应用：基于文件配置落盘（flag/env 覆盖不落盘），
// 内存态同步 cfg.Global；host/work_dir/exec_timeout 变更经 ApplyConfig 应用
// （保留会话与 bg 任务，NATS 地址变化时重连）。
func SetConfig(x *vigo.X, req *SetConfigReq) (*OKResp, error) {
	unlock := cfg.LockUpdate()
	defer unlock()
	if s := strings.TrimSpace(req.ExecTimeout); s != "" {
		if _, err := time.ParseDuration(s); err != nil {
			return nil, vigo.ErrInvalidArg.WithString("invalid exec_timeout: " + err.Error())
		}
	}
	// 授权配置显式校验（policy 取值 / net/ssh 条目形态——运行期坏条目静默跳过，
	// 写入前必须显式报错，否则用户以为生效）
	if !validPolicy(req.ExecPolicy) || !validPolicy(req.FsPolicy) || !validPolicy(req.NetPolicy) || !validPolicy(req.SshPolicy) {
		return nil, vigo.ErrInvalidArg.WithString("invalid policy: want deny | open")
	}
	for name, list := range map[string]*[]string{"net_deny": req.NetDeny, "net_allow": req.NetAllow, "ssh_deny": req.SshDeny, "ssh_allow": req.SshAllow} {
		if list != nil {
			if err := netauth.ValidateEntries(*list); err != nil {
				return nil, vigo.ErrInvalidArg.WithString("invalid " + name + ": " + err.Error())
			}
		}
	}
	// 持久化运行参数（基于文件配置）
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	if h := strings.TrimSpace(req.Host); h != "" {
		fileCfg.Host = h
	}
	// work_dir：~ 展开 + 归一绝对路径 + 有效性校验，落盘即真实路径
	//（Go exec 不做 shell 展开，配置页填 ~/test 会因路径不存在导致所有 exec 失败）
	wd := strings.TrimSpace(req.WorkDir)
	if wd != "" {
		wd = expandHome(wd)
		abs, err := filepath.Abs(wd)
		if err != nil {
			return nil, vigo.ErrInvalidArg.WithString("invalid work_dir: " + err.Error())
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return nil, vigo.ErrInvalidArg.WithString("invalid work_dir: not a directory: " + abs)
		}
		wd = abs
	}
	fileCfg.WorkDir = wd
	fileCfg.ExecTimeout = strings.TrimSpace(req.ExecTimeout)
	// 授权九键：policy 空串 = 不改；列表 nil = 不改，非 nil = 整体替换。
	authChanged := false
	if req.ExecPolicy != "" {
		fileCfg.ExecPolicy = req.ExecPolicy
		authChanged = true
	}
	if req.ExecDeny != nil {
		fileCfg.ExecDeny = *req.ExecDeny
		authChanged = true
	}
	if req.ExecAllow != nil {
		fileCfg.ExecAllow = *req.ExecAllow
		authChanged = true
	}
	if req.FsPolicy != "" {
		fileCfg.FsPolicy = req.FsPolicy
		authChanged = true
	}
	if req.NetPolicy != "" {
		fileCfg.NetPolicy = req.NetPolicy
		authChanged = true
	}
	if req.SshPolicy != "" {
		fileCfg.SshPolicy = req.SshPolicy
		authChanged = true
	}
	if req.FsDeny != nil {
		fileCfg.FsDeny = *req.FsDeny
		authChanged = true
	}
	if req.FsAllow != nil {
		fileCfg.FsAllow = *req.FsAllow
		authChanged = true
	}
	if req.NetDeny != nil {
		fileCfg.NetDeny = *req.NetDeny
		authChanged = true
	}
	if req.NetAllow != nil {
		fileCfg.NetAllow = *req.NetAllow
		authChanged = true
	}
	if req.SshDeny != nil {
		fileCfg.SshDeny = *req.SshDeny
		authChanged = true
	}
	if req.SshAllow != nil {
		fileCfg.SshAllow = *req.SshAllow
		authChanged = true
	}
	// home_path：必须以单个 / 开头（// 开头是协议相对 URL，拼接后会跳转到别的站点，拒绝）
	if hp := strings.TrimSpace(req.HomePath); hp != "" {
		if !strings.HasPrefix(hp, "/") || strings.HasPrefix(hp, "//") {
			return nil, vigo.ErrInvalidArg.WithString("invalid home_path: must be a path starting with / (e.g. / or /a)")
		}
		fileCfg.HomePath = hp
	} else {
		fileCfg.HomePath = "/" // 清空 = 恢复默认首页
	}
	fileCfg.Normalize()
	if err := fileCfg.ValidateAuth(); err != nil {
		return nil, vigo.ErrInvalidArg.WithError(err)
	}
	if err := cfg.Save(fileCfg); err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	mu.Lock()
	hostChanged := cfg.Global.Host != fileCfg.Host
	workDirChanged := cfg.Global.WorkDir != fileCfg.WorkDir
	execTimeoutChanged := cfg.Global.ExecTimeout != fileCfg.ExecTimeout
	cfg.Global.Host = fileCfg.Host
	cfg.Global.WorkDir = fileCfg.WorkDir
	cfg.Global.ExecTimeout = fileCfg.ExecTimeout
	cfg.Global.HomePath = fileCfg.HomePath
	o := *cfg.Global
	mu.Unlock()
	cfg.SetAuth(cfg.AuthFrom(fileCfg))
	// 运行参数变更（host/work_dir/exec_timeout/授权模型）：应用新配置——保留会话与
	// bg 任务，仅更新参数；NATS 地址变化时重连（Client.Reconfigure，内部同步
	// fsauth/netauth Policy：work_dir 重设 + 授权名单重载，内存即时生效）。
	if host.Running() && (hostChanged || workDirChanged || execTimeoutChanged || authChanged) {
		if err := host.ApplyConfig(o); err != nil {
			logv.Warn().Msgf("apply config failed: %v", err)
		}
	}
	return &OKResp{OK: true}, nil
}

// expandHome 展开 work_dir 的 ~ 前缀（~ 或 ~/xxx → 用户主目录）。
// 配置保存时调用：落盘即为真实绝对路径，运行时不再需要 shell 展开语义。
func expandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}
