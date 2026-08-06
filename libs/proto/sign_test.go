package proto

import (
	"encoding/json"
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

func TestToolRequestSigVector(t *testing.T) {
	req := &ToolRequest{
		MsgID:        "msg_001",
		SessionID:    "s_abc",
		Tool:         ToolExec,
		Data:         []byte(`{"action":"ls","argv":["-la"]}`),
		GrantedLevel: 3,
		Nonce:        "AAAAAAAAAAAAAAAAAAAAAA",
		Deadline:     "2026-01-02T03:04:05Z",
	}
	SignToolRequest(req, vecHostID, vecKTool)
	if req.Sig != vecToolReqSig {
		t.Errorf("sig = %q, want %q", req.Sig, vecToolReqSig)
	}
	if !VerifyToolRequest(req, vecHostID, vecKTool) {
		t.Error("verify failed for freshly signed request")
	}
	// 篡改任一字段验签必失败
	tampered := *req
	tampered.GrantedLevel = 9
	if VerifyToolRequest(&tampered, vecHostID, vecKTool) {
		t.Error("verify passed with tampered granted_level")
	}
	tampered = *req
	tampered.Data = []byte(`{"action":"rm","argv":["-r","/"]}`)
	if VerifyToolRequest(&tampered, vecHostID, vecKTool) {
		t.Error("verify passed with tampered data")
	}
	if VerifyToolRequest(req, "host_other", vecKTool) {
		t.Error("verify passed with wrong host_id")
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

// TestToolRequestWireRoundTrip 锁定“签名字节 == 线上字节”性质（§6.2）：
// data 走服务端真实路径（map → json.Marshal，紧凑 + HTML 转义）时，
// 签名 → 信封序列化 → 反序列化 → 验签必须成立。覆盖 <>& 转义与非 ASCII。
// 历史教训：RawMessage 非 canonical（含未转义 <>&/多余空白）时 appendCompact
// 会改写线上字节，验签静默失败——Data 必须 json.Marshal 产出（见 envelope.go）。
func TestToolRequestWireRoundTrip(t *testing.T) {
	data := map[string]any{
		"action": "browser",
		"argv":   []string{"open", "https://example.com/?a=1&b=<测试>"},
	}
	rawData, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	req := &ToolRequest{
		MsgID:        "msg_wire",
		SessionID:    "s_wire",
		Tool:         ToolExec,
		Data:         rawData,
		GrantedLevel: 2,
		Nonce:        "AAAAAAAAAAAAAAAAAAAAAA",
		Deadline:     "2026-01-02T03:04:05Z",
	}
	SignToolRequest(req, vecHostID, vecKTool)

	// 线上字节（json.Marshal 信封）→ host 端反序列化
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var back ToolRequest
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	if !VerifyToolRequest(&back, vecHostID, vecKTool) {
		t.Error("verify failed after wire round-trip (data contains <>&/非ASCII)")
	}
	// 线上字节的 data 段必须与签名输入逐字节一致（appendCompact 幂等）
	if string(back.Data) != string(rawData) {
		t.Errorf("wire data = %q, want canonical %q", back.Data, rawData)
	}
}
