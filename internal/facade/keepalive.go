package facade

import (
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/sse"
)

// streamKeepAliveInterval 是 Chat / Anthropic 流等待上游期间的保活间隔。
const streamKeepAliveInterval = 15 * time.Second

var (
	// anthropicPingFrame 是 Anthropic 协议自带的保活事件，客户端会忽略它。
	anthropicPingFrame = []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
	// sseCommentFrame 是 SSE 注释行，任何 SSE 客户端都会跳过。
	sseCommentFrame = []byte(": keepalive\n\n")
)

// keepAlive 在等待上游期间每隔 every 写一帧 frame，直到返回的 stop 被调用。
//
// 上游一轮常要几十秒到几分钟（沙箱重试、xhigh 推理），期间一个字节都不发，客户端与
// 中间代理会按空闲把连接断掉：Claude Code 等满 5 分钟没收到数据就断开（2026-10-05 实测）。
// sse.Writer 自带锁，保活帧可以与正文帧交错写出。
func keepAlive(sw *sse.Writer, every time.Duration, frame []byte) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if sw.WriteRaw(frame) != nil {
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}
