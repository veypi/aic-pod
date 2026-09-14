//go:build !darwin && !linux

package fsauth

// systemCAPaths（其他平台）：windows 的沙箱 deny 隔离为 no-op（受限令牌
// 模型），无系统 CA 放行需求。
func systemCAPaths() []string {
	return nil
}
