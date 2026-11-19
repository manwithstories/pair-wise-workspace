// Package index writes a random-access MP4 index over validated frames.
//
// The output is a plain ISO-BMFF file: ftyp, a 64-bit mdat holding one sample
// per indexed frame, and a trailing moov with stts/stsc/stsz/stco(co64)
// tables. Samples use the explicitly declared frame record format "mfrm".
// The media timescale is the unified 90 kHz clock, so any player or tool can
// seek by timestamp. The file is assembled at path+".part" and only renamed
// into place once the index is complete.
package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"
)

// Timescale is the media clock stored in mdhd and used by stts.
const Timescale = 90000

var sampleEntryType = mp4.StrToBoxType("mfrm")

// frameEntry is the sample description for one indexed frame. It deliberately
// carries no codec configuration: the samples are raw validated frame records.
type frameEntry struct {
	mp4.SampleEntry `mp4:"0,extend"`
	Width           uint16 `mp4:"1,size=16"`
	Height          uint16 `mp4:"2,size=16"`
}

func init() {
	mp4.AddAnyTypeBoxDef(&frameEntry{}, sampleEntryType)
}

// State is the committed position of a writer. A checkpoint persists it and
// a resumed run must reproduce it exactly.
type State struct {
	Samples    uint64
	Chunks     uint64
	PayloadLen int64
	PayloadCRC uint32
	FirstMedia int64
	LastMedia  int64
	LastDelta  int64
	// LastSeq is the frame sequence number of the most recent sample, so a
	// resumed writer can continue the chunk summary correctly.
	LastSeq      uint64
	TableSizes   TableSizes
	CurrentChunk uint64 // chunk index carrying an open partial chunk
	ChunkSamples uint64 // samples in the open partial chunk
}

// TableSizes records the exact committed length of every spooled table.
type TableSizes struct {
	Stsz int64
	Stts int64
	Stsc int64
	Stco int64
}

// Writer appends frame payloads and spools the sample tables to disk, so the
// process footprint stays constant no matter how many frames are indexed.
type Writer struct {
	data        *os.File
	dataPath    string
	tblDir      string
	chunkFrames int

	stsz *os.File
	stts *os.File
	stsc *os.File
	stco *os.File

	sampleCount uint64
	chunkCount  uint64
	chunkLens   uint64
	curChunk    uint64
	payloadLen  int64
	crc         uint32
	firstMedia  int64
	lastMedia   int64
	lastDelta   int64
	lastSeq     uint64

	runCount      uint32
	runDelta      uint32
	runOpen       bool
	firstDeltaSet bool

	lastSPC              uint32
	haveSPC              bool
	finished             bool
	headerEnd            int64 // offset of the first sample payload
	lastClosedPayloadLen int64
	crcBuf               []byte

	chunkFirstSeq uint64
	chunkLastSeq  uint64
	chunkCRC      uint32
	chunkSamples  uint64
}

// Create starts a fresh index at path; no output file is published until
// Close. The part file must not already exist.
func Create(path, tblDir string, chunkFrames int) (*Writer, error) {
	if err := os.MkdirAll(tblDir, 0o755); err != nil {
		return nil, err
	}
	part := path + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{data: f, dataPath: part, tblDir: tblDir, chunkFrames: chunkFrames}
	if err := w.openTables(true); err != nil {
		w.Abort()
		return nil, err
	}
	if err := w.writeFileHeader(); err != nil {
		w.Abort()
		return nil, err
	}
	return w, nil
}

// Resume reopens a part file and its tables at a committed state. Payload
// bytes already committed are checksum-verified and tables truncated to the
// recorded sizes, dropping any uncommitted tail left by a crash.
func Resume(path, tblDir string, chunkFrames int, st State) (*Writer, error) {
	part := path + ".part"
	f, err := os.OpenFile(part, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{data: f, dataPath: part, tblDir: tblDir, chunkFrames: chunkFrames}
	if err := w.openTables(false); err != nil {
		w.Abort()
		return nil, err
	}
	if err := w.verifyCommitted(st); err != nil {
		w.Abort()
		return nil, err
	}
	start, err := w.dataStart()
	if err != nil {
		return nil, err
	}
	w.headerEnd = start
	w.sampleCount = st.Samples
	w.chunkCount = st.Chunks
	w.payloadLen = st.PayloadLen
	w.lastClosedPayloadLen = st.PayloadLen
	w.crc = st.PayloadCRC
	w.firstMedia = st.FirstMedia
	w.lastMedia = st.LastMedia
	w.lastDelta = st.LastDelta
	w.lastSeq = st.LastSeq
	w.curChunk = st.CurrentChunk
	w.chunkLens = st.ChunkSamples
	w.firstDeltaSet = st.Samples > 1
	if w.chunkLens != 0 {
		// A committed state always closes its chunk; opening one would mean
		// the checkpoint was written inconsistently.
		return nil, fmt.Errorf("checkpoint carries an open chunk (%d samples)", w.chunkLens)
	}
	return w, nil
}

func (w *Writer) openTables(truncate bool) error {
	names := []string{"stsz.bin", "stts.bin", "stsc.bin", "stco.bin"}
	files := make([]*os.File, len(names))
	for i, n := range names {
		flags := os.O_CREATE | os.O_RDWR
		if truncate {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(filepath.Join(w.tblDir, n), flags, 0o644)
		if err != nil {
			for _, opened := range files[:i] {
				opened.Close()
			}
			return err
		}
		files[i] = f
	}
	w.stsz, w.stts, w.stsc, w.stco = files[0], files[1], files[2], files[3]
	return nil
}

func (w *Writer) verifyCommitted(st State) error {
	for _, f := range []struct {
		file *os.File
		want int64
	}{
		{w.stsz, st.TableSizes.Stsz},
		{w.stts, st.TableSizes.Stts},
		{w.stsc, st.TableSizes.Stsc},
		{w.stco, st.TableSizes.Stco},
	} {
		fi, err := f.file.Stat()
		if err != nil {
			return err
		}
		if fi.Size() < f.want {
			return fmt.Errorf("index table %s truncated: %d < %d", f.file.Name(), fi.Size(), f.want)
		}
		if err := f.file.Truncate(f.want); err != nil {
			return err
		}
		if _, err := f.file.Seek(f.want, io.SeekStart); err != nil {
			return err
		}
	}
	fi, err := w.data.Stat()
	if err != nil {
		return err
	}
	start, err := w.dataStart()
	if err != nil {
		return err
	}
	if fi.Size() < start+st.PayloadLen {
		return fmt.Errorf("index payload truncated: %d < %d", fi.Size(), start+st.PayloadLen)
	}
	if err := w.data.Truncate(start + st.PayloadLen); err != nil {
		return err
	}
	// Verify the committed payload really is what the checkpoint describes.
	crc, err := w.crcRegion(start, st.PayloadLen)
	if err != nil {
		return err
	}
	if crc != st.PayloadCRC {
		return fmt.Errorf("index payload checksum mismatch: %08x != %08x", crc, st.PayloadCRC)
	}
	// Position the tables and the data file for appending.
	for _, f := range []*os.File{w.stsz, w.stts, w.stsc, w.stco} {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
	}
	if _, err := w.data.Seek(start+st.PayloadLen, io.SeekStart); err != nil {
		return err
	}
	// Recover run-length and stsc state from the spooled tails so appends
	// continue the tables rather than corrupting them.
	if err := w.reloadTableState(st); err != nil {
		return err
	}
	return nil
}

func (w *Writer) reloadTableState(st State) error {
	if st.TableSizes.Stts > 0 {
		var tail [8]byte
		if _, err := w.stts.ReadAt(tail[:], st.TableSizes.Stts-8); err != nil {
			return err
		}
		// Keep the last run's delta for heuristic continuity but leave the run
		// closed: it is already on disk and appending a fresh run keeps the
		// resulting stts identical to an uninterrupted run.
		w.runDelta = binary.BigEndian.Uint32(tail[4:8])
		w.runOpen = false
	}
	if st.TableSizes.Stsc > 0 {
		var tail [12]byte
		if _, err := w.stsc.ReadAt(tail[:], st.TableSizes.Stsc-12); err != nil {
			return err
		}
		w.haveSPC = true
		w.lastSPC = binary.BigEndian.Uint32(tail[4:8])
	}
	return nil
}

// dataStart returns the offset of the first sample payload, derived from the
// ftyp box already on disk.
func (w *Writer) dataStart() (int64, error) {
	var hdr [8]byte
	if _, err := w.data.ReadAt(hdr[:], 0); err != nil {
		return 0, err
	}
	ftypSize := int64(binary.BigEndian.Uint32(hdr[0:4]))
	if string(hdr[4:8]) != "ftyp" || ftypSize < 8 {
		return 0, fmt.Errorf("index file has no usable ftyp box")
	}
	return ftypSize + int64(mp4.LargeHeaderSize), nil
}

// writeFileHeader emits ftyp followed by a large-header mdat whose size is
// patched in Close.
func (w *Writer) writeFileHeader() error {
	ftyp := &mp4.Ftyp{MajorBrand: mp4.BrandISOM(), MinorVersion: 0x200}
	ftyp.AddCompatibleBrand(mp4.BrandISOM())
	ftyp.AddCompatibleBrand(mp4.BrandISO6())
	ftyp.AddCompatibleBrand(mp4.BrandMP41())
	var buf bytes.Buffer
	n, err := mp4.Marshal(&buf, ftyp, mp4.Context{})
	if err != nil {
		return err
	}
	if n+8 > 1<<32 {
		return fmt.Errorf("ftyp too large")
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[0:4], uint32(n+8))
	ftypType := mp4.BoxTypeFtyp()
	copy(hdr[4:8], ftypType[:])
	if _, err := w.data.Write(append(hdr, buf.Bytes()...)); err != nil {
		return err
	}
	mdatHdr := make([]byte, mp4.LargeHeaderSize)
	binary.BigEndian.PutUint32(mdatHdr[0:4], 1)
	mdatHdr[4] = 'm'
	mdatHdr[5] = 'd'
	mdatHdr[6] = 'a'
	mdatHdr[7] = 't'
	// Size patched at Close; write 0 now so a torn part never looks valid.
	if _, err := w.data.Write(mdatHdr); err != nil {
		return err
	}
	pos, err := w.data.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	w.headerEnd = pos
	return w.data.Sync()
}

// Append writes one frame payload as the next sample. media is absolute 90 kHz
// ticks and must strictly increase across calls.
// ChunkSummary describes the chunk just closed by CloseChunk.
type ChunkSummary struct {
	Index      uint32
	FirstFrame uint64
	LastFrame  uint64
	Samples    uint32
	Checksum   uint32
}

// VerifyLastChunk re-reads the just-committed chunk payload from disk and
// compares its CRC with the summary. It catches a short write or a bad sector
// before the chunk is advertised as recoverable in a checkpoint.
func (w *Writer) VerifyLastChunk(sum ChunkSummary, st State) error {
	chunkBytes := st.PayloadLen - w.lastClosedPayloadLen
	if chunkBytes < 0 {
		return fmt.Errorf("chunk %d payload length went backwards", sum.Index)
	}
	crc, err := w.crcRegion(w.headerEnd+w.lastClosedPayloadLen, chunkBytes)
	if err != nil {
		return err
	}
	if crc != sum.Checksum {
		return fmt.Errorf("chunk %d checksum %08x does not match payload on disk %08x", sum.Index, sum.Checksum, crc)
	}
	w.lastClosedPayloadLen = st.PayloadLen
	// Verification moves the read cursor; put it back at the append point.
	_, err = w.data.Seek(w.headerEnd+st.PayloadLen, io.SeekStart)
	return err
}

// crcRegion streams a file region through CRC-32 using a reusable buffer so
// per-chunk verification does not allocate.
func (w *Writer) crcRegion(off, n int64) (uint32, error) {
	if n == 0 {
		return 0, nil
	}
	if w.crcBuf == nil {
		w.crcBuf = make([]byte, 64<<10)
	}
	if _, err := w.data.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	var crc uint32
	for remaining := n; remaining > 0; {
		chunk := int64(len(w.crcBuf))
		if remaining < chunk {
			chunk = remaining
		}
		if _, err := io.ReadFull(w.data, w.crcBuf[:chunk]); err != nil {
			return 0, err
		}
		crc = crc32.Update(crc, crc32.IEEETable, w.crcBuf[:chunk])
		remaining -= chunk
	}
	return crc, nil
}

// CloseChunk closes the current chunk, flushes and syncs every file so the
// index is crash-consistent at this boundary, and reports the chunk summary.
// It returns (nil, nil) when no samples are pending since the last close.
func (w *Writer) CloseChunk() (*ChunkSummary, error) {
	if w.chunkLens == 0 {
		return nil, nil
	}
	if _, err := w.Commit(); err != nil {
		return nil, err
	}
	sum := &ChunkSummary{
		Index:      uint32(w.chunkCount - 1),
		FirstFrame: w.chunkFirstSeq,
		LastFrame:  w.chunkLastSeq,
		Samples:    uint32(w.chunkSamples),
		Checksum:   w.chunkCRC,
	}
	w.chunkCRC = 0
	w.chunkSamples = 0
	return sum, nil
}

func (w *Writer) Append(seq uint64, media int64, payload []byte) error {
	if len(payload) == 0 || uint64(len(payload)) > uint64(^uint32(0)) {
		return fmt.Errorf("payload length %d unsupported", len(payload))
	}
	if w.sampleCount == 0 {
		w.firstMedia = media
	} else {
		delta := media - w.lastMedia
		if delta <= 0 {
			return fmt.Errorf("media time %d not after %d", media, w.lastMedia)
		}
		if delta > int64(^uint32(0)) {
			return fmt.Errorf("sample delta %d does not fit stts", delta)
		}
		if !w.firstDeltaSet {
			// The first sample has no predecessor; give it the same duration
			// as the second so stts covers every sample exactly once and the
			// reported duration is sensible.
			if err := w.addDelta(uint32(delta)); err != nil {
				return err
			}
			w.firstDeltaSet = true
		}
		if err := w.addDelta(uint32(delta)); err != nil {
			return err
		}
	}
	if w.chunkLens == 0 {
		pos, err := w.data.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if err := w.writeU64(w.stco, uint64(pos)); err != nil {
			return err
		}
		w.curChunk++
	}
	if _, err := w.data.Write(payload); err != nil {
		return err
	}
	w.crc = crc32.Update(w.crc, crc32.IEEETable, payload)
	w.payloadLen += int64(len(payload))
	if err := w.writeU32(w.stsz, uint32(len(payload))); err != nil {
		return err
	}
	if w.chunkLens == 0 {
		w.chunkFirstSeq = seq
	}
	w.chunkLastSeq = seq
	w.chunkSamples++
	w.chunkCRC = crc32.Update(w.chunkCRC, crc32.IEEETable, payload)
	w.sampleCount++
	w.chunkLens++
	w.lastMedia = media
	w.lastSeq = seq
	return nil
}

func (w *Writer) addDelta(d uint32) error {
	if w.runOpen && w.runDelta == d && w.runCount < ^uint32(0) {
		w.runCount++
		return nil
	}
	if err := w.flushRun(); err != nil {
		return err
	}
	w.runOpen = true
	w.runCount = 1
	w.runDelta = d
	w.lastDelta = int64(d)
	return nil
}

func (w *Writer) flushRun() error {
	if !w.runOpen {
		return nil
	}
	var tmp [8]byte
	binary.BigEndian.PutUint32(tmp[0:4], w.runCount)
	binary.BigEndian.PutUint32(tmp[4:8], w.runDelta)
	if _, err := w.stts.Write(tmp[:]); err != nil {
		return err
	}
	w.runOpen = false
	return nil
}

func (w *Writer) finishChunk() error {
	if w.chunkLens == 0 {
		return nil
	}
	spc := uint32(w.chunkLens)
	if !w.haveSPC || w.lastSPC != spc {
		var tmp [12]byte
		binary.BigEndian.PutUint32(tmp[0:4], uint32(w.curChunk))
		binary.BigEndian.PutUint32(tmp[4:8], spc)
		binary.BigEndian.PutUint32(tmp[8:12], 1)
		if _, err := w.stsc.Write(tmp[:]); err != nil {
			return err
		}
		w.haveSPC = true
		w.lastSPC = spc
	}
	w.chunkLens = 0
	w.chunkCount++
	return nil
}

func (w *Writer) writeU32(f *os.File, v uint32) error {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	_, err := f.Write(tmp[:])
	return err
}

func (w *Writer) writeU64(f *os.File, v uint64) error {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	_, err := f.Write(tmp[:])
	return err
}

// Commit closes the open chunk, flushes tables and syncs everything to disk.
// The returned state is exactly what a checkpoint should persist.
func (w *Writer) Commit() (State, error) {
	if err := w.flushRun(); err != nil {
		return State{}, err
	}
	if err := w.finishChunk(); err != nil {
		return State{}, err
	}
	if err := w.data.Sync(); err != nil {
		return State{}, err
	}
	var sizes TableSizes
	for _, t := range []struct {
		f   *os.File
		dst *int64
	}{
		{w.stsz, &sizes.Stsz},
		{w.stts, &sizes.Stts},
		{w.stsc, &sizes.Stsc},
		{w.stco, &sizes.Stco},
	} {
		if err := t.f.Sync(); err != nil {
			return State{}, err
		}
		pos, err := t.f.Seek(0, io.SeekEnd)
		if err != nil {
			return State{}, err
		}
		*t.dst = pos
	}
	return State{
		Samples:      w.sampleCount,
		Chunks:       w.chunkCount,
		PayloadLen:   w.payloadLen,
		PayloadCRC:   w.crc,
		FirstMedia:   w.firstMedia,
		LastMedia:    w.lastMedia,
		LastDelta:    w.lastDelta,
		LastSeq:      w.lastSeq,
		TableSizes:   sizes,
		CurrentChunk: w.curChunk,
		ChunkSamples: w.chunkLens,
	}, nil
}

// CloseWithoutPublishing releases file handles and removes the temporary table
// spool while leaving the part file and committed checkpoint state intact, so
// an interrupted job can resume. It is the correct teardown for cancellation.
func (w *Writer) CloseWithoutPublishing() error {
	if w.data != nil {
		if err := w.data.Sync(); err != nil {
			return err
		}
		if err := w.data.Close(); err != nil {
			return err
		}
		w.data = nil
	}
	// Tables are deliberately kept: Resume truncates them to the sizes the
	// checkpoint recorded, dropping any uncommitted tail.
	for _, f := range []*os.File{w.stsz, w.stts, w.stsc, w.stco} {
		if f != nil {
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
	w.stsz, w.stts, w.stsc, w.stco = nil, nil, nil, nil
	return nil
}

// Abort discards the part file and tables. It is safe to call after Close.
func (w *Writer) Abort() {
	if w.data != nil {
		w.data.Close()
		w.data = nil
	}
	os.Remove(w.dataPath)
	for _, f := range []*os.File{w.stsz, w.stts, w.stsc, w.stco} {
		if f != nil {
			f.Close()
		}
	}
	w.stsz, w.stts, w.stsc, w.stco = nil, nil, nil, nil
	os.RemoveAll(w.tblDir)
}

// Finish writes the movie box and patches the mdat size, making the part file
// a complete, self-contained MP4. It does not rename it; call Publish.
//
// If a previous interrupted run already appended a moov to this part file,
// Finish truncates it before writing the new one. The moov always starts at
// headerEnd+PayloadLen, so the committed payload checksum stays valid.
func (w *Writer) Finish() error {
	if w.finished {
		return nil
	}
	if err := w.data.Truncate(w.headerEnd + w.payloadLen); err != nil {
		return err
	}
	st, err := w.Commit()
	if err != nil {
		return err
	}
	// The mdat payload ends where the moov begins; capture it before appending
	// the movie box, otherwise the patched size would swallow the moov.
	mdatEnd := w.headerEnd + st.PayloadLen
	if err := w.writeMoov(st); err != nil {
		return err
	}
	mdatStart := w.headerEnd - int64(mp4.LargeHeaderSize)
	if _, err := w.data.Seek(mdatStart+8, io.SeekStart); err != nil {
		return err
	}
	if err := w.writeU64(w.data, uint64(mdatEnd-mdatStart)); err != nil {
		return err
	}
	if _, err := w.data.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if err := w.data.Sync(); err != nil {
		return err
	}
	w.finished = true
	return nil
}

// Publish atomically renames the finished part file to the final path.
func (w *Writer) Publish() error {
	if !w.finished {
		return errors.New("index: Publish before Finish")
	}
	if err := w.releaseTables(); err != nil {
		return err
	}
	if err := w.data.Close(); err != nil {
		return err
	}
	w.data = nil
	if err := os.Rename(w.dataPath, w.finalPath()); err != nil {
		return err
	}
	return syncDir(filepath.Dir(w.finalPath()))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (w *Writer) releaseTables() error {
	for _, f := range []*os.File{w.stsz, w.stts, w.stsc, w.stco} {
		if f != nil {
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
	w.stsz, w.stts, w.stsc, w.stco = nil, nil, nil, nil
	return os.RemoveAll(w.tblDir)
}

func (w *Writer) finalPath() string { return w.dataPath[:len(w.dataPath)-len(".part")] }
