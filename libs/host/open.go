package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/veypi/aic-pod/libs/execution"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

const openStdinLimit = 32 * 1024

// registerOpen registers the platform virtual command before package commands.
// The operating system chooses the native default application; no shell is used.
func (c *Client) registerOpen(reg *commands.Registry) error {
	return reg.RegisterGuarded(commands.DefineCommand("open", c.runOpen))
}

func (c *Client) runOpen(ctx context.Context, inv *commands.Invocation) error {
	args := inv.Args
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(inv.Stdout, "usage: open <path> | open --stdin")
		return nil
	}
	var name string
	if len(args) == 1 && args[0] == "--stdin" {
		data, err := io.ReadAll(io.LimitReader(inv.Stdin, openStdinLimit+1))
		if err != nil {
			return commands.Exitf(inv, 1, "open: %v", err)
		}
		if len(data) > openStdinLimit {
			return commands.Exitf(inv, 2, "open: stdin path exceeds %d bytes", openStdinLimit)
		}
		name = strings.TrimRight(string(data), "\r\n")
	} else {
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 1 {
			name = args[0]
		}
	}
	if name == "" {
		return commands.Exitf(inv, 2, "usage: open <path> | open --stdin")
	}

	abs := inv.FS.Resolve(name)
	if !c.perms.fsSnapshot(execution.SessionFromContext(ctx)).Match(abs, vbox.OpRead).Allow {
		return commands.Exitf(inv, 1, "open: %s: read denied by fs rules", abs)
	}
	native := vbox.HostPathToOS(abs)
	if _, err := os.Stat(native); err != nil {
		return commands.Exitf(inv, 1, "open: %v", err)
	}
	run := c.openRun
	if run == nil {
		run = nativeOpen
	}
	if err := run(ctx, native); err != nil {
		return commands.Exitf(inv, 1, "open: %v", err)
	}
	fmt.Fprintf(inv.Stdout, "opened %s\n", abs)
	return nil
}

func nativeOpenSpec(goos, target string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "/usr/bin/open", []string{target}, nil
	case "windows":
		return "rundll32.exe", []string{"url.dll,FileProtocolHandler", target}, nil
	case "linux":
		return "xdg-open", []string{target}, nil
	default:
		return "", nil, fmt.Errorf("open: unsupported operating system %s", goos)
	}
}

func nativeOpen(ctx context.Context, target string) error {
	name, args, err := nativeOpenSpec(runtime.GOOS, target)
	if err != nil {
		return err
	}
	return exec.CommandContext(ctx, name, args...).Run()
}
