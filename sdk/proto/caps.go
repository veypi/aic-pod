package proto

import (
	"fmt"
	"regexp"
	"strconv"
)

// AllFSActions 是 fs 的全部 action（§4）：fs.actions 为 null/未声明时的默认全集。
var AllFSActions = []string{"read", "write", "edit"}

// Caps 是 host 能力声明（§6.3 caps v2），随 host 每次连接/重连发布，
// 服务端以最近一次为准；host 离线即不可用，不依据过期 caps 转发。
type Caps struct {
	HostID        string      `json:"host_id"`
	CredentialVer uint64      `json:"credential_ver"`
	AgentVersion  string      `json:"agent_version"`
	DeviceType    string      `json:"device_type,omitempty"`
	Hostname      string      `json:"hostname,omitempty"`
	DeviceInfo    *DeviceInfo `json:"device_info,omitempty"`
	FS            FSCaps      `json:"fs"`
	Exec          ExecCaps    `json:"exec"`
}

// DeviceInfo 是 host 设备信息（连接时上报，用户不可编辑）。
type DeviceInfo struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	NumCPU int    `json:"num_cpu"`
}

// FSCaps 声明 fs 能力。Actions 为指针以严格区分三形态（§6.3）：
// nil（null/未声明）= 全部 3 个 action；空数组 = 不支持 fs。
type FSCaps struct {
	Actions *[]string `json:"actions"`
}

// EffectiveActions 返回有效 fs action 集（nil → 全集）。
func (c FSCaps) EffectiveActions() []string {
	if c.Actions == nil {
		return AllFSActions
	}
	return *c.Actions
}

// Supports 判定 fs action 是否可用。
func (c FSCaps) Supports(action string) bool {
	for _, a := range c.EffectiveActions() {
		if a == action {
			return true
		}
	}
	return false
}

// ExecCaps 声明 exec 能力（§6.3）。
type ExecCaps struct {
	// Programs 为指针以严格区分两形态：nil（null/未声明）= 不限制
	// （host 端按 PATH 查找自检）；空数组 = 无程序（browser 类 host 的
	// 纯虚拟环境必须显式声明空数组）。
	Programs *[]string     `json:"programs"`
	Virtual  []VirtualDecl `json:"virtual,omitempty"`
}

// ProgramsUnrestricted 报告 programs 是否为「不限制」语义。
func (c ExecCaps) ProgramsUnrestricted() bool { return c.Programs == nil }

// VirtualDecl 是虚拟指令声明（§6.3）。
// 物理 host 必备 = 核心 8（虚拟包装）+ commands + bg_*。
type VirtualDecl struct {
	Name           string `json:"name"`
	RequiredLevel  int    `json:"required_level"`
	Stateful       bool   `json:"stateful,omitempty"`
	Backgroundable bool   `json:"backgroundable,omitempty"`
}

// ---- 客户端版本门禁（§6.3） ----

var agentVersionRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?$`)

// ParseAgentVersion 解析 va.b.c（允许 -prerelease 后缀）为 (major, minor, patch)。
func ParseAgentVersion(v string) (major, minor, patch int, err error) {
	m := agentVersionRe.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("proto: invalid agent_version %q (want v{a}.{b}.{c}, optional -prerelease)", v)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	return
}

// MajorVersionMatch 报告两个 agent_version 主版本号是否一致。
// 连接认证时校验，不一致即拒绝连接（§6.3：协议不兼容的 client/server
// 组合在连接期即暴露，而不是运行期出现诡谲行为）。
func MajorVersionMatch(client, server string) bool {
	ca, _, _, err1 := ParseAgentVersion(client)
	sa, _, _, err2 := ParseAgentVersion(server)
	return err1 == nil && err2 == nil && ca == sa
}
