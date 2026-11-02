package host

import "github.com/veypi/aic-pod/libs/rtc"

// streamResolver 是 rtc.Config.Streams 的装配：按 DataChannel 标签现查统一
// command hub（通道开启时解析，运行期 install 的流式服务无需重启 RTC）。
func (c *Client) streamResolver(label string) (rtc.StreamRelay, bool) {
	if c.cmds != nil {
		if relay, ok := c.cmds.streamResolver(label); ok {
			return relay, true
		}
	}
	return nil, false
}
