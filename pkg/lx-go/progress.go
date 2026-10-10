package lxgo

import (
	"encoding/json"
	"sync"
	"time"
)

// ProgressEvent 描述一次 /api/music/url 解析过程中的一步进展，
// 通过 SSE 推送给订阅了同一个 reqId 的 /api/music/progress 连接。
type ProgressEvent struct {
	Stage    string `json:"stage"`    // trying | success | failed | error | done
	Source   string `json:"source"`   // 当前尝试的音源脚本名称（@name）
	Platform string `json:"platform"` // wy/tx/kw/kg/mg...
	Message  string `json:"message"`
	Done     bool   `json:"done"` // true 表示这是最后一条事件，SSE 连接可以关闭了
}

// ProgressHub 是一个按 reqId 分组的简单发布/订阅中心，用于把 Go 侧"正在尝试哪个
// 音源、成功/失败"这些内部过程，实时透传给通过 SSE 订阅同一个 reqId 的客户端。
type ProgressHub struct {
	mu   sync.Mutex
	subs map[string][]chan ProgressEvent
}

func NewProgressHub() *ProgressHub {
	return &ProgressHub{subs: make(map[string][]chan ProgressEvent)}
}

// Subscribe 订阅指定 reqId 的进度事件，返回只读 channel 和取消订阅函数。
// channel 有缓冲区，避免慢消费者阻塞正在解析 URL 的请求处理协程；
// Publish 端发送时使用非阻塞写，缓冲区满则丢弃最旧的进度（不影响最终结果）。
func (h *ProgressHub) Subscribe(reqID string) (<-chan ProgressEvent, func()) {
	ch := make(chan ProgressEvent, 16)
	h.mu.Lock()
	h.subs[reqID] = append(h.subs[reqID], ch)
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.subs[reqID]
		for i, c := range list {
			if c == ch {
				h.subs[reqID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(h.subs[reqID]) == 0 {
			delete(h.subs, reqID)
		}
		close(ch)
	}
	return ch, cancel
}

// Publish 把一条进度事件广播给当前订阅了该 reqId 的所有 SSE 连接。
// 如果没有任何人订阅（比如调用方压根没传 x-req-id），直接丢弃，零开销。
func (h *ProgressHub) Publish(reqID string, ev ProgressEvent) {
	if reqID == "" {
		return
	}
	h.mu.Lock()
	subs := append([]chan ProgressEvent(nil), h.subs[reqID]...)
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// 消费者太慢，丢弃最旧的一条腾出空间，保证最新进度不丢
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

func (ev ProgressEvent) marshal() []byte {
	data, err := json.Marshal(ev)
	if err != nil {
		return []byte(`{"stage":"error","message":"marshal progress failed"}`)
	}
	return data
}

// progressReqID 从请求头里取可选的 x-req-id，用于串联 /api/music/url 与
// /api/music/progress 这两次独立的 HTTP 请求。
const progressHeader = "x-req-id"

// progressSubscribeTimeout 是 SSE 连接在没有任何事件时的最长等待时间，
// 超时后主动关闭连接（避免请求早已完成但客户端忘了带 reqId 导致的连接泄漏）。
const progressSubscribeTimeout = 60 * time.Second
