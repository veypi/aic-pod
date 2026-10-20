package host

import (
	"errors"
	"io/fs"

	"github.com/veypi/aic-pod/protocol"
)

// fsFault 把 fsx 的文件操作错误归类为协议 Fault：文件不存在 / 权限不足是调用方
// 可自纠的预期结果，不该一律上报 internal（内部故障，字面像平台 bug）；其余
// OS 层失败归 filesystem_error。非 FS 类错误原样返回，交给 protocol.AsFault
// 判定（internal / cancelled / deadline_exceeded / Coded 自报码等）。
//
// 公共归类（not_found / already_exists / permission_denied）由
// protocol.FSFault 单点提供；本函数只保留 host 侧兜底口径。
func fsFault(err error) error {
	if err == nil {
		return nil
	}
	if fault, ok := protocol.FSFault(err); ok {
		return fault
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return protocol.FSFail("filesystem_error", err.Error())
	}
	return err
}
