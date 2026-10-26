package host

// pullpush.go — pull/push vsh 命令（host-only 接线）：设备 ↔ 平台云盘文件传输，
// 走 /fs/cloud HTTP 面（device-key-migration：替代已删除的 NATS fetch 分块通道）。
//
// 凭证纪律：Bearer（凭证前 3 段 JWT，第 4 段 secret 不出设备）由 pod 进程内
// HTTP client 注入——不写沙箱子进程 env、不随命令参数回显。护栏：平台地址
// 非 loopback 且为 http:// 时拒绝发送凭证（防明文泄露）。
//
// 落盘/读取一律经 inv.FS（vsh 策略与 jail 自动生效）；pull 目录 = 一次
// ?depth=5 递归 JSON 列表 + 逐文件 GET（覆盖语义同 curl -o：PUT 即截断写）；
// push 目录 v1 拒绝（先打包）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/veypi/vsh/commands"
)

const (
	pullHelp = `usage: pull <云端路径> [本地路径]
  从平台云盘拉取文件/目录到本机（/fs/cloud HTTP 面，设备凭证认证）。
  <云端路径> 为 UFS 物理拼写（/u/{uid}/...、/skills/{id}/...、/public/{uid}/...）。
  [本地路径] 省略时取云端路径 basename（相对当前工作目录）。
  目录递归拉取（深度 ≤5）；覆盖语义同 curl -o。`
	pushHelp = `usage: push <本地路径> <云端路径>
  推送本机文件到平台云盘（PUT /fs/cloud{path}，设备凭证认证）。
  <云端路径> 须为本人树内目标（服务端权限门执行）。目录 v1 不支持——请先打包。`
)

// cloudFS 是 pod → 平台 /fs/cloud 面的最小 HTTP 客户端。
type cloudFS struct {
	base string // 平台地址（可带路径前缀）
	cred string // vbase 四段凭证
	http *http.Client
}

// newCloudFS 装配客户端；非 loopback 的 http:// 地址拒绝（不明文发凭证）。
func newCloudFS(platformHost, credential string) (*cloudFS, error) {
	base := strings.TrimRight(strings.TrimSpace(platformHost), "/")
	if base == "" {
		return nil, fmt.Errorf("platform host not configured")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid platform host %q", platformHost)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("refusing to send credentials over plaintext http to non-loopback host %s", u.Host)
	}
	return &cloudFS{base: base, cred: credential, http: &http.Client{Timeout: 5 * time.Minute}}, nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
}

// do 发请求并校验状态码；错误响应读前 4KB 摘要。
func (c *cloudFS) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.cred)
	// 禁带 Accept: text/html——目录须拿到 raw JSON 列表而非 SPA 壳（IsRawRequest）。
	req.Header.Set("Accept", "application/json, application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("platform %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(snippet)))
	}
	return resp, nil
}

// dirEntry 是 /fs/cloud 目录列表的条目（vigo ufs ItemEntry 子集）。
type dirEntry struct {
	Name  string     `json:"name"`
	Dir   bool       `json:"dir"`
	Size  int64      `json:"size"`
	Items []dirEntry `json:"items,omitempty"`
}

// pullFile 流式拉单文件：GET → inv.FS 截断写。
func (c *cloudFS) pullFile(ctx context.Context, inv *commands.Invocation, cloudPath, localPath string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/fs/cloud"+cloudPath, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-UFS-Dir") == "true" {
		return 0, fmt.Errorf("%s is a directory (pull directories by giving the directory path)", cloudPath)
	}
	out, err := inv.FS.OpenFile(ctx, localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	n, err := io.Copy(out, resp.Body)
	if err != nil {
		return n, fmt.Errorf("write %s: %w", localPath, err)
	}
	return n, nil
}

// pullDir 递归拉目录（一次 depth=5 列表 + 逐文件 GET）。
func (c *cloudFS) pullDir(ctx context.Context, inv *commands.Invocation, cloudPath, localRoot string) (int, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/fs/cloud"+cloudPath+"?depth=5", nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-UFS-Dir") != "true" {
		return 0, 0, fmt.Errorf("%s is not a directory", cloudPath)
	}
	var root dirEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&root); err != nil {
		return 0, 0, fmt.Errorf("parse directory listing: %w", err)
	}
	if err := inv.FS.MkdirAll(ctx, localRoot, 0o755); err != nil {
		return 0, 0, err
	}
	files, bytes := 0, int64(0)
	var walk func(entry *dirEntry, cloudDir, localDir string) error
	walk = func(entry *dirEntry, cloudDir, localDir string) error {
		for i := range entry.Items {
			item := &entry.Items[i]
			cp := path.Join(cloudDir, item.Name)
			lp := path.Join(localDir, item.Name)
			if item.Dir {
				if err := inv.FS.MkdirAll(ctx, lp, 0o755); err != nil {
					return err
				}
				if err := walk(item, cp, lp); err != nil {
					return err
				}
				continue
			}
			n, err := c.pullFile(ctx, inv, cp, lp)
			if err != nil {
				return err
			}
			files++
			bytes += n
		}
		return nil
	}
	if err := walk(&root, cloudPath, localRoot); err != nil {
		return files, bytes, err
	}
	return files, bytes, nil
}

// pushFile 流式推单文件：inv.FS 读 → PUT（Content-Length 已知 → 服务端配额预检生效）。
func (c *cloudFS) pushFile(ctx context.Context, inv *commands.Invocation, localPath, cloudPath string) (int64, error) {
	file, err := inv.FS.Open(ctx, localPath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if stat.IsDir() {
		return 0, fmt.Errorf("%s is a directory (pack it first: push supports files only)", localPath)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/fs/cloud"+cloudPath, file)
	if err != nil {
		return 0, err
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return stat.Size(), nil
}

// cloudPathArg 校验云端路径参数形态（绝对路径；归属/范围由服务端门禁执行）。
func cloudPathArg(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("cloud path must be absolute (got %q)", p)
	}
	return path.Clean(p), nil
}

// cmdPull 是 `pull` 命令执行体（host 接线）。
func (c *Client) cmdPull(ctx context.Context, inv *commands.Invocation) error {
	if len(inv.Args) == 0 || inv.Args[0] == "--help" || inv.Args[0] == "-h" {
		fmt.Fprintln(inv.Stdout, pullHelp)
		return nil
	}
	remote, err := cloudPathArg(inv.Args[0])
	if err != nil {
		return commands.Exitf(inv, 2, "pull: %v", err)
	}
	cfs, err := newCloudFS(c.options().Host, c.authToken)
	if err != nil {
		return commands.Exitf(inv, 2, "pull: %v", err)
	}

	// 目录/文件分流：先探一次（目录 GET 是 JSON 列表，文件直出字节）。
	local := ""
	if len(inv.Args) > 1 {
		local = inv.Args[1]
	}
	if strings.HasSuffix(inv.Args[0], "/") {
		// 显式目录形态
		if local == "" {
			local = path.Base(path.Dir(remote))
		}
		files, bytes, err := cfs.pullDir(ctx, inv, remote, local)
		if err != nil {
			return commands.Exitf(inv, 1, "pull: %v", err)
		}
		fmt.Fprintf(inv.Stdout, "pulled %d files, %d bytes → %s\n", files, bytes, local)
		return nil
	}
	// 探测类型：HEAD 不可靠（部分面未挂），用一次 GET 判 X-UFS-Dir。
	if local == "" {
		local = path.Base(remote)
	}
	n, err := cfs.pullFile(ctx, inv, remote, local)
	if err == nil {
		fmt.Fprintf(inv.Stdout, "pulled %d bytes → %s\n", n, local)
		return nil
	}
	if !strings.Contains(err.Error(), "is a directory") {
		return commands.Exitf(inv, 1, "pull: %v", err)
	}
	// 是目录：递归拉取（local 作为目录名）
	files, bytes, err := cfs.pullDir(ctx, inv, remote, local)
	if err != nil {
		return commands.Exitf(inv, 1, "pull: %v", err)
	}
	fmt.Fprintf(inv.Stdout, "pulled %d files, %d bytes → %s\n", files, bytes, local)
	return nil
}

// cmdPush 是 `push` 命令执行体（host 接线）。
func (c *Client) cmdPush(ctx context.Context, inv *commands.Invocation) error {
	if len(inv.Args) == 0 || inv.Args[0] == "--help" || inv.Args[0] == "-h" {
		fmt.Fprintln(inv.Stdout, pushHelp)
		return nil
	}
	if len(inv.Args) < 2 {
		return commands.Exitf(inv, 2, "push: <本地路径> <云端路径> both required")
	}
	remote, err := cloudPathArg(inv.Args[1])
	if err != nil {
		return commands.Exitf(inv, 2, "push: %v", err)
	}
	cfs, err := newCloudFS(c.options().Host, c.authToken)
	if err != nil {
		return commands.Exitf(inv, 2, "push: %v", err)
	}
	n, err := cfs.pushFile(ctx, inv, inv.Args[0], remote)
	if err != nil {
		return commands.Exitf(inv, 1, "push: %v", err)
	}
	fmt.Fprintf(inv.Stdout, "pushed %d bytes → %s\n", n, remote)
	return nil
}
