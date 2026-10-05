// hosts_nats/3 签名信封：工具请求绑定已认证目的地与调用方。
package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const NatsProtocol = "hosts_nats/3"

type NatsRequest struct {
	HostID             string `json:"host_id"`
	Subject            string `json:"subject"`
	Caller             string `json:"caller"`
	Origin             string `json:"origin,omitempty"`
	Scope              string `json:"scope,omitempty"`
	AuthorizationUntil int64  `json:"authorization_until_ms"`
	// GrantApproved 是服务端审批结果事实（默认 false）：本次脚本可通过 grant
	// 修改授权。随签名信封传递，模型不能自行设置，篡改即验签失败。
	GrantApproved bool    `json:"grant_approved,omitempty"`
	Nonce         string  `json:"nonce"`
	Deadline      int64   `json:"deadline_ms"`
	Request       Request `json:"request"`
	Signature     string  `json:"signature"`
}

func NatsSubject(uid, host string) (string, error) {
	if uid == "" || host == "" || strings.ContainsAny(uid+host, ".*> \t\n\r") {
		return "", fmt.Errorf("invalid destination")
	}
	return "u." + uid + ".h.host_" + host + ".tools.req", nil
}
func signature(key string, r NatsRequest) string {
	r.Signature = ""
	b, _ := json.Marshal(r)
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(NatsProtocol + "\n"))
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// 跨机时效校验的时钟容差：校验方以平台时钟为基准（设备侧对时见 libs/host/clock.go），
// 校准误差与漂移是毫秒~秒级——窗口判定不能零余量：平台签发的 AuthorizationUntil 恒为
// 「平台 now+30min」，零容差时「设备校准时间略慢于平台」会把整段会话的请求全部拒掉；
// Deadline 上限须覆盖平台工具请求的最长等待（aic MaxToolReqTimeout=10min，此前 5min
// 上限让所有 >5min 的调用（如 curl 600s）必然被拒）。
const (
	clockSlack          = 2 * time.Minute
	authorizationWindow = 30*time.Minute + clockSlack
	deadlineHorizon     = 15 * time.Minute
)

// NatsSign 为信封签名（先清 Signature 再覆盖全字段）。
func NatsSign(key string, r *NatsRequest) { r.Signature = signature(key, *r) }

// NatsVerify 校验信封目的地、身份、时效窗口与签名。
func NatsVerify(key, host, subject string, r NatsRequest, now time.Time) error {
	if (r.Scope != "" && r.Scope != "fs") || (r.Origin != "" && !ValidID(r.Origin)) || r.AuthorizationUntil < r.Deadline || r.AuthorizationUntil > now.Add(authorizationWindow).UnixMilli() || r.HostID != host || r.Subject != subject || !ValidID(r.Caller) || !ValidID(r.Nonce) || r.Deadline <= now.Add(-clockSlack).UnixMilli() || r.Deadline > now.Add(deadlineHorizon).UnixMilli() {
		return Fail("unauthorized", "Invalid request destination, identity or validity window")
	}
	// Caller 必须与签名 subject 目的地中的 uid 一致：执行归属统一从
	// caller.Subject 派生（exec/任务表/取消登记），信封里的 Caller 不能
	// 与服务端路由的归属 uid 脱节。
	if dest, err := NatsSubject(r.Caller, host); err != nil || r.Subject != dest {
		return Fail("unauthorized", "Caller does not match request destination")
	}
	if !hmac.Equal([]byte(signature(key, r)), []byte(r.Signature)) {
		return Fail("unauthorized", "Invalid request signature")
	}
	if r.Request.Protocol != NatsProtocol {
		return Fail("unsupported", "Invalid request protocol")
	}
	return r.Request.Validate()
}
