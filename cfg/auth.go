package cfg

import (
	"fmt"

	"github.com/veypi/vbox"
)

// AuthCfg 是四张规则表的纯数据快照（permissionState 编译输入）。exec 使用
// 原表序，FS/NET/SSH 使用 level + 原表序。
type AuthCfg struct {
	ExecPolicy       string
	ExecRules        []string
	FsPolicy         string
	FsRules          []string
	NetPolicy        string
	NetRules         []string
	NetUpstreamProxy string
	SshPolicy        string
	SshRules         []string
}

// AuthFrom 从 Options 取授权快照（纯函数；host permissionState 的启动构建
// 与 permanent grant 的候选编译共用）。policy 在读点归一化（防空值/非法值
// 漂移到安全语义外：fs/ssh 空=deny，net 空=open）。
func AuthFrom(o *Options) AuthCfg {
	return AuthCfg{
		ExecPolicy: NormalizePolicy(o.ExecPolicy, PolicyOpen), ExecRules: append([]string(nil), o.ExecRules...),
		FsPolicy: NormalizePolicy(o.FsPolicy, PolicyDeny), FsRules: append([]string(nil), o.FsRules...),
		NetPolicy: NormalizePolicy(o.NetPolicy, PolicyOpen), NetRules: append([]string(nil), o.NetRules...),
		NetUpstreamProxy: o.NetUpstreamProxy,
		SshPolicy:        NormalizePolicy(o.SshPolicy, PolicyDeny), SshRules: append([]string(nil), o.SshRules...),
	}
}

// AuthFieldError 标记授权配置中失败的具体字段（设置面按 field 展示错误）。
type AuthFieldError struct {
	Field string
	Err   error
}

func (e *AuthFieldError) Error() string { return e.Err.Error() }
func (e *AuthFieldError) Unwrap() error { return e.Err }

// Validate 校验四域授权配置：policy 取值（空 = 未设置，合法）与有序规则表。
// 这是授权规则语法的唯一实现——Options.ValidateAuth、permissionState 编译与
// 设置面保存都走这里。
func (a AuthCfg) Validate() error {
	for _, p := range []struct {
		field, value string
	}{
		{"exec_policy", a.ExecPolicy},
		{"fs_policy", a.FsPolicy},
		{"net_policy", a.NetPolicy},
		{"ssh_policy", a.SshPolicy},
	} {
		if p.value != "" && p.value != PolicyOpen && p.value != PolicyDeny {
			return &AuthFieldError{Field: p.field, Err: fmt.Errorf("invalid policy %q (want deny | open)", p.value)}
		}
	}
	if err := vbox.ValidateFSRules(a.FsRules); err != nil {
		return &AuthFieldError{Field: "fs_rules", Err: err}
	}
	if err := ValidateExecRules(a.ExecRules); err != nil {
		return &AuthFieldError{Field: "exec_rules", Err: err}
	}
	if err := vbox.ValidateTargetRules(a.NetRules); err != nil {
		return &AuthFieldError{Field: "net_rules", Err: err}
	}
	if a.NetUpstreamProxy != "" {
		if err := vbox.ValidateUpstreamProxy(a.NetUpstreamProxy); err != nil {
			return &AuthFieldError{Field: "net_upstream_proxy", Err: err}
		}
	}
	if err := vbox.ValidateTargetRules(a.SshRules); err != nil {
		return &AuthFieldError{Field: "ssh_rules", Err: err}
	}
	return nil
}
