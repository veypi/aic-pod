package api

import (
	"io"
	"os"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/vigo"
)

// Ping 探测端点（无需凭证）。
func Ping(x *vigo.X) (string, error) {
	return "pong", nil
}

// statusView 是 get_status 的返回：运行状态 + 基本信息（hostname/host_id/version）。
type statusView struct {
	Running  bool   `json:"running"`
	HostID   string `json:"host_id"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

// GetStatus 返回运行状态与基本信息。
func GetStatus(x *vigo.X) (*statusView, error) {
	hostname, _ := os.Hostname()
	return &statusView{
		Running:  host.Running(),
		HostID:   boundHostID(effective().Key),
		Hostname: hostname,
		Version:  cfg.Version,
	}, nil
}

// LogResp 是 get_log 的返回（前端 local_handler call() 按 JSON 解析）。
type LogResp struct {
	Log string `json:"log"`
}

// logReadMax 是 get_log 读取的最大字节数（文件尾部）。
const logReadMax = 256 << 10

// GetLog 返回日志文件（cfg.LogPath()，logv 文件写入）尾部内容；文件不存在返回空串。
func GetLog(x *vigo.X) (*LogResp, error) {
	p, err := cfg.LogPath()
	if err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &LogResp{}, nil
		}
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	start := int64(0)
	if st.Size() > logReadMax {
		start = st.Size() - logReadMax
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	// 截断起点可能落在行中间：丢弃首个换行前的残段
	if start > 0 {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return &LogResp{Log: string(b)}, nil
}
