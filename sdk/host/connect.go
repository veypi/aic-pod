package host

// Connect 是 cli/desktop 共用的 host 会话启动入口（同一套代码，同一版本语义）。
//
//   - hostURL：平台地址（https://ivec.ai 或 http://127.0.0.1:4000）
//   - natsURL：显式 NATS 端点（空 = 经 ResolveNATSURL 按 hostURL 推断）
//   - opts：其余选项由调用方填充（Credential/Version/WorkDir/DeviceName/
//     ExecTimeout/OnLog 等），NATSURL 为空时自动推断
//
// 返回已连接的 Client（内部 goroutine 维护心跳/订阅，非阻塞）。
func Connect(hostURL, natsURL string, opts Options) (*Client, error) {
	if opts.NATSURL == "" {
		opts.NATSURL = ResolveNATSURL(hostURL, natsURL)
	}
	c := New(opts)
	if err := c.Connect(); err != nil {
		return nil, err
	}
	return c, nil
}
