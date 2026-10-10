package host

import (
	"errors"
	"io/fs"

	"github.com/veypi/aic-pod/protocol"
)

// fsFault 把 fsx 的文件操作错误归类为协议 Fault：文件不存在 / 权限不足是调用方
// 可自纠的预期结果，不该一律上报 internal（内部故障，字面像平台 bug）；其余
// OS 层失败归 filesystem_error。非 FS 类错误原样返回，交给 protocol.AsFault
// 判定（internal / cancelled / deadline_exceeded / Coded 自报码等）——fsx 的
// 参数/语义类错误自带 FaultCode()=invalid_argument，在 AsFault 里被采纳
// （2026-10-10）。
//
// 依赖 fsx 用 fsOpErr 保留错误链（2026-10-07 之前 fsErr 走 %s 拼字符串，
// errors.Is 断链，ENOENT 只能落到 internal）。
func fsFault(err error) error {
	var pathErr *fs.PathError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return protocol.FSFail("not_found", err.Error())
	case errors.Is(err, fs.ErrPermission):
		return protocol.FSFail("permission_denied", err.Error())
	case errors.As(err, &pathErr):
		return protocol.FSFail("filesystem_error", err.Error())
	default:
		return err
	}
}
