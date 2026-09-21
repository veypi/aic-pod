package host

import (
	"fmt"
	"strings"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/hostfs"
)

// optionsOf 将配置转换为 host 客户端 Options（解析 ExecTimeout）。
// deviceType 由调用方指定（cli/desktop），version 为客户端版本（va.b.c）。
func optionsOf(o cfg.Options, deviceType, version string, onLog func(string, ...any)) (Options, error) {
	if o.HostsUploadBytes < 0 || o.HostsProxyUploadBytes < 0 || o.HostsSources < 0 {
		return Options{
			BrowserPath: o.BrowserPath, BrowserWidth: o.BrowserWidth, BrowserHeight: o.BrowserHeight}, fmt.Errorf("hosts transfer budgets must be nonnegative")
	}
	timeout := 30 * time.Minute
	if s := strings.TrimSpace(o.ExecTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return Options{
				BrowserPath: o.BrowserPath, BrowserWidth: o.BrowserWidth, BrowserHeight: o.BrowserHeight}, fmt.Errorf("invalid exec_timeout %q: %w", s, err)
		}
		timeout = d
	}
	return Options{
		BrowserPath: o.BrowserPath, BrowserWidth: o.BrowserWidth, BrowserHeight: o.BrowserHeight,
		Transfers:   hostfs.TransferConfig{MaxUploadBytes: o.HostsUploadBytes, ProxyUploadBytes: o.HostsProxyUploadBytes, MaxSources: o.HostsSources},
		Host:        o.Host,
		Key:         o.Key,
		WorkDir:     o.WorkDir,
		DeviceType:  deviceType,
		Version:     version,
		ExecTimeout: timeout,
		NoSandbox:   o.NoSandbox,
		RTC:         o.RTC,
		OnLog:       onLog,
	}, nil
}
