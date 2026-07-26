package aichost

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// verifyApprovalFingerprint 按 §2.3 公式重算审批指纹并比对（§10.1 第 4 条）：
//
//	fingerprint = fp:{sessionID}:{tool}:{policyVersion}:{sha256(canonical_json({target,action,argv}))[:16]}
//
// host 端 target 即本 host_id；action/argv 取自 tool_data；
// policyVersion 从指纹自身解析（服务端指令集声明，host 无需独立获知）。
func verifyApprovalFingerprint(fingerprint, sessionID, toolName, hostID string, params *ToolParams) bool {
	// fp:{sessionID}:{tool}:{policyVersion}:{hash16}
	parts := strings.Split(fingerprint, ":")
	if len(parts) != 5 || parts[0] != "fp" {
		return false
	}
	if parts[1] != sessionID || parts[2] != toolName {
		return false
	}
	hash := approvalInputHash(hostID, params.Action, params.Argv)
	return parts[4] == hash
}

// approvalInputHash 计算 {target, action, argv} 的 JCS 规范化 JSON 的
// sha256 前 16 位 hex，与 aic types.ApprovalInputHash 逐字节一致。
func approvalInputHash(target, action string, argv []string) string {
	var b strings.Builder
	b.WriteString(`{"action":`)
	writeJCSString(&b, action)
	b.WriteString(`,"argv":[`)
	for i, s := range argv {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJCSString(&b, s)
	}
	b.WriteByte(']')
	if target != "" {
		b.WriteString(`,"target":`)
		writeJCSString(&b, target)
	}
	b.WriteByte('}')
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// writeJCSString 按 JCS（RFC 8785）规则写 JSON 字符串：最简转义
// （仅 \\、\" 与控制字符），与 aic types.writeJCSString 一致。
func writeJCSString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				const hexDigits = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[byte(r)>>4])
				b.WriteByte(hexDigits[byte(r)&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
