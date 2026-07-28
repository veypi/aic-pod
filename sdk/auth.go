package aichost

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// generateConnectToken 生成连接 token。
func generateConnectToken(hostID, uid, version, deviceType, deviceName, kConnect string) string {
	envInfo, _ := json.Marshal(map[string]string{
		"agent_version": version,
		"device_type":   deviceType,
		"device_name":   deviceName,
	})
	envInfoB64 := base64.RawURLEncoding.EncodeToString(envInfo)

	unixMs := time.Now().UnixMilli()
	tsStr := fmt.Sprintf("%d", unixMs)

	nonce := make([]byte, 16)
	rand.Read(nonce)
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)

	sigInput := canonicalConnectSigInput(hostID, uid, string(envInfo), unixMs, nonceB64)
	sig := hmacSHA256Raw(kConnect, sigInput)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return fmt.Sprintf("e1.%s.%s.%s.%s.%s", hostID, envInfoB64, tsStr, nonceB64, sigB64)
}

func canonicalConnectSigInput(hostID, uid, envInfo string, unixMs int64, nonce string) string {
	b, _ := json.Marshal(connectSigPayload{
		Domain:   "aic-host-connect-v1",
		HostID:   hostID,
		UID:      uid,
		HostInfo: envInfo,
		UnixMS:   unixMs,
		Nonce:    nonce,
	})
	return string(b)
}

func canonicalToolReqSigInput(hostID, msgID, sessionID, toolName, nonce, deadline string, toolData any, grantedLevel int) string {
	toolDataJSON, _ := json.Marshal(toolData)
	toolDataHash := fmt.Sprintf("%x", sha256.Sum256(toolDataJSON))
	b, _ := json.Marshal(toolReqSigPayload{
		Version:        1,
		HostID:         hostID,
		MsgID:          msgID,
		SessionID:      sessionID,
		ToolName:       toolName,
		ToolDataSHA256: toolDataHash,
		GrantedLevel:   grantedLevel,
		Nonce:          nonce,
		Deadline:       deadline,
	})
	return string(b)
}

func verifyToolRequestSig(req *toolRequest, hostID, kTool string) bool {
	expected := canonicalToolReqSigInput(hostID, req.MsgID, req.SessionID, req.ToolName, req.Nonce, req.Deadline, req.ToolData, req.GrantedLevel)
	mac := hmac.New(sha256.New, []byte(kTool))
	mac.Write([]byte(expected))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(req.Signature), []byte(expectedSig))
}
