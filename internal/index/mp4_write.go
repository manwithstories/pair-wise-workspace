package index

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"
)

const (
	durationUnknown = ^uint64(0)
	handlerVideo    = "vide"
)

// tableStats is everything the moov writer needs about a spooled table.
type tableStats struct {
	size      int64
	count     uint32
	maxOffset uint64
}

func (w *Writer) tableStats() (stts, stsc, stsz, stco tableStats, err error) {
	for _, t := range []struct {
		f      *os.File
		entry  int64
		target *tableStats
	}{
		{w.stts, 8, &stts},
		{w.stsc, 12, &stsc},
		{w.stsz, 4, &stsz},
		{w.stco, 8, &stco},
	} {
		fi, statErr := t.f.Stat()
		if statErr != nil {
			return tableStats{}, tableStats{}, tableStats{}, tableStats{}, statErr
		}
		t.target.size = fi.Size()
		if fi.Size()%t.entry != 0 {
			return tableStats{}, tableStats{}, tableStats{}, tableStats{},
				fmt.Errorf("index table %s has a partial entry", t.f.Name())
		}
		t.target.count = uint32(fi.Size() / t.entry)
	}
	if err := scanMaxU64(w.stco, &stco.maxOffset); err != nil {
		return tableStats{}, tableStats{}, tableStats{}, tableStats{}, err
	}
	return stts, stsc, stsz, stco, nil
}

func scanMaxU64(f *os.File, dst *uint64) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	buf := make([]byte, 8*4096)
	for off := int64(0); off < fi.Size(); {
		n := int64(len(buf))
		if fi.Size()-off < n {
			n = fi.Size() - off
		}
		if _, err := f.ReadAt(buf[:n], off); err != nil && err != io.EOF {
			return err
		}
		for i := int64(0); i+8 <= n; i += 8 {
			if v := binary.BigEndian.Uint64(buf[i : i+8]); v > *dst {
				*dst = v
			}
		}
		off += n
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return nil
}

// writeMoov appends the movie box describing every committed sample.
func (w *Writer) writeMoov(st State) error {
	stts, stsc, stsz, stco, err := w.tableStats()
	if err != nil {
		return err
	}
	duration := uint64(0)
	if st.Samples > 0 {
		duration = uint64(st.LastMedia+st.LastDelta-st.FirstMedia) * 1
		if duration == 0 {
			duration = 1
		}
	}
	mw := mp4.NewWriter(w.data)
	ctx := mp4.Context{}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov(), Size: 0}); err != nil {
		return err
	}

	mvhd := &mp4.Mvhd{
		Timescale:   Timescale,
		DurationV0:  uint32(duration),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      unityMatrix(),
		NextTrackID: 2,
	}
	if err := marshalTo(mw, mvhd, ctx); err != nil {
		return err
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak(), Size: 0}); err != nil {
		return err
	}

	tkhd := &mp4.Tkhd{
		TrackID:    1,
		DurationV0: uint32(duration),
		Matrix:     unityMatrix(),
		Width:      0,
		Height:     0,
	}
	tkhd.SetFlags(0x000007) // enabled | in movie | in preview
	if err := marshalTo(mw, tkhd, ctx); err != nil {
		return err
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia(), Size: 0}); err != nil {
		return err
	}
	mdhd := &mp4.Mdhd{
		Timescale:  Timescale,
		DurationV0: uint32(duration),
		Language:   [3]byte{'u', 'n', 'd'},
	}
	if err := marshalTo(mw, mdhd, ctx); err != nil {
		return err
	}
	hdlr := &mp4.Hdlr{
		HandlerType: mp4.StrToBoxType(handlerVideo),
		Name:        "mediq frame index",
	}
	if err := marshalTo(mw, hdlr, ctx); err != nil {
		return err
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf(), Size: 0}); err != nil {
		return err
	}
	vmhd := &mp4.Vmhd{}
	vmhd.SetFlags(1) // no lean-ahead
	if err := marshalTo(mw, vmhd, ctx); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf(), Size: 0}); err != nil {
		return err
	}
	// dinf is written manually so the nested dref/url layout is explicit.
	drefPayload := make([]byte, 8)
	binary.BigEndian.PutUint32(drefPayload[4:], 1)
	drefSize := uint64(8 + 4 + 4 + 12) // header + fullbox + count + url box
	if err := writeBoxHeader(mw, mp4.BoxTypeDref(), drefSize); err != nil {
		return fmt.Errorf("dref header: %w", err)
	}
	if _, err := mw.Write(drefPayload[:4]); err != nil {
		return err
	}
	if _, err := mw.Write(drefPayload[4:8]); err != nil {
		return err
	}
	urlPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(urlPayload, mp4.UrlSelfContained)
	if err := writeBoxHeader(mw, mp4.BoxTypeUrl(), 12); err != nil {
		return fmt.Errorf("url header: %w", err)
	}
	if _, err := mw.Write(urlPayload); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // dinf
		return err
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl(), Size: 0}); err != nil {
		return err
	}
	if err := w.writeStsd(mw); err != nil {
		return err
	}
	if err := w.copyTable(mw, mp4.BoxTypeStts(), w.stts, stts, 4+4); err != nil {
		return err
	}
	if err := w.copyTable(mw, mp4.BoxTypeStsc(), w.stsc, stsc, 4+4); err != nil {
		return err
	}
	if err := w.writeStsz(mw, stsz); err != nil {
		return err
	}
	if stco.maxOffset > 0xFFFFFFFF {
		if err := w.copyTable(mw, mp4.BoxTypeCo64(), w.stco, stco, 4+4); err != nil {
			return err
		}
	} else {
		if err := w.writeStco32(mw, stco); err != nil {
			return err
		}
	}
	// Close stbl, minf, mdia, trak, moov (dinf is closed inline above).
	for i := 0; i < 5; i++ {
		if _, err := mw.EndBox(); err != nil {
			return err
		}
	}
	return nil
}

func unityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func marshalTo(w io.Writer, box mp4.IImmutableBox, ctx mp4.Context) error {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, box, ctx); err != nil {
		return err
	}
	if err := writeBoxHeader(w, box.GetType(), uint64(buf.Len()+8)); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func writeBoxHeader(w io.Writer, t mp4.BoxType, size uint64) error {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(size))
	copy(hdr[4:8], t[:])
	_, err := w.Write(hdr[:])
	return err
}

// writeStsd emits the sample description with the declared frame-record entry.
func (w *Writer) writeStsd(mw io.Writer) error {
	entry := &frameEntry{Width: 0, Height: 0}
	entry.Type = sampleEntryType
	entry.DataReferenceIndex = 1
	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, entry, mp4.Context{}); err != nil {
		return err
	}
	entrySize := payload.Len() + 8
	stsdSize := uint64(8 + 4 + 4 + entrySize)
	var hdr [16]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(stsdSize))
	stsdType := mp4.BoxTypeStsd()
	copy(hdr[4:8], stsdType[:])
	binary.BigEndian.PutUint32(hdr[8:12], 0)  // version + flags
	binary.BigEndian.PutUint32(hdr[12:16], 1) // entry count
	if _, err := mw.Write(hdr[:]); err != nil {
		return err
	}
	var entryHdr [8]byte
	binary.BigEndian.PutUint32(entryHdr[0:4], uint32(entrySize))
	copy(entryHdr[4:8], sampleEntryType[:])
	if _, err := mw.Write(entryHdr[:]); err != nil {
		return err
	}
	_, err := mw.Write(payload.Bytes())
	return err
}

// copyTable streams a spooled table into a box with a 4-byte full-box header,
// a 4-byte entry count and the raw entries.
func (w *Writer) copyTable(mw io.Writer, t mp4.BoxType, f *os.File, stats tableStats, overhead int64) error {
	size := uint64(8 + overhead + stats.size)
	if err := writeBoxHeader(mw, t, size); err != nil {
		return err
	}
	var full [8]byte
	binary.BigEndian.PutUint32(full[0:4], 0)
	binary.BigEndian.PutUint32(full[4:8], stats.count)
	if _, err := mw.Write(full[:]); err != nil {
		return err
	}
	return copyExact(mw, f, stats.size)
}

func (w *Writer) writeStsz(mw io.Writer, stats tableStats) error {
	size := uint64(8 + 16 + stats.size)
	if err := writeBoxHeader(mw, mp4.BoxTypeStsz(), size); err != nil {
		return err
	}
	var full [12]byte
	binary.BigEndian.PutUint32(full[0:4], 0) // version + flags
	binary.BigEndian.PutUint32(full[4:8], 0) // sample_size (0 = per-sample table)
	binary.BigEndian.PutUint32(full[8:12], stats.count)
	if _, err := mw.Write(full[:]); err != nil {
		return err
	}
	return copyExact(mw, w.stsz, stats.size)
}

func (w *Writer) writeStco32(mw io.Writer, stats tableStats) error {
	size := uint64(8 + 8 + stats.count*4)
	if err := writeBoxHeader(mw, mp4.BoxTypeStco(), size); err != nil {
		return err
	}
	var full [8]byte
	binary.BigEndian.PutUint32(full[0:4], 0)
	binary.BigEndian.PutUint32(full[4:8], stats.count)
	if _, err := mw.Write(full[:]); err != nil {
		return err
	}
	// Narrow the same 8-byte entries to 32 bits while streaming.
	buf := make([]byte, 8*4096)
	for off := int64(0); off < stats.size; {
		n := int64(len(buf))
		if stats.size-off < n {
			n = stats.size - off
		}
		if _, err := w.stco.ReadAt(buf[:n], off); err != nil && err != io.EOF {
			return err
		}
		out := make([]byte, 0, n/2)
		for i := int64(0); i+8 <= n; i += 8 {
			v := binary.BigEndian.Uint64(buf[i : i+8])
			var tmp [4]byte
			binary.BigEndian.PutUint32(tmp[:], uint32(v))
			out = append(out, tmp[:]...)
		}
		if _, err := mw.Write(out); err != nil {
			return err
		}
		off += n
	}
	_, err := w.stco.Seek(0, io.SeekStart)
	return err
}

func copyExact(dst io.Writer, src *os.File, n int64) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	written, err := io.CopyN(dst, src, n)
	if err != nil {
		return err
	}
	if written != n {
		return fmt.Errorf("table copy short: %d != %d", written, n)
	}
	return nil
}
