package proto

// RTC 信令（2026-09-10，WebRTC 直连）：页面为 offer 方，host 为应答方。
// 信令双向走 NATS（入向 RtcInSubject / 出向 RtcOutSubject，权限模型见
// natsauth 注释）；数据面为 DataChannel（DTLS 强制加密，UDP host candidate
// LAN 直连，无 STUN/TURN/CA 证书依赖——身份锚点 = 信令通道交换的 SDP 指纹）。

// RTC 信令种类（RtcSignal.Kind）。
const (
	RtcOffer     = "offer"     // 页面 → 设备：SDP offer
	RtcAnswer    = "answer"    // 设备 → 页面：SDP answer
	RtcCandidate = "candidate" // 双向：trickle ICE candidate
	RtcBye       = "bye"       // 页面 → 设备：主动关闭连接
)

// RtcSignal 是 rtc.in / rtc 出向 subject 上的信令信封。
type RtcSignal struct {
	PC   string `json:"pc"`   // 页面侧生成的连接 id（关联 offer/answer/trickle 全流程）
	Kind string `json:"kind"` // RtcOffer / RtcAnswer / RtcCandidate / RtcBye
	SDP  string `json:"sdp,omitempty"`
	// Candidate 是 trickle ICE 候选的 JSON 字符串（浏览器 RTCIceCandidate.toJSON()
	// 与 pion ICECandidate.ToJSON() 同为 ICECandidateInit 形状，直接对传）。
	Candidate string `json:"candidate,omitempty"`
}
