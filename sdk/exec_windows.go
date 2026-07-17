//go:build windows

package aicenv

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {}
