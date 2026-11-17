package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	SessionConnectScheme       = "s1"
	MaxSessionConnectTokenSize = 4096
)

// SessionConnectToken 是 s1.<host_id>.<credential_ver>.<web_access_token> 的解析结果。
// AccessToken 保留完整三段 JWT；protocol secret 不属于 CONNECT envelope。
type SessionConnectToken struct {
	HostID        string
	CredentialVer uint64
	AccessToken   string
}

// ComposeSessionConnectToken 组装 Session Host 的 NATS CONNECT token。
// 这里只做本地形态校验；签名、有效期、dynamic + inherit 和 Host 归属由服务端验证。
func ComposeSessionConnectToken(hostID string, credentialVer uint64, accessToken string) (string, error) {
	hostID = strings.TrimSpace(hostID)
	accessToken = strings.TrimSpace(accessToken)
	if !ValidHostID(hostID) {
		return "", fmt.Errorf("proto: invalid session host id")
	}
	if credentialVer == 0 {
		return "", fmt.Errorf("proto: invalid session credential generation")
	}
	if _, err := SessionAccessTokenUID(accessToken); err != nil {
		return "", err
	}
	token := SessionConnectScheme + "." + hostID + "." + strconv.FormatUint(credentialVer, 10) + "." + accessToken
	if len(token) > MaxSessionConnectTokenSize {
		return "", fmt.Errorf("proto: session connect token too long")
	}
	return token, nil
}

// ParseSessionConnectToken 按 envelope 的前三个分隔点切分，最后一段保留完整 JWT。
func ParseSessionConnectToken(token string) (*SessionConnectToken, error) {
	token = strings.TrimSpace(token)
	if len(token) > MaxSessionConnectTokenSize {
		return nil, fmt.Errorf("proto: session connect token too long")
	}
	payload, ok := strings.CutPrefix(token, SessionConnectScheme+".")
	if !ok {
		return nil, fmt.Errorf("proto: invalid session connect scheme")
	}
	parts := strings.SplitN(payload, ".", 3)
	if len(parts) != 3 || !ValidHostID(parts[0]) {
		return nil, fmt.Errorf("proto: invalid session connect token")
	}
	ver, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || ver == 0 {
		return nil, fmt.Errorf("proto: invalid session credential generation")
	}
	accessToken := parts[2]
	if _, err := SessionAccessTokenUID(accessToken); err != nil {
		return nil, err
	}
	return &SessionConnectToken{HostID: parts[0], CredentialVer: ver, AccessToken: accessToken}, nil
}

// SessionAccessTokenUID 解析 Web access token 的 uid，供 Pod 在重连前阻止账户漂移。
// 不验签：服务端仍是签名、有效期和数据库状态的唯一认证边界。
func SessionAccessTokenUID(accessToken string) (string, error) {
	parts := strings.Split(strings.TrimSpace(accessToken), ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("proto: web access token must be a three-segment JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("proto: web access token payload: %w", err)
	}
	var claims struct {
		UID     string `json:"uid"`
		Subject string `json:"sub"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("proto: web access token payload: %w", err)
	}
	if claims.Type != "access" {
		return "", fmt.Errorf("proto: session requires a web access token")
	}
	uid := claims.UID
	if uid == "" {
		uid = claims.Subject
	}
	if !ValidHostOwnerID(uid) {
		return "", fmt.Errorf("proto: web access token has invalid uid")
	}
	return uid, nil
}
