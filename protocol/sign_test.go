package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// unsignedJWT 造一枚不签名的三段 JWT 形态串（pod 侧解析不验签——
// 签名密钥在平台；这里只测解析形态，伪造串在平台侧必然被拒）。
func unsignedJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// 固定向量：锁定 HKDF 派生的位级一致性（双端零漂移）。
// 修改派生参数必须同步更新本文件，并视为协议变更。

const (
	vecSecret = "dGVzdC1zZWNyZXQtMDEyMzQ1Njc4OWFiY2RlZg"
	vecHostID = "host_vec01"
)

func TestDeriveKToolVector(t *testing.T) {
	kt, err := DeriveKTool(vecSecret, vecHostID)
	if err != nil {
		t.Fatal(err)
	}
	if kt != vecKTool {
		t.Errorf("K_tool = %q, want %q", kt, vecKTool)
	}
	if _, err := DeriveKTool("", vecHostID); err == nil {
		t.Error("empty secret want error")
	}
	if _, err := DeriveKTool(vecSecret, ""); err == nil {
		t.Error("empty hostID want error")
	}
}

func TestParseDeviceCredential(t *testing.T) {
	composite := ComposeDeviceCredential("host32hex", unsignedJWT(map[string]any{
		"uid": "u_vec", "sub": "u_vec", "ver": 3, "type": "static", "mod": "limited",
	})+".secret42")
	dc, err := ParseDeviceCredential(composite)
	if err != nil {
		t.Fatal(err)
	}
	if dc.HostID != "host32hex" || dc.UID != "u_vec" || dc.Ver != 3 || dc.Secret != "secret42" {
		t.Errorf("parsed = %+v", dc)
	}
	// 认证用 credential = 原样四段（不含 host_id 前缀）
	want := strings.TrimPrefix(composite, "host32hex.")
	if dc.Credential != want {
		t.Errorf("credential = %q, want %q", dc.Credential, want)
	}

	// 负例：缺段 / 缺 uid / 缺 ver / 非 static
	for _, bad := range []string{
		"no-separator",
		"host.only.two",
		"host." + unsignedJWT(map[string]any{"uid": "u", "ver": 1, "type": "access"}) + ".s", // 非 static
		"host." + unsignedJWT(map[string]any{"ver": 1, "type": "static"}) + ".s",             // 缺 uid
		"host." + unsignedJWT(map[string]any{"uid": "u", "type": "static"}) + ".s",           // 缺 ver
		"host." + unsignedJWT(map[string]any{"uid": "u", "ver": 1, "type": "static"}),        // 缺 secret 段
	} {
		if _, err := ParseDeviceCredential(bad); err == nil {
			t.Errorf("bad credential %q want error", bad)
		}
	}
}

// （hosts_nats/3：ToolRequest 逐字段签名机制已删除——hosts_nats 信封对完整
// 请求做 HMAC，覆盖 grant_approved 标记；见 protocol/hosts_nats/types_test.go。）
