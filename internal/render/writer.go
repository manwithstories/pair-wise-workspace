package render

import (
	"bytes"
	"os"
	"path/filepath"

	"mediasim/internal/clock"
)

// Writer writes frames to a directory and records their hashes. Re-running
// into the same directory overwrites byte-for-byte identical files.
type Writer struct {
	dir     string
	canvas  *Canvas
	hashes  []FrameHash
	written map[string]string
}

// NewWriter prepares dir and the canvas.
func NewWriter(dir string, p Params) (*Writer, error) {
	c, err := NewCanvas(p)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, clock.NewError(clock.CatRender, clock.Span{}, clock.RecoveredNone, "create output dir", err)
	}
	return &Writer{dir: dir, canvas: c, written: map[string]string{}}, nil
}

// Write composites f, writes its PNG and records the hash. It refuses to
// write a frame twice, which prevents a recovery pass from changing an
// already committed frame.
func (w *Writer) Write(f Frame) (FrameHash, error) {
	img, err := w.canvas.Composite(f)
	if err != nil {
		return FrameHash{}, err
	}
	name := frameName(f.Index)
	if prev, ok := w.written[name]; ok {
		// A repeated index is a bug in the plan, not a recoverable event:
		// commit-once semantics must not be silently violated.
		sp := clock.NewSpan("", "", clock.Interval{Start: f.Ticks, End: f.Ticks})
		sp.Frame = f.Index
		return FrameHash{}, clock.Errorf(clock.CatRender, sp,
			clock.RecoveredNone, "frame %d already committed as %s", f.Index, prev)
	}
	var buf bytes.Buffer
	if err := EncodePNG(&buf, img); err != nil {
		return FrameHash{}, err
	}
	path := filepath.Join(w.dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return FrameHash{}, clock.NewError(clock.CatRender, clock.Span{}, clock.RecoveredNone,
			"write frame "+name, err)
	}
	fh := FrameHash{
		Index: f.Index, Ticks: f.Ticks,
		SHA256: HashImage(img), PNG: name, PNGBytes: buf.Len(),
	}
	w.written[name] = fh.SHA256
	w.hashes = append(w.hashes, fh)
	return fh, nil
}

// Hashes returns the recorded hashes in commit order.
func (w *Writer) Hashes() []FrameHash { return w.hashes }

// Count returns how many frames have been committed.
func (w *Writer) Count() int { return len(w.hashes) }

func frameName(idx int) string {
	return "frame_" + pad(idx) + ".png"
}

func pad(n int) string {
	const digits = 4
	s := []byte("0000")
	v := n
	for i := digits - 1; i >= 0 && v > 0; i-- {
		s[i] = byte('0' + v%10)
		v /= 10
	}
	return string(s)
}
