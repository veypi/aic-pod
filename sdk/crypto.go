package aichost

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// deriveKeys 从 secret 和 hostID 通过 HKDF-SHA256 派生三个用途隔离的密钥。
func deriveKeys(secret, hostID string) (kConnect, kServer, kTool string, err error) {
	derive := func(info string) (string, error) {
		r := hkdf.New(sha256.New, []byte(secret), []byte(hostID), []byte(info))
		key := make([]byte, 32)
		if _, err := io.ReadFull(r, key); err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(key), nil
	}
	kConnect, err = derive("aic/host/connect/v1")
	if err != nil {
		return "", "", "", fmt.Errorf("deriving K_connect: %w", err)
	}
	kServer, err = derive("aic/host/server-proof/v1")
	if err != nil {
		return "", "", "", fmt.Errorf("deriving K_server: %w", err)
	}
	kTool, err = derive("aic/host/tool-request/v1")
	if err != nil {
		return "", "", "", fmt.Errorf("deriving K_tool: %w", err)
	}
	return
}

func hmacSHA256Raw(key, message string) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return mac.Sum(nil)
}
