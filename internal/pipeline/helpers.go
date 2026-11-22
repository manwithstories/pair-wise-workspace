package pipeline

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"time"

	"mediapack/internal/cache"
	"mediapack/internal/codec"
)

// writeSeeker is an in-memory io.WriteSeeker backing fragment muxing.
type writeSeeker struct {
	buf []byte
	pos int64
}

func (w *writeSeeker) Write(p []byte) (int, error) {
	end := int(w.pos) + len(p)
	if end > len(w.buf) {
		grown := make([]byte, end)
		copy(grown, w.buf)
		w.buf = grown
	}
	copy(w.buf[w.pos:end], p)
	w.pos = int64(end)
	return len(p), nil
}

func (w *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = offset
	case io.SeekCurrent:
		w.pos += offset
	case io.SeekEnd:
		w.pos = int64(len(w.buf)) + offset
	}
	return w.pos, nil
}

func (w *writeSeeker) bytes() []byte { return w.buf }

// timeAfter is wrapped so cancellation tests can reason about the delay.
func timeAfter(d time.Duration) <-chan time.Time { return time.After(d) }

func sha256Bytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// cacheParse stores the parse summary under the content-addressed cache.
func (p *Pipeline) cacheParse(id cache.Identity, cp cachedParse,
	aus []codec.AccessUnit, meta *codec.StreamMeta) error {
	blob, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	if _, err := p.store.Put(id, blob); err != nil {
		return err
	}
	p.aus, p.meta = aus, meta
	p.parseCache = cp
	return nil
}
