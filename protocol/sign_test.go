package protocol

import (
	"testing"
)

// 固定向量：锁定 HKDF 派生与 canonical 输入的位级一致性（双端零漂移）。
// 修改派生参数或 canonical 结构必须同步更新本文件，并视为协议变更。

const (
	vecSecret = "dGVzdC1zZWNyZXQtMDEyMzQ1Njc4OWFiY2RlZg"
	vecHostID = "host_vec01"
	vecUID    = "u_vec"
)

func TestDeriveKeysVector(t *testing.T) {
	kc, ks, kt, err := DeriveKeys(vecSecret, vecHostID)
	if err != nil {
		t.Fatal(err)
	}
	if kc != vecKConnect {
		t.Errorf("K_connect = %q, want %q", kc, vecKConnect)
	}
	if ks != vecKServer {
		t.Errorf("K_server = %q, want %q", ks, vecKServer)
	}
	if kt != vecKTool {
		t.Errorf("K_tool = %q, want %q", kt, vecKTool)
	}
	if _, _, _, err := DeriveKeys("", vecHostID); err == nil {
		t.Error("empty secret want error")
	}
	if _, _, _, err := DeriveKeys(vecSecret, ""); err == nil {
		t.Error("empty hostID want error")
	}
}

func TestConnectTokenVector(t *testing.T) {
	token := GenerateConnectToken(vecHostID, vecUID, "v0.3.0", "cli", "nas-01",
		1767225600000, "BBBBBBBBBBBBBBBBBBBBBB", vecKConnect)
	if token != vecConnectToken {
		t.Errorf("token = %q, want %q", token, vecConnectToken)
	}
	ct, err := ParseConnectToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if ct.HostID != vecHostID || ct.UnixMS != 1767225600000 || ct.Nonce != "BBBBBBBBBBBBBBBBBBBBBB" {
		t.Errorf("parsed = %+v", ct)
	}
	if ct.EnvInfo != `{"agent_version":"v0.3.0","device_name":"nas-01","device_type":"cli"}` {
		t.Errorf("env_info = %q", ct.EnvInfo)
	}
	if _, err := ParseConnectToken("e1.only.two"); err == nil {
		t.Error("bad token want error")
	}
}

func TestNonceUnique(t *testing.T) {
	a, err1 := NewNonce()
	b, err2 := NewNonce()
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if a == b || len(a) != 22 {
		t.Errorf("nonces = %q %q", a, b)
	}
}

// （hosts_nats/3：ToolRequest 逐字段签名机制已删除——hosts_nats 信封对完整
// 请求做 HMAC，覆盖 grant_approved 标记；见 protocol/hosts_nats/types_test.go。）
