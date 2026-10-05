// Package protocol 是 aic 与 aic-pod 之间的全部线上契约（批次 2 由
// protocol/{tool,fs,hosts_nats,hosts_rtc} 四子包与 libs/proto 合并为单一
// Go 包；NATS/RTC 的信封与签名格式仍各自独立，Go 类型用前缀区分，不统一成
// 万能 Envelope）。按文件分组：
//
//   - tool.go：传输无关的工具请求/响应（Request/Response/Output/Fault/
//     FSInvocation/ExecPayload）、错误模型（Fail/AsFault/Reply）、身份与
//     边界校验（ValidID/ValidName/Decode/NewID/MaxMessageBytes）。
//   - caller.go：已认证请求的调用者身份（Caller），经执行上下文传递。
//   - fs.go：FS 数据面类型（FSContract/FSPath/FSEntry/FSCondition/
//     FSResourceRef/FSMaxSafeInteger/FSFail——FSFail 保留 Effect=none 语义）。
//   - hosts_nats.go：hosts_nats/3 签名信封（NatsProtocol/NatsRequest/
//     NatsSubject/NatsSign/NatsVerify）。
//   - hosts_rtc.go：hosts_rtc/3 设备数据通道信封（RtcProtocol/RtcChannel/
//     RtcBrowserChannel/RtcRequest/RtcAuthResult 及分块上限）。
//   - rtc_ticket.go：RTC 直连票据（RtcTicket/SignRtcTicket/VerifyRtcTicket/
//     RtcDirectKey/NormalizeFingerprint 与租约常量）。
//   - rtc_signal.go：NATS 上的 RTC 信令信封（RtcSignal 与种类常量）。
//   - subject.go：NATS subject 构造（conn/caps/presence/rtc/fetch 族）与
//     工具名常量。
//   - sign.go：host 连接凭证（ConnectToken/HostConnectInfo/DeriveKeys/
//     GenerateConnectToken/ParseConnectToken）。
//   - caps.go：设备能力声明（Caps/DeviceInfo/FSCaps/MgmtCaps/TransportCaps
//     与版本协商）。
//   - path.go：路径可解析层（ResolvePath/NormalizeHostPath/SplitDriveRoot/
//     WinTmpToOS/WithinRoots——纯语法、双端结果一致；原生 OS 路径转换统一
//     用 vbox.HostPathToOS）。
package protocol
