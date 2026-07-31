package proto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// HKDF info 串：用途隔离的密钥派生。
// K_connect — 连接 proof；K_server — 服务端 challenge；K_tool — 工具请求签名。
const (
	hkdfInfoConnect = "aic/host/connect/v1"
	hkdfInfoServer  = "aic/host/server-proof/v1"
	hkdfInfoTool    = "aic/host/tool-request/v1"
)

// DeriveKeys 从 host secret 派生三个用途隔离的 HMAC key
// （HKDF-SHA256，salt = host_id，输出 base64url 编码的 32 字节）。
func DeriveKeys(secret, hostID string) (kConnect, kServer, kTool string, err error) {
	if secret == "" || hostID == "" {
		return "", "", "", fmt.Errorf("proto: secret and host_id are required")
	}
	derive := func(info string) (string, error) {
		r := hkdf.New(sha256.New, []byte(secret), []byte(hostID), []byte(info))
		key := make([]byte, 32)
		if _, err := io.ReadFull(r, key); err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(key), nil
	}
	if kConnect, err = derive(hkdfInfoConnect); err != nil {
		return "", "", "", fmt.Errorf("proto: derive K_connect: %w", err)
	}
	if kServer, err = derive(hkdfInfoServer); err != nil {
		return "", "", "", fmt.Errorf("proto: derive K_server: %w", err)
	}
	if kTool, err = derive(hkdfInfoTool); err != nil {
		return "", "", "", fmt.Errorf("proto: derive K_tool: %w", err)
	}
	return
}

// HMACSHA256 返回 HMAC-SHA256(key, msg) 原始字节。
func HMACSHA256(key, msg string) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}

// NewNonce 生成 16 字节随机 nonce（base64url）。
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("proto: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ---- 工具请求签名（§6.2） ----

// toolReqSigPayload 是工具请求签名的 canonical 输入。
// 双端必须位级一致：JSON 字段序即签名输入序，禁止各自另写。
type toolReqSigPayload struct {
	Version      int    `json:"version"`
	HostID       string `json:"host_id"`
	MsgID        string `json:"msg_id"`
	SessionID    string `json:"session_id"`
	Tool         string `json:"tool"`
	DataSHA256   string `json:"data_sha256"`
	GrantedLevel int    `json:"granted_level"`
	Nonce        string `json:"nonce"`
	Deadline     string `json:"deadline"`
}

// CanonicalToolReqSigInput 构造签名 canonical 输入：
// 覆盖 host_id/msg_id/session_id/tool/data_hash/granted_level/nonce/deadline（§6.2）。
func CanonicalToolReqSigInput(hostID string, req *ToolRequest) string {
	sum := sha256.Sum256(req.Data)
	b, _ := json.Marshal(toolReqSigPayload{
		Version:      2,
		HostID:       hostID,
		MsgID:        req.MsgID,
		SessionID:    req.SessionID,
		Tool:         req.Tool,
		DataSHA256:   fmt.Sprintf("%x", sum),
		GrantedLevel: req.GrantedLevel,
		Nonce:        req.Nonce,
		Deadline:     req.Deadline,
	})
	return string(b)
}

// SignToolRequest 计算并填入 req.Sig（HMAC-SHA256，K_tool）。
func SignToolRequest(req *ToolRequest, hostID, kTool string) {
	req.Sig = base64.RawURLEncoding.EncodeToString(HMACSHA256(kTool, CanonicalToolReqSigInput(hostID, req)))
}

// VerifyToolRequest 校验请求签名（host 端验证规范第 1 步，§6.2）。
func VerifyToolRequest(req *ToolRequest, hostID, kTool string) bool {
	expected := base64.RawURLEncoding.EncodeToString(HMACSHA256(kTool, CanonicalToolReqSigInput(hostID, req)))
	return hmac.Equal([]byte(req.Sig), []byte(expected))
}

// ---- 连接 token（host 端连接认证；server natsauth 用同一 canonical 输入验签） ----

const connectTokenDomain = "aic-host-connect-v1"

// connectSigPayload 是连接 token 签名的 canonical 输入。
type connectSigPayload struct {
	Domain   string `json:"domain"`
	HostID   string `json:"host_id"`
	UID      string `json:"uid"`
	HostInfo string `json:"host_info"`
	UnixMS   int64  `json:"unix_ms"`
	Nonce    string `json:"nonce"`
}

// CanonicalConnectSigInput 构造连接 token 的签名 canonical 输入。
// envInfo 为 host_info 的原始 JSON 文本（不是 base64）。
func CanonicalConnectSigInput(hostID, uid, envInfo string, unixMS int64, nonce string) string {
	b, _ := json.Marshal(connectSigPayload{
		Domain:   connectTokenDomain,
		HostID:   hostID,
		UID:      uid,
		HostInfo: envInfo,
		UnixMS:   unixMS,
		Nonce:    nonce,
	})
	return string(b)
}

// ConnectToken 是解析后的连接 token：e1.{host_id}.{env_info_b64}.{unix_ms}.{nonce}.{sig}
type ConnectToken struct {
	HostID  string
	EnvInfo string // host_info 原始 JSON 文本
	UnixMS  int64
	Nonce   string
	Sig     string
}

// GenerateConnectToken 生成连接 token（host 端）。
// envInfo 携带 agent_version/device_type/device_name（§6.3 版本门禁与一级字段上报）。
func GenerateConnectToken(hostID, uid, agentVersion, deviceType, deviceName string, unixMS int64, nonce, kConnect string) string {
	envInfo, _ := json.Marshal(map[string]string{
		"agent_version": agentVersion,
		"device_type":   deviceType,
		"device_name":   deviceName,
	})
	sig := base64.RawURLEncoding.EncodeToString(
		HMACSHA256(kConnect, CanonicalConnectSigInput(hostID, uid, string(envInfo), unixMS, nonce)))
	return fmt.Sprintf("e1.%s.%s.%d.%s.%s",
		hostID,
		base64.RawURLEncoding.EncodeToString(envInfo),
		unixMS, nonce, sig)
}

// ParseConnectToken 解析连接 token（server natsauth）。不验签。
func ParseConnectToken(token string) (*ConnectToken, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 6 || parts[0] != "e1" {
		return nil, fmt.Errorf("proto: invalid connect token format")
	}
	envInfoRaw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("proto: invalid connect token env_info: %w", err)
	}
	v := &ConnectToken{HostID: parts[1], EnvInfo: string(envInfoRaw), Nonce: parts[4], Sig: parts[5]}
	if v.UnixMS, err = strconv.ParseInt(parts[3], 10, 64); err != nil {
		return nil, fmt.Errorf("proto: invalid connect token unix_ms: %w", err)
	}
	return v, nil
}

// HostConnectInfo 是连接 token 中 host_info 的标准结构（§6.3）：
// agent_version 用于主版本门禁，device_type/device_name 连接成功后写入 host 一级字段。
type HostConnectInfo struct {
	AgentVersion string `json:"agent_version"`
	DeviceType   string `json:"device_type,omitempty"`
	DeviceName   string `json:"device_name,omitempty"`
}
