package vsh

import (
	"context"
	"time"

	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

// CallerFromContext 由可信执行上下文构造业务调用身份（browser/cua 等
// vsh 指令调用业务服务用）：Subject=Owner（用户身份）、Origin=Session
// （会话）；有效期跟随执行 ctx（30min 墙钟）。审批事实不从模型可控
// 的 argv/env 读取——GrantApproved 由引擎注入的可信 ctx 携带。
func CallerFromContext(ctx context.Context) wire.Caller {
	owner, session := OwnerFromContext(ctx), SessionFromContext(ctx)
	c := wire.Caller{
		Subject:       owner,
		Origin:        session,
		ConnectionID:  "vsh:" + owner + ":" + session,
		GrantApproved: GrantApprovedFromContext(ctx),
		ExpiresAt:     time.Now().Add(time.Hour),
	}
	if dl, ok := ctx.Deadline(); ok {
		c.ExpiresAt = dl
	}
	return c
}
