package canonical

import (
	"io"
	"sync"
	"time"
)

type idleTimeoutReader struct {
	r       io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	mu      sync.Mutex
	closed  bool
}

// NewIdleTimeoutReader wraps an io.ReadCloser with an idle timeout.
// If no data is read for the specified timeout duration, the underlying ReadCloser is closed.
// A timeout of <= 0 disables the idle timeout.
func NewIdleTimeoutReader(r io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return r
	}
	tr := &idleTimeoutReader{
		r:       r,
		timeout: timeout,
	}
	tr.timer = time.AfterFunc(timeout, func() {
		tr.mu.Lock()
		tr.closed = true
		tr.mu.Unlock()
		_ = r.Close()
	})
	return tr
}

func (tr *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	if n > 0 {
		tr.mu.Lock()
		if !tr.closed && tr.timer != nil {
			tr.timer.Reset(tr.timeout)
		}
		tr.mu.Unlock()
	}
	return n, err
}

func (tr *idleTimeoutReader) Close() error {
	tr.mu.Lock()
	tr.closed = true
	if tr.timer != nil {
		tr.timer.Stop()
	}
	tr.mu.Unlock()
	return tr.r.Close()
}
