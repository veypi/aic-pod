package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// HKDF info 串：用途隔离的密钥派生。
// K_tool — 工具请求签名（device-key-migration 后唯一存活用途；
// K_connect/K_server 随 e1. 自签体系于 2026-10-12 退役删除）。
// 派生参数（info/salt/输出编码）是协议常量：改动 = 协议变更（sign_vectors_test.go 锁定）。
const hkdfInfoTool = "aic/host/tool-request/v1"

// DeriveKTool 从设备凭证 secret（vbase credential 第 4 段）派生工具请求签名 key
// （HKDF-SHA256，salt = host_id，输出 base64url 编码的 32 字节）。
func DeriveKTool(secret, hostID string) (string, error) {
	if secret == "" || hostID == "" {
		return "", fmt.Errorf("proto: secret and host_id are required")
	}
	r := hkdf.New(sha256.New, []byte(secret), []byte(hostID), []byte(hkdfInfoTool))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}

// HMACSHA256 返回 HMAC-SHA256(key, msg) 原始字节。
func HMACSHA256(key, msg string) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}
