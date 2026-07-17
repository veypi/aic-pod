package aicenv

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
func generateConnectToken(envID, uid, version, deviceType, deviceName, kConnect string) string {
	envInfo, _ := json.Marshal(map[string]string{
		"env_version": version,
		"env_type":    deviceType,
		"env_name":    deviceName,
	})
	envInfoB64 := base64.RawURLEncoding.EncodeToString(envInfo)

	unixMs := time.Now().UnixMilli()
	tsStr := fmt.Sprintf("%d", unixMs)

	nonce := make([]byte, 16)
	rand.Read(nonce)
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)

	sigInput := canonicalConnectSigInput(envID, uid, string(envInfo), unixMs, nonceB64)
	sig := hmacSHA256Raw(kConnect, sigInput)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return fmt.Sprintf("e1.%s.%s.%s.%s.%s", envID, envInfoB64, tsStr, nonceB64, sigB64)
}

func canonicalConnectSigInput(envID, uid, envInfo string, unixMs int64, nonce string) string {
	b, _ := json.Marshal(connectSigPayload{
		Domain:  "aic-env-connect-v1",
		EnvID:   envID,
		UID:     uid,
		EnvInfo: envInfo,
		UnixMS:  unixMs,
		Nonce:   nonce,
	})
	return string(b)
}

func canonicalToolReqSigInput(envID, msgID, sessionID, toolName, nonce, deadline string, toolData any, grantedLevel int, approvalFingerprint *string) string {
	toolDataJSON, _ := json.Marshal(toolData)
	toolDataHash := fmt.Sprintf("%x", sha256.Sum256(toolDataJSON))
	b, _ := json.Marshal(toolReqSigPayload{
		Version:             1,
		EnvID:               envID,
		MsgID:               msgID,
		SessionID:           sessionID,
		ToolName:            toolName,
		ToolDataSHA256:      toolDataHash,
		GrantedLevel:        grantedLevel,
		Nonce:               nonce,
		Deadline:            deadline,
		ApprovalFingerprint: approvalFingerprint,
	})
	return string(b)
}

func verifyToolRequestSig(req *toolRequest, envID, kTool string) bool {
	expected := canonicalToolReqSigInput(envID, req.MsgID, req.SessionID, req.ToolName, req.Nonce, req.Deadline, req.ToolData, req.GrantedLevel, approvalFingerprint(req))
	mac := hmac.New(sha256.New, []byte(kTool))
	mac.Write([]byte(expected))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(req.Signature), []byte(expectedSig))
}

func approvalFingerprint(req *toolRequest) *string {
	if req.Approval != nil {
		return &req.Approval.Fingerprint
	}
	return nil
}
