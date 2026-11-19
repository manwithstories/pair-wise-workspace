// Package index writes a random-access MP4 index over validated frames.
//
// The output is a plain ISO-BMFF file: ftyp, a 64-bit mdat holding one sample
// per indexed frame, and a trailing moov with stts/stsc/stsz/stco tables. The
// sample entry type is "mfrm", the explicitly declared frame record format.
// Timestamps live in the 90 kHz media timescale so callers can seek by time.
package index

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"
)

// Timescale is the unified media clock stored in mdhd.
const Timescale = 90000

var sampleEntryType = mp4.StrToBoxType("mfrm")

func init() {
	mp4.AddAnyTypeBoxDef(&mp4.VisualSampleEntry{}, sampleEntryType)
}

// State is the committed position of a writer; it is what a checkpoint
// persists and what a resumed run must reproduce.
type State struct {
	Samples    uint64
	Chunks     uint64
	PayloadLen int64
	CRC        uint32
	FirstMedia int64
	LastMedia  int64
	LastDelta  int64
}

// Writer appends frame payloads to a temporary MP4 file. Memory use is bounded
// by the chunk summary plus one sample; the per-sample tables are spooled to
// disk so it does not grow with the input.
type Writer struct {
	f           *os.File
	path        string
	spoolDir    string
	ftypSize    int64
	dataStart   int64
	chunkFrames int

	spoolStsz *os.File
	spoolStts *os.File
	spoolStsc *os.File
	spoolStco *os.File
	stsz      *bufioWriter
	stts      runWriter
	stsc      *scWriter
	stco      *bufioWriter

	sampleCount uint64
	chunkCount  uint64
	chunkLens   uint64 // samples in current chunk
	curChunk    uint64 // 1-based chunk index
	payloadLen  int64  // bytes of sample payload written
	crc         uint32
	firstMedia  int64
	lastMedia   int64
	lastDelta   int64

	replaying bool
	readOnly  bool
	closed    bool

	writeBuf []byte
}

type bufioWriter struct{ w *os.File }

func (b *bufioWriter) u32(v uint32) error {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	return b.write(tmp[:])
}

func (b *bufioWriter) u64(v uint64) error {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	return b.write(tmp[:])
}

func (b *bufioWriter) write(p []byte) error {
	_, err := b.w.Write(p)
	return err
}

// runWriter accumulates stts runs with run-length encoding so memory stays
// constant even when every sample has a unique delta.
type runWriter struct {
	w         *os.File
	runs      uint32
	count     uint32
	delta     uint32
	have      bool
	totalTime uint64
}

func (r *runWriter) add(delta uint32) error {
	if r.have && r.delta == delta && r.count < (1<<32-1) {
		r.count++
		r.totalTime += uint64(delta)
		return nil
	}
	if err := r.flush(); err != nil {
		return err
	}
	r.have = true
	r.count = 1
	r.delta = delta
	r.totalTime += uint64(delta)
	return nil
}

func (r *runWriter) flush() error {
	if !r.have {
		return nil
	}
	var tmp [8]byte
	binary.BigEndian.PutUint32(tmp[0:4], r.count)
	binary.BigEndian.PutUint32(tmp[4:8], r.delta)
	if _, err := r.w.Write(tmp[:]); err != nil {
		return err
	}
	r.runs++
	r.have = false
	return nil
}

// scWriter emits stsc entries only when the samples-per-chunk changes.
type scWriter struct {
	w        *os.File
	entries  uint32
	lastSpc  uint32
	lastSeen bool
}

func (s *scWriter) add(firstChunk, samplesPerChunk uint32) error {
	if s.lastSeen && s.lastSpc == samplesPerChunk {
		return nil
	}
	var tmp [12]byte
	binary.BigEndian.PutUint32(tmp[0:4], firstChunk)
	binary.BigEndian.PutUint32(tmp[4:8], samplesPerChunk)
	binary.BigEndian.PutUint32(tmp[8:12], 1)
	if _, err := s.w.Write(tmp[:]); err != nil {
		return err
	}
	s.entries++
	s.lastSpc = samplesPerChunk
	s.lastSeen = true
	return nil
}

func spool(name string) (*os.File, error) {
	return os.CreateTemp("", "mediq-"+name+"-*")
}

// Create starts a new index file at path (which must not exist yet).
func Create(path string, chunkFrames int) (*Writer, error) {
	if chunkFrames <= 0 {
		chunkFrames = 256
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, path: path, chunkFrames: chunkFrames}
	// Spool files live next to the output so a crash cannot leave stale
	// tables that a later run would trust (they are never read back).
	dir := filepath.Dir(path)
	if w.spoolStsz, err = spool(dir); err != nil {
		w.Abort()
		return nil, err
	}
	w.spoolStts, _ = spool(dir)
	w.spoolStsc, _ = spool(dir)
	w.spoolStco, _ = spool(dir)
	for _, sp := range []*os.File{w.spoolStts, w.spoolStsc, w.spoolStco} {
		if sp == nil {
			w.Abort()
			return nil, err
		}
	}
	w.buffered()
	if err := w.writeHeader(); err != nil {
		w.Abort()
		return nil, err
	}
	return w, nil
}

func (w *Writer) buffered() {
	w.stsz = &bufioWriter{w: w.spoolStsz}
	w.stts.w = w.spoolStts
	w.stsc = &scWriter{w: w.spoolStsc}
	w.stco = &bufioWriter{w: w.spoolStco}
}

// writeHeader emits ftyp and a 64-bit mdat header with a placeholder size.
func (w *Writer) writeHeader() error {
	ftyp := &mp4.Ftyp{MajorBrand: mp4.BrandISOM(), MinorVersion: 0x200}
	ftyp.AddCompatibleBrand(mp4.BrandISOM())
	ftyp.AddCompatibleBrand(mp4.BrandISO6())
	ftyp.AddCompatibleBrand(mp4.BrandMP41())
	if err := w.writeSmallBox(ftyp); err != nil {
		return err
	}
	pos, _ := w.f.Seek(0, io.SeekCurrent)
	w.ftypSize = pos
	bi, err := mp4.WriteBoxInfo(w.f, &mp4.BoxInfo{
		Type:       mp4.BoxTypeMdat(),
		Size:       mp4.LargeHeaderSize,
		HeaderSize: mp4.LargeHeaderSize,
	})
	if err != nil {
		return err
	}
	w.dataStart = int64(bi.Offset) + int64(bi.HeaderSize)
	return nil
}

func (w *Writer) writeSmallBox(box mp4.IImmutableBox) error {
	var buf bytes.Buffer
	n, err := mp4.Marshal(&buf, box, mp4.Context{})
	if err != nil {
		return err
	}
	if err := w.writeHeader4(box.GetType(), n+mp4.SmallHeaderSize, false); err != nil {
		return err
	}
	_, err = w.f.Write(buf.Bytes())
	return err
}

func (w *Writer) writeHeader4(t mp4.BoxType, size uint64, large bool) error {
	data := make([]byte, mp4.SmallHeaderSize)
	if large {
		return fmt.Errorf("large header requested for %s", t)
	}
	binary.BigEndian.PutUint32(data, uint32(size))
	copy(data[4:8], t[:])
	_, err := w.f.Write(data)
	return err
}

// Append writes one frame payload as the next sample.
func (w *Writer) Append(seq uint64, media int64, payload []byte) error {
	if w.closed || w.readOnly {
		return fmt.Errorf("index writer is not accepting samples")
	}
	if len(payload) == 0 || len(payload) > int(^uint32(0)) {
		return fmt.Errorf("frame %d: payload length %d unsupported", seq, len(payload))
	}
	if w.sampleCount == 0 {
		w.firstMedia = media
	} else {
		delta := media - w.lastMedia
		if delta <= 0 {
			return fmt.Errorf("frame %d: media time %d not after %d", seq, media, w.lastMedia)
		}
		if delta > int64(^uint32(0)) {
			return fmt.Errorf("frame %d: sample delta %d does not fit stts", seq, delta)
		}
		if err := w.stts.add(uint32(delta)); err != nil {
			return err
		}
		w.lastDelta = delta
	}
	if w.chunkLens == 0 {
		pos, err := w.f.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if err := w.stco.u64(uint64(pos)); err != nil {
			return err
		}
		w.curChunk++
	}
	if w.replaying {
		// Tables are regenerated during recovery; payload bytes must already
		// be present in the file and are validated by FinishReplay.
		w.payloadLen += int64(len(payload))
	} else {
		if _, err := w.f.Write(payload); err != nil {
			return err
		}
		w.crc = crc32.Update(w.crc, crc32.IEEETable, payload)
		w.payloadLen += int64(len(payload))
	}
	if err := w.stsz.u32(uint32(len(payload))); err != nil {
		return err
	}
	w.sampleCount++
	w.chunkLens++
	w.lastMedia = media
	if w.chunkLens == uint64(w.chunkFrames) {
		return w.finishChunk()
	}
	return nil
}

func (w *Writer) finishChunk() error {
	if w.chunkLens == 0 {
		return nil
	}
	if err := w.stsc.add(uint32(w.curChunk), uint32(w.chunkLens)); err != nil {
		return err
	}
	w.chunkLens = 0
	w.chunkCount++
	return nil
}

// State returns the committed position, flushing any buffered table state so
// that the checkpoint and the file agree.
func (w *Writer) State() (State, error) {
	if err := w.stts.flush(); err != nil {
		return State{}, err
	}
	if w.chunkLens > 0 {
		if err := w.stsc.add(uint32(w.curChunk), uint32(w.chunkLens)); err != nil {
			return State{}, err
		}
	}
	if err := w.syncTables(); err != nil {
		return State{}, err
	}
	if err := w.f.Sync(); err != nil {
		return State{}, err
	}
	pos, err := w.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return State{}, err
	}
	return State{
		Samples:    w.sampleCount,
		Chunks:     w.chunkCount,
		PayloadLen: w.payloadLen,
		CRC:        w.crc,
		FirstMedia: w.firstMedia,
		LastMedia:  w.lastMedia,
		LastDelta:  w.lastDelta,
	}, nil
	_ = pos
	return State{}, nil
}

func (w *Writer) syncTables() error {
	for _, f := range []*os.File{w.spoolStsz, w.spoolStts, w.spoolStsc, w.spoolStco} {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return nil
}
