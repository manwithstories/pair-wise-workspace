// Package repack writes the valid segments of a stream to an archive
// file and records damaged zones as isolation entries. It never repairs
// damaged data silently: with PolicyIsolate (default) damaged packet
// ranges are recorded with their start/end packet numbers; with
// PolicyDrop they are explicitly dropped and the drop ranges reported.
// Per-segment SHA-256 over media payloads proves pre/post-repack
// content identity.
package repack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"time"

	"tsbatch/internal/fault"
	"tsbatch/internal/inspect"
	"tsbatch/internal/ts"
)

// Policy selects how damaged zones are handled.
type Policy int

const (
	// PolicyIsolate keeps damaged zones as isolation records (default).
	PolicyIsolate Policy = iota
	// PolicyDrop explicitly drops damaged zones and reports the ranges.
	PolicyDrop
)

// SegmentInfo describes one archived segment.
type SegmentInfo struct {
	ID                      int
	FirstPacket, LastPacket uint64
	StartTime, EndTime      int64 // unified 90 kHz media time
	Packets                 int
	SHA256                  string // hex, over concatenated media payloads
}

// IsolationRecord describes one damaged zone.
type IsolationRecord struct {
	StartPacket, EndPacket uint64
	Reason                 fault.Category
	Dropped                bool
}

// Writer is an inspect.Sink that archives valid segments.
type Writer struct {
	out    io.Writer
	policy Policy
	// DelayPerPacket simulates a slow consumer (backpressure tests).
	DelayPerPacket time.Duration
	// PSIHook returns packets (e.g. fresh PAT/PMT) written at the start
	// of every segment; may be nil.
	PSIHook func(segID int) [][ts.PacketSize]byte

	hash       hash.Hash
	cur        SegmentInfo
	open       bool
	err        error
	Segments   []SegmentInfo
	Isolations []IsolationRecord
}

// New returns a Writer archiving to w.
func New(w io.Writer, policy Policy) *Writer {
	return &Writer{out: w, policy: policy}
}

// Err returns the first write failure, if any.
func (w *Writer) Err() error { return w.err }

func (w *Writer) BeginSegment(id int, firstPacket uint64, t int64) {
	if w.err != nil {
		return
	}
	w.cur = SegmentInfo{ID: id, FirstPacket: firstPacket, StartTime: t}
	w.hash = sha256.New()
	w.open = true
	if w.PSIHook != nil {
		for _, pkt := range w.PSIHook(id) {
			if _, w.err = w.out.Write(pkt[:]); w.err != nil {
				return
			}
		}
	}
}

func (w *Writer) Append(ev inspect.PacketEvent) {
	if w.err != nil || !w.open {
		return
	}
	if w.DelayPerPacket > 0 {
		time.Sleep(w.DelayPerPacket)
	}
	if _, w.err = w.out.Write(ev.Raw[:]); w.err != nil {
		return
	}
	h := ev.Header
	if ev.Media && h.PayloadOff >= 0 {
		w.hash.Write(ev.Raw[h.PayloadOff:])
	}
	w.cur.Packets++
}

func (w *Writer) EndSegment(id int, lastPacket uint64, t int64) {
	if !w.open {
		return
	}
	w.open = false
	w.cur.LastPacket = lastPacket
	w.cur.EndTime = t
	w.cur.SHA256 = hex.EncodeToString(w.hash.Sum(nil))
	w.Segments = append(w.Segments, w.cur)
}

func (w *Writer) Isolate(start, end uint64, reason fault.Category) {
	w.Isolations = append(w.Isolations, IsolationRecord{
		StartPacket: start, EndPacket: end,
		Reason: reason, Dropped: w.policy == PolicyDrop,
	})
}

// FileWriter archives to finalPath via a securely created temp file that
// is renamed on success and removed on failure. It refuses to overwrite
// the input file.
type FileWriter struct {
	*Writer
	tmp   *os.File
	final string
}

// Create opens a FileWriter; inputPath must differ from finalPath.
func Create(finalPath, inputPath string, policy Policy) (*FileWriter, error) {
	if finalPath == inputPath {
		return nil, errors.New("repack: output must not overwrite input")
	}
	tmp, err := os.CreateTemp("", "tsbatch-*.ts")
	if err != nil {
		return nil, err
	}
	return &FileWriter{Writer: New(tmp, policy), tmp: tmp, final: finalPath}, nil
}

// Close finalizes the archive: on any prior error the temp file is
// removed; otherwise it is atomically renamed to the final path.
func (f *FileWriter) Close() error {
	name := f.tmp.Name()
	if f.err != nil {
		f.tmp.Close()
		os.Remove(name)
		return f.err
	}
	if err := f.tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, f.final); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Abort removes the temp file (cancellation path).
func (f *FileWriter) Abort() {
	name := f.tmp.Name()
	f.tmp.Close()
	os.Remove(name)
}

// Verify re-parses an archive produced by Writer and independently
// recomputes per-segment payload hashes and keyframe hits. mediaPIDs
// selects which PIDs carry elementary-stream payload; psiPIDs are
// skipped (regenerated tables). segmentPacketCounts must match the
// archive layout.
func Verify(r io.Reader, segSizes []int, mediaPIDs map[uint16]bool) (hashes []string, keyframes int, err error) {
	rd, err := ts.NewReader(r, ts.PacketSize)
	if err != nil {
		return nil, 0, err
	}
	var pkt [ts.PacketSize]byte
	seg := 0
	h := sha256.New()
	flush := func() {
		hashes = append(hashes, hex.EncodeToString(h.Sum(nil)))
		h = sha256.New()
	}
	count := 0
	for {
		_, rerr := rd.Next(&pkt)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, 0, fmt.Errorf("verify: %w", rerr)
		}
		hdr, perr := ts.ParseHeader(&pkt)
		if perr != nil {
			return nil, 0, perr
		}
		if !mediaPIDs[hdr.PID] {
			continue // regenerated PSI or null packets
		}
		if seg < len(segSizes) && count == segSizes[seg] {
			flush()
			seg++
			count = 0
		}
		if hdr.RandomAccess && hdr.PUSI {
			keyframes++
		}
		if hdr.PayloadOff >= 0 {
			h.Write(pkt[hdr.PayloadOff:])
		}
		count++
	}
	if count > 0 || len(hashes) < len(segSizes) {
		flush()
	}
	return hashes, keyframes, nil
}
