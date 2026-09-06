package host

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
)

// ssh 一级工具（独立通道，2026-09-06 三域授权模型）：
//
//		exec ssh <target> [remote command...]
//
//	  - target = [user@]host[:port] 或 ~/.ssh/config 别名；目标闸 = ssh 域
//	    Policy（ssh_policy/ssh_deny/ssh_allow）——与 net 域完全独立（ssh 免沙箱
//	    执行，net 域的沙箱网络规则本就不作用于它）。
//	  - 免沙箱内置执行：ssh 需读 ~/.ssh 密钥/config（fs_deny 0/0 不可审批绕过，
//	    沙箱化必须破例开洞反而破坏 deny 语义）；目标审批（grant ssh，level 4）
//	    即授权「用本机密钥连这台机器」。与 browser 同属 host 内部管控调用方
//	    （StartOptions.NoSandbox 合法来源）。
//	  - flag 白名单由工具固定拼装：目标前不接受任何用户 flag（-o/-L/-R/-D/-W/
//	    ProxyCommand 等可注命令/开端口的形态从根上不存在）；目标后的一切参数
//	    原样作为远端命令传递（ssh 自身语义：目标后不再解析 flag）。
//	  - 认证：设备已有密钥 / ssh agent（SSH_AUTH_SOCK 继承）/ ~/.ssh/config；
//	    host key 用 accept-new（目标审批即信任决策，首连自动记录 known_hosts）。
//	    密码形态 v1 不可用（exec 无 TTY，密码进 argv 会泄漏进日志）——
//	    报错文案引导换密钥或联系用户配置。
func (c *Client) runSSH(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse {
	if len(argv) == 0 {
		return errResp(req.MsgID, "exec ssh: target is required (usage: ssh <[user@]host[:port]|alias> [remote command...])")
	}
	target := argv[0]
	if strings.HasPrefix(target, "-") {
		return errResp(req.MsgID, "exec ssh: flags are not accepted before the target (the tool owns all flags)")
	}
	// host:port 形态：ssh CLI 不收 host:port（会当主机名解析）——剥端口转 -p
	sshTarget, portOverride := splitSSHPort(target)

	// 目标解析：ssh -G 拿生效配置（别名 → hostname/port 复用 ssh 原生逻辑，
	// 不自写 ~/.ssh/config 解析器）。pod 进程非沙箱，读 config 无阻碍。
	host, port, err := sshResolve(ctx, sshTarget, portOverride)
	if err != nil {
		return errResp(req.MsgID, "exec ssh: resolve target: "+err.Error())
	}
	if !c.sshPol.Allowed(sid, host, port) {
		e := fmt.Sprintf("%s:%d", host, port)
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateRejected,
			Error: fmt.Sprintf("ssh: target %s is not in the ssh allow list — request access via: grant ssh %s [--temp|--permanent]", e, e)}
	}

	// 工具固定 flag 拼装（不接受用户 flag）；远端命令原样续传。
	execArgv := []string{"ssh",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
	}
	if portOverride > 0 {
		execArgv = append(execArgv, "-p", strconv.Itoa(portOverride))
	}
	execArgv = append(execArgv, sshTarget)
	execArgv = append(execArgv, argv[1:]...)
	// 免沙箱（内部管控调用方）；workdir 空（远端命令的 cwd 语义在远端）。
	return c.runProcess(ctx, sid, req.MsgID, "ssh", execArgv, "", req.GrantedLevel, true)
}

// splitSSHPort 剥 host:port 形态端口（ssh CLI 不收 host:port，须转 -p）。
// user@ 前缀不影响端口识别；多冒号（IPv6）不判为端口形态。
func splitSSHPort(target string) (string, int) {
	i := strings.LastIndex(target, ":")
	if i < 0 {
		return target, 0
	}
	n, err := strconv.Atoi(target[i+1:])
	if err != nil || n < 1 || n > 65535 {
		return target, 0
	}
	head := target[:i]
	rest := head
	if j := strings.LastIndex(rest, "@"); j >= 0 {
		rest = rest[j+1:]
	}
	if strings.Contains(rest, ":") {
		return target, 0 // IPv6 多冒号形态：不判端口
	}
	return head, n
}

// sshResolve 经 ssh -G 解析目标的生效 hostname/port（10s 超时；portOverride>0
// 时经 -p 随行）。输出缺 hostname 时回落 target 去 user@ 的形态解析。
func sshResolve(ctx context.Context, target string, portOverride int) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	argv := []string{"-G"}
	if portOverride > 0 {
		argv = append(argv, "-p", strconv.Itoa(portOverride))
	}
	argv = append(argv, target)
	out, err := exec.CommandContext(cctx, "ssh", argv...).Output()
	if err != nil {
		return "", 0, fmt.Errorf("ssh -G %s: %v", target, err)
	}
	host, port := "", 22
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "hostname":
			host = v
		case "port":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				port = n
			}
		}
	}
	if host == "" {
		host = target
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		if portOverride > 0 {
			port = portOverride
		}
	}
	return host, port, nil
}
