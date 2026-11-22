package mux

import (
	"github.com/abema/go-mp4"

	"mediapack/internal/codec"
	"mediapack/internal/fault"
)

// box runs StartBox/Marshal/EndBox around one leaf box payload.
func box(mw *mp4.Writer, t mp4.BoxType, payload mp4.IBox) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if payload != nil {
		if _, err := mp4.Marshal(mw, payload, mp4.Context{}); err != nil {
			return fault.Wrap(err, "mux")
		}
	}
	if _, err := mw.EndBox(); err != nil {
		return fault.Wrap(err, "mux")
	}
	return nil
}

func writeMinf(mw *mp4.Writer, meta *codec.StreamMeta, samples []Sample,
	timescale uint32, chunkOffsets []uint64) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if err := box(mw, mp4.BoxTypeVmhd(), &mp4.Vmhd{
		FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}},
	}); err != nil {
		return err
	}
	// dinf > dref > url
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if err := marshal(mw, &mp4.Dref{EntryCount: 1}); err != nil {
		return err
	}
	if err := box(mw, mp4.BoxTypeUrl(), &mp4.Url{
		FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}},
	}); err != nil {
		return err
	}
	if err := endAll(mw, 2); err != nil { // dref, dinf
		return err
	}
	// stbl
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if err := writeStsd(mw, meta); err != nil {
		return err
	}
	if err := writeSampleTables(mw, samples, chunkOffsets); err != nil {
		return err
	}
	if err := endAll(mw, 2); err != nil { // stbl, minf
		return err
	}
	return nil
}

func writeStsd(mw *mp4.Writer, meta *codec.StreamMeta) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return fault.Wrap(err, "mux")
	}
	if err := marshal(mw, &mp4.Stsd{EntryCount: 1}); err != nil {
		return err
	}
	var entryType mp4.BoxType
	var cfgType mp4.BoxType
	var cfg mp4.IBox
	switch meta.Codec {
	case codec.H264:
		entryType, cfgType, cfg = mp4.BoxTypeAvc1(), mp4.BoxTypeAvcC(), avcConfig(meta)
	case codec.H265:
		entryType, cfgType, cfg = mp4.BoxTypeHvc1(), mp4.BoxTypeHvcC(), hevcConfig(meta)
	default:
		entryType, cfgType, cfg = mp4.BoxTypeAv01(), mp4.BoxTypeAv1C(), av1Config()
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: entryType}); err != nil {
		return fault.Wrap(err, "mux")
	}
	vse := &mp4.VisualSampleEntry{}
	vse.SetType(entryType) // AnyType boxes carry their type dynamically
	vse.Width, vse.Height = 320, 240
	vse.Horizresolution, vse.Vertresolution = 0x00480000, 0x00480000
	vse.FrameCount, vse.Depth, vse.PreDefined3 = 1, 0x0018, -1
	if err := marshal(mw, vse); err != nil {
		return err
	}
	if err := box(mw, cfgType, cfg); err != nil {
		return err
	}
	if err := endAll(mw, 2); err != nil { // sample entry, stsd
		return err
	}
	return nil
}

func writeSampleTables(mw *mp4.Writer, samples []Sample, chunkOffsets []uint64) error {
	if err := box(mw, mp4.BoxTypeStts(), &mp4.Stts{
		EntryCount: 1,
		Entries:    []mp4.SttsEntry{{SampleCount: uint32(len(samples)), SampleDelta: 3000}},
	}); err != nil {
		return err
	}
	entries := buildStscEntries(samples)
	if err := box(mw, mp4.BoxTypeStsc(), &mp4.Stsc{
		EntryCount: uint32(len(entries)), Entries: entries,
	}); err != nil {
		return err
	}
	sizes := make([]uint32, len(samples))
	for i, s := range samples {
		sizes[i] = s.Size
	}
	if err := box(mw, mp4.BoxTypeStsz(), &mp4.Stsz{
		SampleCount: uint32(len(samples)), EntrySize: sizes,
	}); err != nil {
		return err
	}
	if err := box(mw, mp4.BoxTypeCo64(), &mp4.Co64{
		EntryCount: uint32(len(chunkOffsets)), ChunkOffset: chunkOffsets,
	}); err != nil {
		return err
	}
	return nil
}

// buildStscEntries mirrors the chunk grouping done by writeMdat: a new chunk
// starts whenever the accumulated byte budget is reached.
func buildStscEntries(samples []Sample) []mp4.StscEntry {
	var entries []mp4.StscEntry
	chunkIdx, chunkSize, samplesInChunk := uint32(1), 0, 0
	flush := func() {
		if samplesInChunk == 0 {
			return
		}
		entries = append(entries, mp4.StscEntry{
			FirstChunk: chunkIdx, SamplesPerChunk: uint32(samplesInChunk),
			SampleDescriptionIndex: 1,
		})
		chunkIdx++
		chunkSize, samplesInChunk = 0, 0
	}
	for _, s := range samples {
		if chunkSize > 0 && chunkSize+len(s.Data) > chunkBytes {
			flush()
		}
		chunkSize += len(s.Data)
		samplesInChunk++
	}
	flush()
	return entries
}
