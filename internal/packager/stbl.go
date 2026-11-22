package packager

import (
	"encoding/binary"

	"github.com/abema/go-mp4"
)

// writeStbl 写采样表：stsd(sowt PCM 入口)、stts、stsc、stsz、stco。
func (p *Packager) writeStbl(w *mp4.Writer, samples int64) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	defer w.EndBox() // stbl
	// stsd：FullBox 头 + entryCount + "sowt" 采样入口
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(0)); err != nil { // version+flags
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(1)); err != nil { // entry count
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxType{'s', 'o', 'w', 't'}}); err != nil {
		return err
	}
	var entry [28]byte // 6 reserved + 2 dref + 8 audio fields + 4 rate
	binary.BigEndian.PutUint16(entry[6:], 1) // data reference index
	binary.BigEndian.PutUint16(entry[16:], uint16(p.channels))
	binary.BigEndian.PutUint16(entry[18:], 16) // 16-bit PCM
	binary.BigEndian.PutUint32(entry[24:], uint32(p.rate)<<16)
	if _, err := w.Write(entry[:]); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // sowt
		return err
	}
	if _, err := w.EndBox(); err != nil { // stsd
		return err
	}
	frames := uint32(len(p.sizes))
	if err := marshalTo(w, &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{
		{SampleCount: frames, SampleDelta: uint32(p.frameSamples)},
	}}); err != nil {
		return err
	}
	if err := marshalTo(w, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: frames, SampleDescriptionIndex: 1},
	}}); err != nil {
		return err
	}
	if err := marshalTo(w, &mp4.Stsz{SampleSize: 0, SampleCount: frames, EntrySize: p.sizes}); err != nil {
		return err
	}
	return marshalTo(w, &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{uint32(p.mdatPayload)}})
}
