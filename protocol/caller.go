package protocol

import (
	"context"
	"time"
)

// Caller 是已认证请求的调用者身份（hosts-vsh-redesign §3：身份和会话来自
// 已认证请求，经执行上下文传递；不再有数字等级）。
//
// Subject=用户 id；Origin=会话 id（RTC 由票据绑定，不能由请求字段冒认）；
// GrantApproved=服务端确认过的「本次脚本可通过 grant 修改授权」事实，
// 默认 false，只能由可信适配器写入，不从脚本可修改的 argv/env 读取。
type Caller struct {
	Scope         string // Empty = device capabilities; fs = owner file proxy.
	Direct        bool   // Set only by the authenticated RTC adapter.
	Subject       string
	ConnectionID  string
	Origin        string
	GrantApproved bool
	ExpiresAt     time.Time
	// Check is supplied by the authenticated adapter, never decoded from tool args.
	Expiry func() time.Time
	Check  func(context.Context) error
}

func (c Caller) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Subject == "" || c.ConnectionID == "" || !c.Deadline().After(time.Now()) {
		return Fail("unauthorized", "Caller authorization expired")
	}
	if c.Check != nil {
		return c.Check(ctx)
	}
	return nil
}

func (c Caller) Deadline() time.Time {
	if c.Expiry != nil {
		return c.Expiry()
	}
	return c.ExpiresAt
}
