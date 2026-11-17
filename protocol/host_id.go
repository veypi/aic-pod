package protocol

const (
	HostOwnerIDLength = 32
	HostIDLength      = 36
)

// ValidHostOwnerID 校验 32 位小写无连字符 UUID。
func ValidHostOwnerID(id string) bool {
	if len(id) != HostOwnerIDLength {
		return false
	}
	for i := range len(id) {
		if (id[i] < '0' || id[i] > '9') && (id[i] < 'a' || id[i] > 'f') {
			return false
		}
	}
	return true
}

// ValidHostID 只接受 32 位 owner uid + 4 位非零十进制序号组成的完整 Host 身份。
// 旧 32 位 ID 和 AI 边界的 4 位短引用都不是线上 Host 身份。
func ValidHostID(id string) bool {
	if len(id) != HostIDLength || !ValidHostOwnerID(id[:HostOwnerIDLength]) || id[HostOwnerIDLength:] == "0000" {
		return false
	}
	for i := HostOwnerIDLength; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}
