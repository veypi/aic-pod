package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// 设备凭证（device-key-migration 2026-10-12）：pod 配置串 = 复合形态
// `<host_id>.<vbase 四段 credential>`（vbase credential = JWT.client_secret）。
//
//   - 前 3 段 JWT 是认证凭据（NATS CONNECT token / HTTP Bearer 原样使用）；
//   - 第 4 段 client_secret 是本地 HKDF 根（K_tool 签名 / RTC 票据验证）；
//   - JWT payload 携带 uid 与 key 代次（ver），pod 本地解码（不验签——签名
//     密钥在平台，篡改 payload 会在平台侧认证失败），仅用于 subject 构造。
//
// 复合串只比 credential 多一个 host_id 前缀段，切割点在第一个 '.'。

// ComposeDeviceCredential 组合 pod 配置凭证串（平台 hosts 创建/重置时返回，
// 仅此一次可见）。
func ComposeDeviceCredential(hostID, credential string) string {
	return hostID + "." + credential
}

// DeviceCredential 是 ParseDeviceCredential 的解析结果。
type DeviceCredential struct {
	HostID string // 复合串首段
	UID    string // JWT payload uid
	Ver    uint64 // JWT payload ver（凭证代次，subject 代次段）
	Secret string // 第 4 段 client_secret（HKDF 根）
	// Credential 是原样四段凭证（NATS CONNECT token / HTTP Bearer 用它）。
	Credential string
}

// ParseDeviceCredential 解析复合凭证串；不验签（见上）。各段合法性在此
//
//	fail fast，连接期不再重复解析。
func ParseDeviceCredential(composite string) (*DeviceCredential, error) {
	hostID, credential, ok := strings.Cut(strings.TrimSpace(composite), ".")
	if !ok || !validSeg(hostID) {
		return nil, fmt.Errorf("proto: invalid device credential (want <host_id>.<credential>)")
	}
	parts := strings.Split(credential, ".")
	if len(parts) != 4 || parts[3] == "" {
		return nil, fmt.Errorf("proto: invalid device credential (want 4-segment vbase credential)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("proto: device credential payload: %w", err)
	}
	var claims struct {
		UID     string `json:"uid"`
		Subject string `json:"sub"`
		Version uint64 `json:"ver"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("proto: device credential payload: %w", err)
	}
	uid := claims.UID
	if uid == "" {
		uid = claims.Subject
	}
	if !validSeg(uid) || claims.Version == 0 {
		return nil, fmt.Errorf("proto: device credential payload missing uid/ver")
	}
	if claims.Type != "" && claims.Type != "static" {
		return nil, fmt.Errorf("proto: device credential is not a static key (type=%q)", claims.Type)
	}
	return &DeviceCredential{
		HostID: hostID, UID: uid, Ver: claims.Version,
		Secret: parts[3], Credential: credential,
	}, nil
}
