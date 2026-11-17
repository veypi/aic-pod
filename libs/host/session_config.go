package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/veypi/aic-pod/protocol"
)

const accessTokenProviderTimeout = 15 * time.Second

// ErrSessionAccountChanged 表示 token provider 已切到另一个 uid。
// 此时必须销毁当前 Client 并走 SwitchAccount，禁止在旧连接上悄悄换身份。
var ErrSessionAccountChanged = errors.New("session account changed; rebuild the host client")

// AccessTokenProvider 由原生认证管理器实现。每次 NATS 连接/重连和 HTTP 请求
// 都会重新调用；实现可在内部 singleflight 刷新，但不能返回 refresh/static token。
type AccessTokenProvider func(context.Context) (string, error)

// SessionConfig 是 ensure 返回的 Host 连接档案加动态 Web token 来源。
// ProtocolSecret 只用于本地 K_tool / RTC 派生，绝不进入 CONNECT 或 HTTP。
type SessionConfig struct {
	HostID              string
	UID                 string
	CredentialVer       uint64
	ProtocolSecret      string
	AccessTokenProvider AccessTokenProvider
}

type connectionIdentity struct {
	hostID    string
	uid       string
	credVer   uint64
	authToken string
	secret    string
	kTool     string
	session   bool
}

// identityFromOptions 只解析稳定连接档案，不调用 token provider。
// 静态 Options.Key 与 Session 互斥，避免制造半静态半会话的伪配置。
func identityFromOptions(opts Options) (connectionIdentity, error) {
	hasKey := strings.TrimSpace(opts.Key) != ""
	hasSession := opts.Session != nil
	if hasKey == hasSession {
		return connectionIdentity{}, fmt.Errorf("exactly one of static key or session config is required")
	}
	if hasKey {
		dc, err := protocol.ParseDeviceCredential(opts.Key)
		if err != nil {
			return connectionIdentity{}, fmt.Errorf("invalid device credential: %w", err)
		}
		kTool, err := protocol.DeriveKTool(dc.Secret, dc.HostID)
		if err != nil {
			return connectionIdentity{}, fmt.Errorf("derive device tool key: %w", err)
		}
		return connectionIdentity{hostID: dc.HostID, uid: dc.UID, credVer: dc.Ver, authToken: dc.AuthToken, secret: dc.Secret, kTool: kTool}, nil
	}

	s := *opts.Session
	if !protocol.ValidHostID(s.HostID) || !protocol.ValidHostOwnerID(s.UID) || s.HostID[:protocol.HostOwnerIDLength] != s.UID {
		return connectionIdentity{}, fmt.Errorf("invalid session host identity")
	}
	if s.CredentialVer == 0 {
		return connectionIdentity{}, fmt.Errorf("invalid session credential generation")
	}
	if strings.TrimSpace(s.ProtocolSecret) == "" {
		return connectionIdentity{}, fmt.Errorf("session protocol secret is empty")
	}
	if s.AccessTokenProvider == nil {
		return connectionIdentity{}, fmt.Errorf("session access token provider is required")
	}
	kTool, err := protocol.DeriveKTool(s.ProtocolSecret, s.HostID)
	if err != nil {
		return connectionIdentity{}, fmt.Errorf("derive session tool key: %w", err)
	}
	return connectionIdentity{hostID: s.HostID, uid: s.UID, credVer: s.CredentialVer, secret: s.ProtocolSecret, kTool: kTool, session: true}, nil
}

func (c *Client) installIdentity(identity connectionIdentity) {
	c.hostID = identity.hostID
	c.uid = identity.uid
	c.credVer = identity.credVer
	c.authToken = identity.authToken
	c.secret = identity.secret
	c.kTool = identity.kTool
}

// bearerToken 返回普通 HTTP 使用的原始凭证：静态 JWT 或最新 Web access token。
// Session 的 s1. envelope 只由 natsToken 另外组装，不能进入 HTTP Authorization。
func (c *Client) bearerToken(ctx context.Context) (string, error) {
	opts := c.options()
	if opts.Session == nil {
		if c.authToken == "" {
			return "", fmt.Errorf("device credential is not configured")
		}
		return c.authToken, nil
	}
	providerCtx, cancel := context.WithTimeout(ctx, accessTokenProviderTimeout)
	defer cancel()
	token, err := opts.Session.AccessTokenProvider(providerCtx)
	if err != nil {
		return "", fmt.Errorf("access token provider: %w", err)
	}
	uid, err := protocol.SessionAccessTokenUID(token)
	if err != nil {
		return "", err
	}
	if uid != c.uid {
		return "", fmt.Errorf("%w: token uid %s, client uid %s", ErrSessionAccountChanged, uid, c.uid)
	}
	return strings.TrimSpace(token), nil
}

func (c *Client) natsToken(ctx context.Context) (string, error) {
	token, err := c.bearerToken(ctx)
	if err != nil {
		return "", err
	}
	if c.options().Session == nil {
		return token, nil
	}
	return protocol.ComposeSessionConnectToken(c.hostID, c.credVer, token)
}

// natsTokenHandler 是 nats.TokenHandler 的适配器。nats.go 的回调不能返回 error，
// 因此 provider 失败时显式记录并返回空 token，让服务端拒绝；绝不复用旧 token。
func (c *Client) natsTokenHandler() string {
	token, err := c.natsToken(context.Background())
	if err != nil {
		if c.logf != nil {
			c.logf("NATS token provider failed: %v", err)
		}
		return ""
	}
	return token
}

func staticTokenProvider(token string) AccessTokenProvider {
	return func(context.Context) (string, error) {
		if token == "" {
			return "", fmt.Errorf("device credential is not configured")
		}
		return token, nil
	}
}
