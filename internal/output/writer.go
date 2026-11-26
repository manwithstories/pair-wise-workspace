// Package output writes repaired fragments back to deterministic fMP4 bytes
// using go-mp4. The writer patches mfhd sequence numbers and tfdt decode
// times but never touches sample composition offsets or mdat payloads.
package output

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"

	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/stream"
	"relay/internal/timeline"
)

// Writer is the stream.Sink implementation for repaired fragments.
type Writer struct{}

// NewWriter returns the default output writer.
func NewWriter() *Writer { return &Writer{} }

// Write serializes rf with the repaired sequence number and (for rollback
// cases) corrected tfdt, then stores it under outDir/frags.
func (w *Writer) Write(rf *timeline.RepairedFragment, outDir string) (int64, string, error) {
	raw, err := Serialize(rf)
	if err != nil {
		return 0, "", err
	}
	path := filepath.Join(outDir, "frags", filepath.Base(rf.Key()))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return 0, "", media.NewFault(media.FaultCheckpoint, rf.TrackID,
			rf.NewSequence, rf.MoofOffset, "write fragment: "+err.Error())
	}
	return int64(len(raw)), stream.HashBytes(raw), nil
}

// Serialize rebuilds one fragment deterministically. It reuses the original
// mdat payload verbatim and rewrites only the moof boxes, guaranteeing that
// sample data and composition offsets remain byte-stable.
func Serialize(rf *timeline.RepairedFragment) ([]byte, error) {
	f := rf.Fragment
	buf := newSeekableBuffer()
	mw := mp4.NewWriter(buf)

	// moof
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()}); err != nil {
		return nil, err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTraf()}); err != nil {
		return nil, err
	}
	mfhd := &mp4.Mfhd{SequenceNumber: rf.NewSequence}
	if _, err := writeChild(mw, mp4.BoxTypeMfhd(), mfhd); err != nil {
		return nil, err
	}
	tfhd := &mp4.Tfhd{FullBox: mp4.FullBox{Flags: flags32(0x020000)}, TrackID: f.TrackID}
	if _, err := writeChild(mw, mp4.BoxTypeTfhd(), tfhd); err != nil {
		return nil, err
	}
	tfdt := &mp4.Tfdt{FullBox: mp4.FullBox{Version: 1}, BaseMediaDecodeTimeV1: rf.RepairedTfdt}
	if _, err := writeChild(mw, mp4.BoxTypeTfdt(), tfdt); err != nil {
		return nil, err
	}
	if err := writeTrun(mw, f); err != nil {
		return nil, err
	}
	if _, err := mw.EndBox(); err != nil { // traf
		return nil, err
	}
	moofBI, err := mw.EndBox() // moof
	if err != nil {
		return nil, err
	}

	// mdat: original sample payload verbatim.
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return nil, err
	}
	if _, err := mw.Write(f.Mdat); err != nil {
		return nil, err
	}
	if _, err := mw.EndBox(); err != nil {
		return nil, err
	}

	raw := buf.Bytes()
	// Patch trun data_offset to point at the first sample (moof length +
	// mdat header), matching the convention used by the fixture generator.
	patchTrunDataOffset(raw, moofBI.Size)
	return raw, nil
}

// flags32 packs a 24-bit ISO BMFF flags word into the FullBox [3]byte field.
func flags32(v uint32) [3]byte {
	return [3]byte{byte(v >> 16), byte(v >> 8), byte(v)}
}

func writeChild(mw *mp4.Writer, typ mp4.BoxType, box mp4.IImmutableBox) (uint64, error) {
	bi, err := mw.StartBox(&mp4.BoxInfo{Type: typ})
	if err != nil {
		return 0, err
	}
	n, err := mp4.Marshal(mw, box, mp4.Context{})
	if err != nil {
		return 0, err
	}
	if _, err := mw.EndBox(); err != nil {
		return 0, err
	}
	return bi.Size + n, nil
}

func writeTrun(mw *mp4.Writer, f *ingest.Fragment) error {
	trun := &mp4.Trun{
		FullBox:     mp4.FullBox{Flags: flags32(0x000f01)},
		SampleCount: uint32(len(f.Samples)),
	}
	for _, s := range f.Samples {
		trun.Entries = append(trun.Entries, mp4.TrunEntry{
			SampleDuration:                s.Duration,
			SampleSize:                    s.Size,
			SampleFlags:                   s.Flags,
			SampleCompositionTimeOffsetV1: s.CompositionOffset,
		})
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrun()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, trun, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

// patchTrunDataOffset writes the data offset relative to moof start.
func patchTrunDataOffset(raw []byte, moofSize uint64) {
	// Locate the trun box by scanning the fixed moof/traf header structure.
	idx := bytes.Index(raw, []byte("trun"))
	if idx < 0 {
		return
	}
	// idx points at the type word; data_offset is header(8)+fullbox(4)+
	// sample_count(4) bytes after the box start (idx-4).
	pos := idx - 4 + 8 + 4 + 4
	putU32(raw[pos:], uint32(moofSize+8))
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
