// Package pipeline streams mapped samples from adapters to the muxer through
// a bounded queue. Memory is bounded by both the number of in-flight samples
// and the number of bytes they occupy; a slow consumer blocks the producer
// (backpressure) instead of growing an unbounded buffer.
package pipeline

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"mediacore/internal/media"
)

// byteUnit is the granularity of the byte budget semaphore. A 64 MiB budget
// becomes 65536 tokens, which is cheap to allocate once per queue. The unit
// is deliberately small so the observed peak stays close to the configured
// budget without ever exceeding it.
const byteUnit = int64(1 << 10)

// Limits bound the queue.
type Limits struct {
	// MaxSamples is the maximum number of samples in flight.
	MaxSamples int
	// MaxBytes is the maximum total payload bytes in flight, including the
	// sample currently being delivered to the consumer.
	MaxBytes int64
	// ReadChunk is the size of each source read.
	ReadChunk int
}

// Sample is one queued item. Data is a view into a pooled buffer valid only
// for the duration of the consumer callback.
type Sample struct {
	Track    *media.Track
	Index    int
	Meta     media.Sample
	Data     []byte
	Checksum [32]byte
}

// Stats reports what the queue observed.
type Stats struct {
	MaxQueueBytes int64
	MaxQueueDepth int
	Enqueued      int
	Dequeued      int
	ReadBytes     int64
	Canceled      bool
}

// Queue is a bounded, cancellable producer/consumer channel.
type Queue struct {
	src    media.Source
	limits Limits
	ch     chan *Sample
	pool   sync.Pool
	tokens chan struct{}

	inflightBytes atomic.Int64
	queuedBytes   atomic.Int64
	peakBytes     atomic.Int64
	peakDepth     atomic.Int32
	enqueued      atomic.Int64
	dequeued      atomic.Int64
	readBytes     atomic.Int64
	canceled      atomic.Bool

	closeOnce sync.Once
	mu        sync.Mutex
	err       error
}

// NewQueue constructs a queue with pool buffers and a byte-token semaphore.
func NewQueue(src media.Source, limits Limits) *Queue {
	if limits.MaxSamples <= 0 {
		limits.MaxSamples = 64
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = 64 << 20
	}
	if limits.ReadChunk <= 0 {
		limits.ReadChunk = 64 << 10
	}
	if int64(limits.ReadChunk) > limits.MaxBytes {
		limits.ReadChunk = int(limits.MaxBytes)
	}
	q := &Queue{
		src:    src,
		limits: limits,
		ch:     make(chan *Sample, limits.MaxSamples),
		tokens: make(chan struct{}, maxInt(1, int(limits.MaxBytes/byteUnit))),
	}
	q.pool.New = func() any {
		return &Sample{Data: make([]byte, 0, limits.ReadChunk)}
	}
	return q
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Limits returns the effective limits, including defaults.
func (q *Queue) Limits() Limits { return q.limits }

// tokensFor returns how many semaphore tokens a payload of size bytes needs.
func tokensFor(size int64) int {
	n := int((size + byteUnit - 1) / byteUnit)
	if n < 1 {
		return 1
	}
	return n
}

// acquireBytes blocks until the byte budget has room, or ctx is done.
func (q *Queue) acquireBytes(ctx context.Context, size int64) error {
	need := tokensFor(size)
	got := 0
	for got < need {
		select {
		case q.tokens <- struct{}{}:
			got++
		case <-ctx.Done():
			q.releaseTokens(got)
			return media.NewError(media.ClassCancelled, "cancelled while waiting for queue capacity").
				WithCause(ctx.Err())
		}
	}
	return nil
}

func (q *Queue) releaseTokens(n int) {
	for i := 0; i < n; i++ {
		select {
		case <-q.tokens:
		default:
			return
		}
	}
}

// Push queues sample i of track t, blocking while the queue is full or the
// byte budget is exhausted. Cancellation is observed while blocked.
func (q *Queue) Push(ctx context.Context, t *media.Track, i int) error {
	if err := ctx.Err(); err != nil {
		q.fail(media.NewError(media.ClassCancelled, "push cancelled").WithCause(err))
		return q.Err()
	}
	meta, err := t.Index.At(i)
	if err != nil {
		return q.fail(media.WithTrackContext(err, t.ID.Container, t.ID.Value))
	}
	if meta.Size < 0 {
		return q.fail(media.NewError(media.ClassInvalidLength,
			"sample %d of track %s declares negative size %d", i, t.ID, meta.Size).
			WithContainer(t.ID.Container).WithTrack(t.ID.Value).WithOffset(meta.Offset))
	}
	if meta.Size > q.limits.MaxBytes {
		return q.fail(media.NewError(media.ClassInvalidLength,
			"sample %d of track %s is %d bytes, above the %d-byte queue budget",
			i, t.ID, meta.Size, q.limits.MaxBytes).
			WithContainer(t.ID.Container).WithTrack(t.ID.Value).WithOffset(meta.Offset))
	}

	if err := q.acquireBytes(ctx, meta.Size); err != nil {
		return q.fail(err)
	}

	s, err := q.load(ctx, t, i, meta)
	if err != nil {
		q.releaseTokens(tokensFor(meta.Size))
		return err
	}

	q.inflightBytes.Add(meta.Size)
	select {
	case q.ch <- s:
		q.queuedBytes.Add(meta.Size)
		q.enqueued.Add(1)
		q.observe()
		return nil
	case <-ctx.Done():
		q.inflightBytes.Add(-meta.Size)
		q.releaseTokens(tokensFor(meta.Size))
		q.release(s)
		q.fail(media.NewError(media.ClassCancelled, "push cancelled while queue was full").WithCause(ctx.Err()))
		return q.Err()
	}
}

func (q *Queue) observe() {
	d := int32(len(q.ch))
	for {
		peak := q.peakDepth.Load()
		if d <= peak || q.peakDepth.CompareAndSwap(peak, d) {
			break
		}
	}
	b := q.inflightBytes.Load()
	for {
		peak := q.peakBytes.Load()
		if b <= peak || q.peakBytes.CompareAndSwap(peak, b) {
			break
		}
	}
}

// load reads one sample payload in bounded chunks and hashes it.
func (q *Queue) load(ctx context.Context, t *media.Track, i int, meta media.Sample) (*Sample, error) {
	s := q.pool.Get().(*Sample)
	s.Track, s.Index, s.Meta = t, i, meta
	need := int(meta.Size)
	if cap(s.Data) < need {
		s.Data = make([]byte, need)
	} else {
		s.Data = s.Data[:need]
	}
	read := 0
	for read < need {
		if err := ctx.Err(); err != nil {
			q.release(s)
			return nil, q.fail(media.NewError(media.ClassCancelled, "cancelled while reading a sample").
				WithCause(err))
		}
		chunk := q.limits.ReadChunk
		if chunk > need-read {
			chunk = need - read
		}
		n, err := q.src.ReadAt(s.Data[read:read+chunk], meta.Offset+int64(read))
		if n > 0 {
			read += n
			q.readBytes.Add(int64(n))
		}
		if err != nil && !(err == io.EOF && read == need) {
			q.release(s)
			return nil, q.fail(media.NewError(media.ClassTruncated,
				"sample %d of track %s ends past the input", i, t.ID).
				WithContainer(t.ID.Container).WithTrack(t.ID.Value).
				WithOffset(q.clampToInput(meta.Offset + int64(read))).WithTolerance(0).WithCause(err))
		}
		if n == 0 {
			q.release(s)
			return nil, q.fail(media.NewError(media.ClassTruncated,
				"source returned no bytes for sample %d of track %s", i, t.ID).
				WithContainer(t.ID.Container).WithTrack(t.ID.Value).
				WithOffset(q.clampToInput(meta.Offset + int64(read))).WithTolerance(0))
		}
	}
	s.Checksum = sha256.Sum256(s.Data)
	return s, nil
}

// clampToInput reports the first byte the input could not supply, never a
// position beyond the end of the source.
func (q *Queue) clampToInput(off int64) int64 {
	if size := q.src.Size(); size >= 0 && off > size {
		return size
	}
	return off
}

func (q *Queue) release(s *Sample) {
	s.Track, s.Index = nil, 0
	s.Data = s.Data[:0]
	q.pool.Put(s)
}

// Next delivers the next sample to fn. Data is valid only during fn. It
// returns io.EOF after a normal close and the sticky error otherwise.
func (q *Queue) Next(ctx context.Context, fn func(*Sample) error) error {
	select {
	case s, ok := <-q.ch:
		if !ok {
			if err := q.Err(); err != nil {
				return err
			}
			return io.EOF
		}
		q.queuedBytes.Add(-s.Meta.Size)
		q.dequeued.Add(1)
		err := fn(s)
		q.inflightBytes.Add(-s.Meta.Size)
		q.releaseTokens(tokensFor(s.Meta.Size))
		q.release(s)
		if err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		q.canceled.Store(true)
		q.fail(media.NewError(media.ClassCancelled, "consumer cancelled").WithCause(ctx.Err()))
		return q.Err()
	}
}

// Close marks the producer finished. It is idempotent.
func (q *Queue) Close() { q.closeOnce.Do(func() { close(q.ch) }) }

// Err returns the sticky error, if any.
func (q *Queue) Err() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.err
}

func (q *Queue) fail(err error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err == nil {
		q.err = err
	}
	return q.err
}

// Stats snapshots the queue's counters.
func (q *Queue) Stats() Stats {
	return Stats{
		MaxQueueBytes: q.peakBytes.Load(),
		MaxQueueDepth: int(q.peakDepth.Load()),
		Enqueued:      int(q.enqueued.Load()),
		Dequeued:      int(q.dequeued.Load()),
		ReadBytes:     q.readBytes.Load(),
		Canceled:      q.canceled.Load(),
	}
}

func (q *Queue) String() string {
	return fmt.Sprintf("queue depth=%d bytes=%d/%d", len(q.ch), q.inflightBytes.Load(), q.limits.MaxBytes)
}
