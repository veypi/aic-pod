package host

// Connect 是 cli/desktop 共用的 host 会话启动入口（同一套代码，同一版本语义）。
// NATS 端点由 opts.Host 推断（ResolveNATSURL），无显式 url 配置。
// 返回已连接的 Client（内部 goroutine 维护心跳/订阅，非阻塞）。
func Connect(opts Options) (*Client, error) {
	c := New(opts)
	if err := c.Connect(); err != nil {
		return nil, err
	}
	return c, nil
}
