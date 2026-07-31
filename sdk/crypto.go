package aichost

import (
	"crypto/hmac"
	"crypto/sha256"

	"github.com/veypi/aic-pod/sdk/proto"
)

// deriveKeys 从 secret 和 hostID 通过 HKDF-SHA256 派生三个用途隔离的密钥。
// 实现收敛在 sdk/proto（双端唯一实现）；本包装留至 sdk/host 重建后删除。
func deriveKeys(secret, hostID string) (kConnect, kServer, kTool string, err error) {
	return proto.DeriveKeys(secret, hostID)
}

func hmacSHA256Raw(key, message string) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return mac.Sum(nil)
}
