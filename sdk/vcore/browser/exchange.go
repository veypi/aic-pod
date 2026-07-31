package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veypi/aic-pod/sdk/proto"
	"github.com/veypi/aic-pod/sdk/vcore"
)

// 文件交换（§5.6 cloud 约束，走 VFS 接口不新增适配点）：
//   - upload：源文件限 VFS 路径空间内（路径展开 + 越界拒绝由调用方鉴权策略保证），
//     读内容暂存临时目录后上传；
//   - download / screenshot：CLI 落临时目录，字节经 VFS 写入目标路径
//     （cloud = $SESSION 根约束由 env.Workdir/Vars 与调用方根收容保证）。

// upload <sel> <file...>：源文件经 VFS 读（§5.6），Danger(3) 由分级表控制。
func (b *Browser) upload(ctx context.Context, env *vcore.Env, args []string) (*vcore.Result, error) {
	if len(args) < 2 {
		return nil, &proto.ExecError{Action: "browser", Reason: "upload requires selector and at least one file path"}
	}
	sel := args[0]
	tmpDir, err := os.MkdirTemp(b.cfg.TempDir, "upload-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	osPaths := make([]string, 0, len(args)-1)
	for i, raw := range args[1:] {
		abs, err := env.Resolve(raw)
		if err != nil {
			return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("upload: %s", err)}
		}
		data, err := env.VFS.ReadFile(abs)
		if err != nil {
			return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("upload: cannot read %s: %s", abs, err)}
		}
		tmp := filepath.Join(tmpDir, fmt.Sprintf("%d-%s", i, filepath.Base(abs)))
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return nil, err
		}
		osPaths = append(osPaths, tmp)
	}

	if _, err := b.execCLI(ctx, nil, append([]string{"upload", sel}, osPaths...)...); err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("upload: %s", err)}
	}
	b.markDirty()
	r := &vcore.Result{Attrs: map[string]string{"action": "browser"}}
	r.Content = fmt.Sprintf("uploaded %d file(s) to %s", len(osPaths), sel)
	return r, nil
}

// download <sel> <path>：落盘路径经 VFS 写（§5.6：cloud 限 $SESSION 根内，
// 由 env 路径空间保证）；Attrs path 为 VFS 路径。
func (b *Browser) download(ctx context.Context, env *vcore.Env, args []string) (*vcore.Result, error) {
	if len(args) < 2 {
		return nil, &proto.ExecError{Action: "browser", Reason: "download requires selector and path"}
	}
	sel := args[0]
	abs, err := env.Resolve(args[1])
	if err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("download: %s", err)}
	}
	tmpDir, err := os.MkdirTemp(b.cfg.TempDir, "download-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, filepath.Base(abs))

	if _, err := b.execCLI(ctx, nil, "download", sel, tmpPath); err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("download: %s", err)}
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("download: %s", err)}
	}
	if err := env.VFS.MkdirAll(dirOfVFS(abs)); err != nil {
		return nil, err
	}
	if err := env.VFS.WriteFile(abs, data); err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("download: %s", err)}
	}
	b.markDirty()
	r := &vcore.Result{Attrs: map[string]string{"action": "browser", "path": abs}}
	r.Content = fmt.Sprintf("✓ Downloaded: %s", abs)
	return r, nil
}

// screenshot [--quality N] [--full]：字节经 VFS 写 $SESSION/screenshot/{ts}.jpg
// 后返回 image_path（cloud）；host/page 由上层按 §2.2 图片标准另行转换。
func (b *Browser) screenshot(ctx context.Context, env *vcore.Env, args []string) (*vcore.Result, error) {
	quality := "80"
	var passthrough []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--quality" && i+1 < len(args) {
			quality = args[i+1]
			i++
			continue
		}
		passthrough = append(passthrough, args[i])
	}

	if err := os.MkdirAll(b.cfg.TempDir, 0o700); err != nil {
		return nil, err
	}
	tmpPath := filepath.Join(b.cfg.TempDir, fmt.Sprintf("screenshot-%d.jpg", time.Now().UnixNano()))
	defer os.Remove(tmpPath)

	cliArgs := append([]string{"screenshot", "--screenshot-format", "jpeg", "--screenshot-quality", quality}, passthrough...)
	cliArgs = append(cliArgs, tmpPath)
	if _, err := b.execCLI(ctx, nil, cliArgs...); err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("screenshot: %s", err)}
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("screenshot: %s", err)}
	}

	abs, err := env.Resolve(fmt.Sprintf("screenshot/%s.jpg", time.Now().Format("2006-01-02T15-04-05")))
	if err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("screenshot: %s", err)}
	}
	if err := env.VFS.MkdirAll(dirOfVFS(abs)); err != nil {
		return nil, err
	}
	if err := env.VFS.WriteFile(abs, data); err != nil {
		return nil, &proto.ExecError{Action: "browser", Reason: fmt.Sprintf("screenshot: %s", err)}
	}
	b.markDirty()
	r := &vcore.Result{Attrs: map[string]string{"action": "browser", "image_path": abs}}
	r.Content = fmt.Sprintf("✓ Screenshot saved to %s", abs)
	return r, nil
}

func dirOfVFS(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}
