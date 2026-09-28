// Package hosts_tool 是过渡别名层：Caller/Stream 已移至
// protocol/hosts_tools（hosts_tools/2 传输无关契约）。
//
// hosts-vsh-redesign §5：原声明体系（Command/Method/Bind/Translate/
// AccessRules/RequiredLevel/Dispatcher）已删除——命令执行只走 vsh
// Engine.Exec 完整脚本，FS 为数据面直连，stream 为 RTC 私有端点。
package hosts_tool

import wire "github.com/veypi/aic-pod/protocol/hosts_tools"

// Caller 是已认证请求的调用者身份（定义见 protocol/hosts_tools）。
type Caller = wire.Caller

// Stream 是 opaque 双工消息端点（定义见 protocol/hosts_tools）。
type Stream = wire.Stream
