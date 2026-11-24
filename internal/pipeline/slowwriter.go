package pipeline

import (
	"bytes"
	"sync"
	"time"

	"mediadeliver/internal/model"
)

// SlowWriter models an archive disk: it sleeps before every write to create
// back-pressure, and can inject a recoverable failure on a chosen write index.
// All failures surface as model.KindIO so the pipeline can exercise retry.
type SlowWriter struct {
	mu sync.Mutex

	delay time.Duration
	// failOnWrite (1-based) makes the Nth Write return an error once; 0 disables.
	failOnWrite int
	writes      int
	failedOnce  bool

	buf    bytes.Buffer
	closed bool
}

// NewSlowWriter builds a writer with a per-write delay and an optional
// 1-based failure index.
func NewSlowWriter(delay time.Duration, failOnWrite int) *SlowWriter {
	return &SlowWriter{delay: delay, failOnWrite: failOnWrite}
}

func (w *SlowWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	idx := w.writes
	inject := w.failOnWrite > 0 && idx == w.failOnWrite && !w.failedOnce
	if inject {
		w.failedOnce = true
	}
	delay := w.delay
	w.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if inject {
		return 0, model.At(model.KindIO, "pipeline", "slow write", int64(idx),
			"injected recoverable archive write failure")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// Bytes returns everything successfully written so far.
func (w *SlowWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// Len returns the number of bytes durably written.
func (w *SlowWriter) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}
