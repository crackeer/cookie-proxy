package proxy

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer 是并发安全的日志缓冲区：日志由服务端 goroutine 写入，
// 测试主 goroutine 读取，直接用 bytes.Buffer 会触发 -race 报警。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLog 等待日志中出现指定片段（日志写入发生在响应发出之后）。
func waitForLog(t *testing.T, b *syncBuffer, substr string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		out := b.String()
		if strings.Contains(out, substr) {
			return out
		}
		if time.Now().After(deadline) {
			t.Errorf("日志中未出现 %q:\n%s", substr, out)
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
}
