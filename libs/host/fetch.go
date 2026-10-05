package host

// 静态技能资料下载：经 host 到 cloud 的
// 已认证 NATS 连接拉包 zip——pod 不持有平台 HTTP 凭据，包获取走 NATS。
// 请求 = protocol.FetchReqSubject（payload FetchRequest，Reply = fetch.{reqID}
// 临时地址）；应答分块帧首字节 0x00=zip 分块（同 subject 顺序保证）、
// 0x01=FetchResult JSON 终结帧（Error 非空 = 失败无分块）。接收侧校验
// bytes/sha256 后返回 ZIP；调用方保存到显式路径，不执行或注册。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"time"

	"github.com/nats-io/nats.go"
)

// 分块帧首字节类别（与 aic libs/host/fetch.go 同一协议）。
const (
	fetchFrameChunk = 0x00
	fetchFrameDone  = 0x01
)

// fetchTimeout 单次拉包上限（大包分块传输 + 云端本地目录即时打包）。
const fetchTimeout = 10 * time.Minute

// fetchSkillZip 验证已认证传输返回的静态 ZIP。
func (c *Client) fetchSkillZip(ctx context.Context, ref, version string) ([]byte, *protocol.FetchResult, error) {
	c.ncMu.RLock()
	nc := c.nc
	c.ncMu.RUnlock()
	if nc == nil {
		return nil, nil, fmt.Errorf("skill download: host offline（NATS 未连接）")
	}
	reqID := mustNonce()
	inbox, err := protocol.FetchInboxSubject(c.uid, c.hostID, c.credVer, reqID)
	if err != nil {
		return nil, nil, err
	}
	reqSubj, err := protocol.FetchReqSubject(c.uid, c.hostID, c.credVer)
	if err != nil {
		return nil, nil, err
	}
	// 先订阅再发请求（分块即发即达，漏订阅 = 丢块）。
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, nil, fmt.Errorf("skill download: subscribe inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	payload, err := json.Marshal(protocol.FetchRequest{ReqID: reqID, Ref: ref, Version: version})
	if err != nil {
		return nil, nil, err
	}
	if err := nc.PublishMsg(&nats.Msg{Subject: reqSubj, Reply: inbox, Data: payload}); err != nil {
		return nil, nil, fmt.Errorf("skill download: publish request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	var buf bytes.Buffer
	for {
		msg, err := sub.NextMsgWithContext(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("skill download: fetch interrupted: %w", err)
		}
		if len(msg.Data) == 0 {
			return nil, nil, fmt.Errorf("skill download: empty frame")
		}
		switch msg.Data[0] {
		case fetchFrameChunk:
			if buf.Len()+len(msg.Data)-1 > 16<<20 {
				return nil, nil, fmt.Errorf("skill download exceeds 16 MiB")
			}
			buf.Write(msg.Data[1:])
		case fetchFrameDone:
			var res protocol.FetchResult
			if err := json.Unmarshal(msg.Data[1:], &res); err != nil {
				return nil, nil, fmt.Errorf("skill download: bad result frame: %w", err)
			}
			if res.Error != "" {
				return nil, nil, fmt.Errorf("skill download: %s", res.Error)
			}
			zipData := buf.Bytes()
			if int64(len(zipData)) != res.Bytes {
				return nil, nil, fmt.Errorf("skill download: size mismatch（got %d, want %d）", len(zipData), res.Bytes)
			}
			sum := sha256.Sum256(zipData)
			if got := hex.EncodeToString(sum[:]); got != res.SHA256 {
				return nil, nil, fmt.Errorf("skill download: sha256 mismatch（got %s）", got)
			}
			return zipData, &res, nil
		default:
			return nil, nil, fmt.Errorf("skill download: unknown frame type 0x%02x", msg.Data[0])
		}
	}
}

func (c *Client) skillFetch(ctx context.Context, _ string, ref, version string) ([]byte, error) {
	data, _, err := c.fetchSkillZip(ctx, ref, version)
	return data, err
}
