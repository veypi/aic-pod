package host

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/veypi/aic-pod/libs/proto"
)

// scp 一级工具（独立通道，2026-09-07 三域授权模型）：
//
//		exec scp [-r] [-p] [-q] [-P port] <source> <target>
//
//	  - 恰好一端为远端（[user@]host:path 或 ~/.ssh/config 别名）；远端↔远端
//	    v1 不支持（经本机两步拷贝），双本地请用 cp。
//	  - 远端目标闸 = ssh 域 Policy（与 ssh 工具完全同一套：ssh_policy/ssh_deny/
//	    ssh_allow，grant ssh host:port 申请）；经 ssh -G 解析别名与生效端口。
//	  - 本地侧 fs 门控：本地操作数过 fsauth 判定（deny 0/0 拒绝）——scp 免沙箱
//	    内置执行（读 ~/.ssh 密钥/config，同 ssh 理由），这层工具侧判定是对
//	    本地文件系统的必要补偿。写等级不另查：命令 base = Danger(3)，能执行
//	    即 granted>=3，已覆盖 fsauth 写等级上限（2/3）。
//	  - flag 白名单：-r（递归）/-p（保时间戳）/-q/-P <port>（scp 远端语法
//	    host:path 无法携带端口，-P 是唯一端口通道且结果同样过 ssh 域闸）；
//	    -F/-o/-S/-J/-i/-c/-l 等可注命令/换密钥/绕 config 的形态从根上不存在。
//	  - 认证与 host key 策略同 ssh（密钥/agent/config + accept-new；-B 批模式
//	    禁密码提示，v1 密码形态不可用）。
func (c *Client) runSCP(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse {
	a, err := parseSCPArgv(argv)
	if err != nil {
		return errResp(req.MsgID, "exec scp: "+err.Error())
	}
	srcRemote, srcHead, _ := splitSCPOperand(a.src)
	dstRemote, dstHead, _ := splitSCPOperand(a.dst)
	if srcRemote == dstRemote {
		if srcRemote {
			return errResp(req.MsgID, "exec scp: remote-to-remote copy is not supported (copy via this host in two steps)")
		}
		return errResp(req.MsgID, "exec scp: both operands are local (use cp)")
	}
	head := srcHead
	local, write := a.dst, true // 下载：远端 → 本地写
	if dstRemote {
		head = dstHead
		local, write = a.src, false // 上传：本地读 → 远端
	}

	// 远端目标解析：ssh -G 生效配置（别名/端口；user@ 随行），ssh 域目标闸。
	host, port, err := sshResolve(ctx, head, a.port)
	if err != nil {
		return errResp(req.MsgID, "exec scp: resolve target: "+err.Error())
	}
	if !c.sshPol.Allowed(sid, host, port) {
		e := fmt.Sprintf("%s:%d", host, port)
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateRejected,
			Error: fmt.Sprintf("scp: target %s is not in the ssh allow list — request access via: grant ssh %s [--temp|--permanent]", e, e)}
	}

	// 本地侧 fs 门控（deny 0/0 拒绝；免沙箱执行的工具侧补偿）。本地操作数
	// 解析为绝对路径后回写 argv——相对路径不再依赖 pod 进程 cwd。
	abs, err := filepath.Abs(expandHomeDir(local))
	if err != nil {
		return errResp(req.MsgID, fmt.Sprintf("exec scp: invalid local path %q: %v", local, err))
	}
	rd, wr := c.policy.View(sid).Decide(abs)
	if (write && wr == 0) || (!write && rd == 0) {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateRejected,
			Error: fmt.Sprintf("scp: local path %s is in the fs_deny list (no read/write)", abs)}
	}
	src, dst := a.src, a.dst
	if write {
		dst = abs
	} else {
		src = abs
	}

	// 工具固定 flag 拼装 + 白名单用户 flag；操作数收尾。
	execArgv := []string{"scp", "-B",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
	}
	if a.port > 0 {
		execArgv = append(execArgv, "-P", strconv.Itoa(a.port))
	}
	if a.recursive {
		execArgv = append(execArgv, "-r")
	}
	if a.preserve {
		execArgv = append(execArgv, "-p")
	}
	if a.quiet {
		execArgv = append(execArgv, "-q")
	}
	execArgv = append(execArgv, src, dst)
	// 免沙箱（内部管控调用方，同 ssh）；workdir 空（本地侧已绝对化）。
	return c.runProcess(ctx, sid, req.MsgID, "scp", execArgv, "", req.GrantedLevel, true)
}

// scpArgs 是 parseSCPArgv 的产物（白名单 flag + 两操作数）。
type scpArgs struct {
	recursive bool
	preserve  bool
	quiet     bool
	port      int
	src, dst  string
}

// parseSCPArgv 解析 scp 参数：flag 白名单 -r/-p/-q/-P（仅操作数前；
// -P 支持分离与粘连两形态），其余 flag 一律拒绝；操作数恰好两个。
func parseSCPArgv(argv []string) (scpArgs, error) {
	var a scpArgs
	i := 0
	for ; i < len(argv); i++ {
		s := argv[i]
		if !strings.HasPrefix(s, "-") || s == "-" {
			break
		}
		switch {
		case s == "-r":
			a.recursive = true
		case s == "-p":
			a.preserve = true
		case s == "-q":
			a.quiet = true
		case s == "-P":
			i++
			if i >= len(argv) {
				return a, fmt.Errorf("-P requires a port value")
			}
			n, err := strconv.Atoi(argv[i])
			if err != nil || n < 1 || n > 65535 {
				return a, fmt.Errorf("invalid -P port %q (want 1-65535)", argv[i])
			}
			a.port = n
		case strings.HasPrefix(s, "-P"):
			n, err := strconv.Atoi(s[2:])
			if err != nil || n < 1 || n > 65535 {
				return a, fmt.Errorf("invalid -P port %q (want 1-65535)", s[2:])
			}
			a.port = n
		default:
			return a, fmt.Errorf("flag %q is not accepted (whitelist: -r, -p, -q, -P; the tool owns everything else)", s)
		}
	}
	if rest := argv[i:]; len(rest) != 2 {
		return a, fmt.Errorf("usage: scp [-r] [-p] [-q] [-P port] <source> <target> (exactly one side remote, [user@]host:path)")
	} else {
		a.src, a.dst = rest[0], rest[1]
	}
	return a, nil
}

// splitSCPOperand 判定操作数本地/远端；远端返回 [user@]host 头与路径部分。
// scp 语义：首个冒号出现在首个斜杠之前 = 远端；Windows 盘符形态（C:\、C:/）
// 与冒号在斜杠之后（/a:b/c、./x:y）判本地。
func splitSCPOperand(op string) (remote bool, head, path string) {
	if len(op) >= 3 && op[1] == ':' && (op[2] == '\\' || op[2] == '/') &&
		(op[0] >= 'a' && op[0] <= 'z' || op[0] >= 'A' && op[0] <= 'Z') {
		return false, "", ""
	}
	ci := strings.IndexByte(op, ':')
	si := strings.IndexByte(op, '/')
	if ci > 0 && (si < 0 || ci < si) {
		return true, op[:ci], op[ci+1:]
	}
	return false, "", ""
}
