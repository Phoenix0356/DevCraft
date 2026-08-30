package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// withIdleTimeout 临时把流空闲超时调小，测试结束自动还原（生产值 90 秒，测试不能真等）。
func withIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := streamIdleTimeout
	streamIdleTimeout = d
	t.Cleanup(func() { streamIdleTimeout = old })
}

// newSilentServer 模拟半开连接：只回响应头，永远不发任何 SSE 片段。
func newSilentServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // 阻塞到客户端放弃，期间不吐任何数据
	}))
}

// TestChatStreamIdleTimeout 阻塞不吐数据 → 空闲看门狗必须在预期时间内中断，
// 返回可识别的"响应超时"错误，而不是永久挂起。
func TestChatStreamIdleTimeout(t *testing.T) {
	withIdleTimeout(t, 50*time.Millisecond)
	srv := newSilentServer()
	defer srv.Close()

	c := NewOpenAIClient("test-key", srv.URL+"/v1")
	start := time.Now()
	_, err := c.ChatStream(context.Background(), "m", []Message{{Role: RoleUser, Content: "hi"}}, nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected idle-timeout error, got nil")
	}
	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("err = %v, want errors.Is ErrStreamIdleTimeout", err)
	}
	// 50ms 的超时上限，留足调度余量断言"没有挂死"
	if elapsed > 5*time.Second {
		t.Fatalf("idle timeout took too long: %v", elapsed)
	}
}

// TestChatStreamIdleTimerResetsOnChunks 验证计时器随片段重置：
// 片段间隔小于空闲阈值时流必须正常完成，不触发超时。
func TestChatStreamIdleTimerResetsOnChunks(t *testing.T) {
	withIdleTimeout(t, 200*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		chunk := func(delta string) {
			// 简化的 chat.completion.chunk JSON（delta 内容用 %q 转义，ASCII 下与 JSON 兼容）
			fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", delta)
			f.Flush()
		}
		chunk("Hello")
		time.Sleep(80 * time.Millisecond) // 有心跳：间隔 < 200ms，看门狗必须被重置
		chunk(" world")
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer srv.Close()

	c := NewOpenAIClient("test-key", srv.URL+"/v1")
	var deltas []string
	resp, err := c.ChatStream(context.Background(), "m", []Message{{Role: RoleUser, Content: "hi"}}, nil, func(d string) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Message.Content != "Hello world" {
		t.Fatalf("content = %q, want %q", resp.Message.Content, "Hello world")
	}
	if len(deltas) != 2 {
		t.Fatalf("deltas = %d, want 2", len(deltas))
	}
}

// TestChatStreamParentCancel 上层取消（用户停止/回合超时）→ 原样透传取消错误，
// 不能被误判成"响应超时"。
func TestChatStreamParentCancel(t *testing.T) {
	withIdleTimeout(t, 10*time.Second) // 空闲阈值远大于测试时长，确保不是看门狗触发
	srv := newSilentServer()
	defer srv.Close()

	c := NewOpenAIClient("test-key", srv.URL+"/v1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.ChatStream(ctx, "m", []Message{{Role: RoleUser, Content: "hi"}}, nil, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // 等流建立
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancel error, got nil")
		}
		if errors.Is(err, ErrStreamIdleTimeout) {
			t.Fatalf("parent cancel misclassified as idle timeout: %v", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want errors.Is context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatStream did not return after parent ctx cancel")
	}
}
