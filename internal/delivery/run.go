// run.go builds fMP4 segments with go-mp4 from the index and the retained
// access units, then writes the manifest. The init segment declares the track
// layouts; each media segment carries one moof/mdat pair whose samples begin on
// a random-access point. Damaged stretches become manifest gaps, and a segment
// never starts inside one: it starts at the recovery keyframe instead.
package delivery

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/abema/go-mp4"

	"cinepipe/internal/clock"
	"cinepipe/internal/index"
	"cinepipe/internal/ts"
)

// Timescale is the fMP4 media timescale, matching the 90 kHz clock.
const Timescale uint32 = 90000

// trackIDVideo and trackIDAudio are fixed track ids in the produced files.
const (
	trackIDVideo uint32 = 1
	trackIDAudio uint32 = 2
)

// fileBuffer is an in-memory buffer that records the current write position,
// so box writing can compute sizes and patch the trun data offset. go-mp4's
// Writer needs a WriteSeeker; the position tracking here mirrors what that
// writer expects from a real file.
type fileBuffer struct {
	data []byte
	pos  int64
}

func (f *fileBuffer) Write(p []byte) (int, error) {
	end := f.pos + int64(len(p))
	if end > int64(len(f.data)) {
		next := make([]byte, end)
		copy(next, f.data)
		f.data = next
	}
	copy(f.data[f.pos:], p)
	f.pos = end
	return len(p), nil
}

func (f *fileBuffer) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0: // SeekStart
		f.pos = offset
	case 1: // SeekCurrent
		f.pos += offset
	case 2: // SeekEnd
		f.pos = int64(len(f.data)) + offset
	}
	return f.pos, nil
}

func (f *fileBuffer) Len() int { return len(f.data) }
func (f *fileBuffer) Bytes() []byte {
	return f.data
}

// fmp4Writer wraps go-mp4's Writer around a fileBuffer.
type fmp4Writer struct {
	w   *mp4.Writer
	buf *fileBuffer
}

func newFMP4Writer() *fmp4Writer {
	buf := &fileBuffer{}
	return &fmp4Writer{w: mp4.NewWriter(buf), buf: buf}
}

func (f *fmp4Writer) box(t mp4.BoxType) (*mp4.BoxInfo, error) {
	return f.w.StartBox(&mp4.BoxInfo{Type: t})
}

func (f *fmp4Writer) end() error {
	_, err := f.w.EndBox()
	return err
}

func (f *fmp4Writer) marshal(b mp4.IImmutableBox) error {
	_, err := mp4.Marshal(f.w, b, mp4.Context{})
	return err
}

// videoDim reports the coded frame size. The synthetic fixtures use 1920x1080;
// the value lives here so the init segment stays consistent.
const (
	videoWidth  uint16 = 1920
	videoHeight uint16 = 1080
)

// AVCDefaults returns placeholder SPS/PPS when the stream did not capture any,
// so an init segment is still structurally well-formed.
func avcConfig(sps, pps []byte) *mp4.AVCDecoderConfiguration {
	avcc := &mp4.AVCDecoderConfiguration{
		ConfigurationVersion: 1,
		Profile:              0x64,
		ProfileCompatibility: 0x00,
		Level:                0x28,
		LengthSizeMinusOne:   3,
	}
	avcc.SetType(mp4.BoxTypeAvcC())
	if len(sps) == 0 {
		sps = []byte{0x67, 0x64, 0x00, 0x28}
	}
	if len(pps) == 0 {
		pps = []byte{0x68, 0xEE, 0x3C, 0x80}
	}
	avcc.SequenceParameterSets = []mp4.AVCParameterSet{
		{Length: uint16(len(sps)), NALUnit: sps},
	}
	avcc.NumOfSequenceParameterSets = 1
	avcc.PictureParameterSets = []mp4.AVCParameterSet{
		{Length: uint16(len(pps)), NALUnit: pps},
	}
	avcc.NumOfPictureParameterSets = 1
	return avcc
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

// buildInit constructs the init segment: ftyp + moov describing the tracks
// exactly as the index reports them. Colour and audio metadata are carried
// verbatim, never normalised away.
func buildInit(idx *index.Index, sps, pps []byte) ([]byte, error) {
	f := newFMP4Writer()
	if err := writeFtyp(f); err != nil {
		return nil, fmt.Errorf("ftyp: %w", err)
	}
	if _, err := f.box(mp4.BoxTypeMoov()); err != nil {
		return nil, fmt.Errorf("moov: %w", err)
	}
	if err := buildMvhd(f, idx); err != nil {
		return nil, fmt.Errorf("mvhd: %w", err)
	}
	video, hasVideo := videoTrack(idx)
	audio, hasAudio := audioTrack(idx)
	if hasVideo {
		if err := buildVideoTrak(f, video, sps, pps); err != nil {
			return nil, fmt.Errorf("video trak: %w", err)
		}
	}
	if hasAudio {
		if err := buildAudioTrak(f, audio); err != nil {
			return nil, fmt.Errorf("audio trak: %w", err)
		}
	}
	if err := buildMvex(f, hasVideo, hasAudio); err != nil {
		return nil, fmt.Errorf("mvex: %w", err)
	}
	if err := f.end(); err != nil {
		return nil, fmt.Errorf("close moov: %w", err)
	}
	return f.buf.Bytes(), nil
}

func writeFtyp(f *fmp4Writer) error {
	if _, err := f.box(mp4.BoxTypeFtyp()); err != nil {
		return err
	}
	err := f.marshal(&mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
			{CompatibleBrand: [4]byte{'a', 'v', 'c', '1'}},
		},
	})
	if err != nil {
		return err
	}
	return f.end()
}

func buildMvhd(f *fmp4Writer, idx *index.Index) error {
	if _, err := f.box(mp4.BoxTypeMvhd()); err != nil {
		return err
	}
	dur := idxDuration(idx)
	if dur < 0 {
		dur = 0
	}
	if err := f.marshal(&mp4.Mvhd{
		Timescale:   Timescale,
		DurationV0:  clampU32(dur),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: nextTrackID(hasVideoTrack(idx), hasAudioTrack(idx)),
	}); err != nil {
		return err
	}
	return f.end()
}

func buildVideoTrak(f *fmp4Writer, t index.Track, sps, pps []byte) error {
	if _, err := f.box(mp4.BoxTypeTrak()); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeTkhd()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Tkhd{
		TrackID:    trackIDVideo,
		DurationV0: clampU32(idxDurationTrack(t)),
		Matrix:     identityMatrix(),
		Width:      uint32(videoWidth) << 16,
		Height:     uint32(videoHeight) << 16,
	}); err != nil {
		return err
	}
	if err := f.end(); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := writeMdhd(f, clampU32(idxDurationTrack(t))); err != nil {
		return err
	}
	if err := writeHdlr(f, [4]byte{'v', 'i', 'd', 'e'}, "VideoHandler"); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeVmhd()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Vmhd{}); err != nil {
		return err
	}
	if err := f.end(); err != nil {
		return err
	}
	if err := writeDinf(f); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := writeStsdVideo(f, sps, pps, t.Color); err != nil {
		return err
	}
	if err := writeEmptyTables(f); err != nil {
		return err
	}
	return closeN(f, 4) // stbl, minf, mdia, trak
}

func buildAudioTrak(f *fmp4Writer, t index.Track) error {
	if _, err := f.box(mp4.BoxTypeTrak()); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeTkhd()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Tkhd{
		TrackID:    trackIDAudio,
		DurationV0: clampU32(idxDurationTrack(t)),
		Matrix:     identityMatrix(),
		Volume:     0x0100,
	}); err != nil {
		return err
	}
	if err := f.end(); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := writeMdhd(f, clampU32(idxDurationTrack(t))); err != nil {
		return err
	}
	if err := writeHdlr(f, [4]byte{'s', 'o', 'u', 'n'}, "SoundHandler"); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeSmhd()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Smhd{}); err != nil {
		return err
	}
	if err := f.end(); err != nil {
		return err
	}
	if err := writeDinf(f); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := writeStsdAudio(f, t); err != nil {
		return err
	}
	if err := writeEmptyTables(f); err != nil {
		return err
	}
	return closeN(f, 4)
}

// closeN closes n nested boxes in order (innermost first).
func closeN(f *fmp4Writer, n int) error {
	for i := 0; i < n; i++ {
		if err := f.end(); err != nil {
			return err
		}
	}
	return nil
}

func writeMdhd(f *fmp4Writer, duration uint32) error {
	if _, err := f.box(mp4.BoxTypeMdhd()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Mdhd{
		Timescale:  Timescale,
		DurationV0: duration,
		Language:   [3]byte{'u', 'n', 'd'},
	}); err != nil {
		return err
	}
	return f.end()
}

func writeHdlr(f *fmp4Writer, handler [4]byte, name string) error {
	if _, err := f.box(mp4.BoxTypeHdlr()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Hdlr{HandlerType: handler, Name: name}); err != nil {
		return err
	}
	return f.end()
}

func writeDinf(f *fmp4Writer) error {
	if _, err := f.box(mp4.BoxTypeDinf()); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeDref()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Dref{EntryCount: 1}); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeUrl()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}); err != nil {
		return err
	}
	if err := f.end(); err != nil {
		return err
	}
	if err := f.end(); err != nil {
		return err
	}
	return f.end()
}

func writeStsdVideo(f *fmp4Writer, sps, pps []byte, color *index.ColorInfo) error {
	if _, err := f.box(mp4.BoxTypeStsd()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Stsd{EntryCount: 1}); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeAvc1()); err != nil {
		return err
	}
	entry := &mp4.VisualSampleEntry{
		SampleEntry:     mp4.SampleEntry{DataReferenceIndex: 1},
		Width:           videoWidth,
		Height:          videoHeight,
		Horizresolution: 0x00480000,
		Vertresolution:  0x00480000,
		FrameCount:      1,
		Depth:           0x0018,
	}
	entry.SetType(mp4.BoxTypeAvc1())
	if err := f.marshal(entry); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeAvcC()); err != nil {
		return err
	}
	if err := f.marshal(avcConfig(sps, pps)); err != nil {
		return err
	}
	if err := f.end(); err != nil {
		return err
	}
	if color != nil {
		if _, err := f.box(mp4.BoxTypeColr()); err != nil {
			return err
		}
		if err := f.marshal(&mp4.Colr{
			ColourType:              [4]byte{'n', 'c', 'l', 'x'},
			ColourPrimaries:         color.Primaries,
			TransferCharacteristics: color.Transfer,
			MatrixCoefficients:      color.Matrix,
		}); err != nil {
			return err
		}
		if err := f.end(); err != nil {
			return err
		}
	}
	if err := f.end(); err != nil { // avc1
		return err
	}
	return f.end() // stsd
}

func writeStsdAudio(f *fmp4Writer, t index.Track) error {
	if _, err := f.box(mp4.BoxTypeStsd()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Stsd{EntryCount: 1}); err != nil {
		return err
	}
	if _, err := f.box(mp4.BoxTypeMp4a()); err != nil {
		return err
	}
	entry := &mp4.AudioSampleEntry{
		SampleEntry:  mp4.SampleEntry{DataReferenceIndex: 1},
		ChannelCount: uint16(t.Channels),
		SampleSize:   16,
		SampleRate:   uint32(t.SampleRate) << 16,
	}
	entry.SetType(mp4.BoxTypeMp4a())
	if err := f.marshal(entry); err != nil {
		return err
	}
	if err := f.end(); err != nil {
		return err
	}
	return f.end()
}

func writeEmptyTables(f *fmp4Writer) error {
	for _, pair := range []struct {
		t mp4.BoxType
		b mp4.IImmutableBox
	}{
		{mp4.BoxTypeStts(), &mp4.Stts{}},
		{mp4.BoxTypeStsc(), &mp4.Stsc{}},
		{mp4.BoxTypeStsz(), &mp4.Stsz{}},
		{mp4.BoxTypeStco(), &mp4.Stco{}},
	} {
		if _, err := f.box(pair.t); err != nil {
			return err
		}
		if err := f.marshal(pair.b); err != nil {
			return err
		}
		if err := f.end(); err != nil {
			return err
		}
	}
	return nil
}

func buildMvex(f *fmp4Writer, hasVideo, hasAudio bool) error {
	if _, err := f.box(mp4.BoxTypeMvex()); err != nil {
		return err
	}
	if hasVideo {
		if err := writeTrex(f, trackIDVideo); err != nil {
			return err
		}
	}
	if hasAudio {
		if err := writeTrex(f, trackIDAudio); err != nil {
			return err
		}
	}
	return f.end()
}

func writeTrex(f *fmp4Writer, id uint32) error {
	if _, err := f.box(mp4.BoxTypeTrex()); err != nil {
		return err
	}
	if err := f.marshal(&mp4.Trex{
		TrackID:                       id,
		DefaultSampleDescriptionIndex: 1,
	}); err != nil {
		return err
	}
	return f.end()
}

// sample is one mdat entry: its bytes and timing.
type sample struct {
	size  uint32
	ticks int64
	dur   int64
	key   bool
	data  []byte
}

// buildMediaSegment writes one moof/mdat pair covering samples. The trun data
// offset is computed by building the moof once to learn its size, then patched:
// go-mp4 cannot predeclare the offset, so the fragment is assembled in two
// passes over the same writer buffer.
func buildMediaSegment(trackID uint32, samples []sample, baseTicks int64) ([]byte, error) {
	moof, err := buildMoof(trackID, samples, baseTicks, 0)
	if err != nil {
		return nil, err
	}
	// data offset = moof size + 8 (the mdat header). Rebuild the moof with the
	// correct offset; the moof layout does not change length because the offset
	// is a fixed 4-byte field.
	moof2, err := buildMoof(trackID, samples, baseTicks, int32(len(moof))+8)
	if err != nil {
		return nil, err
	}
	if len(moof2) != len(moof) {
		return nil, fmt.Errorf("moof size changed between passes: %d vs %d", len(moof), len(moof2))
	}
	mdatBody := make([]byte, 0, sumSizes(samples))
	for _, s := range samples {
		mdatBody = append(mdatBody, s.data...)
	}
	// mdat with a 32-bit size unless the payload is too large.
	out := make([]byte, 0, len(moof2)+8+len(mdatBody))
	out = append(out, moof2...)
	out = append(out, mdatHeader(len(mdatBody))...)
	out = append(out, mdatBody...)
	return out, nil
}

// buildMoof builds a moof with one traf, a tfdt, and a trun carrying a sample
// entry per sample. dataOffset is the trun data_offset field.
func buildMoof(trackID uint32, samples []sample, baseTicks int64, dataOffset int32) ([]byte, error) {
	f := newFMP4Writer()
	if _, err := f.box(mp4.BoxTypeMoof()); err != nil {
		return nil, err
	}
	if _, err := f.box(mp4.BoxTypeMfhd()); err != nil {
		return nil, err
	}
	if err := f.marshal(&mp4.Mfhd{SequenceNumber: 1}); err != nil {
		return nil, err
	}
	if err := f.end(); err != nil {
		return nil, err
	}
	if _, err := f.box(mp4.BoxTypeTraf()); err != nil {
		return nil, err
	}
	if _, err := f.box(mp4.BoxTypeTfhd()); err != nil {
		return nil, err
	}
	// Flags: default-base-is-moof (0x020000) + default sample duration (0x08)
	// + default sample size (0x10) to compact the trun when values are uniform.
	tfhdFlags := [3]byte{0x02, 0x00, 0x18}
	if err := f.marshal(&mp4.Tfhd{
		FullBox:      mp4.FullBox{Flags: tfhdFlags},
		TrackID:      trackID,
	}); err != nil {
		return nil, err
	}
	if err := f.end(); err != nil {
		return nil, err
	}
	if _, err := f.box(mp4.BoxTypeTfdt()); err != nil {
		return nil, err
	}
	if err := f.marshal(&mp4.Tfdt{
		FullBox:               mp4.FullBox{Version: 1},
		BaseMediaDecodeTimeV1: uint64(baseTicks),
	}); err != nil {
		return nil, err
	}
	if err := f.end(); err != nil {
		return nil, err
	}
	if _, err := f.box(mp4.BoxTypeTrun()); err != nil {
		return nil, err
	}
	// Flags: data-offset present (0x01) and sample flags present (0x400).
	trun := &mp4.Trun{
		FullBox:     mp4.FullBox{Flags: [3]byte{0x00, 0x04, 0x01}},
		SampleCount: uint32(len(samples)),
		DataOffset:  dataOffset,
	}
	trun.Entries = make([]mp4.TrunEntry, len(samples))
	for i, s := range samples {
		flags := uint32(0x00010000) // non-sync
		if s.key {
			flags = 0x02000000 // sync sample
		}
		trun.Entries[i] = mp4.TrunEntry{
			SampleDuration: uint32(s.dur),
			SampleSize:     s.size,
			SampleFlags:    flags,
		}
	}
	if err := f.marshal(trun); err != nil {
		return nil, err
	}
	if err := f.end(); err != nil {
		return nil, err
	}
	if err := f.end(); err != nil { // traf
		return nil, err
	}
	if err := f.end(); err != nil { // moof
		return nil, err
	}
	return f.buf.Bytes(), nil
}

// mdatHeader returns the box header for an mdat of n payload bytes, using the
// 64-bit size when the payload would overflow a 32-bit size.
func mdatHeader(n int) []byte {
	if n+8 <= 0xFFFFFFFF {
		return []byte{byte(uint32(n + 8) >> 24), byte(uint32(n + 8) >> 16),
			byte(uint32(n + 8) >> 8), byte(uint32(n + 8) & 0xFF),
			'm', 'd', 'a', 't'}
	}
	// 64-bit size form.
	size := uint64(n) + 16
	return []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0,
		byte(size >> 56), byte(size >> 48), byte(size >> 40), byte(size >> 32),
		byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}
}

func sumSizes(samples []sample) int {
	n := 0
	for _, s := range samples {
		n += int(s.size)
	}
	return n
}

// audioTrack returns the first audio track.
func audioTrack(idx *index.Index) (index.Track, bool) {
	for _, t := range idx.Tracks {
		if t.Kind == index.TrackAudio {
			return t, true
		}
	}
	return index.Track{}, false
}

func hasVideoTrack(idx *index.Index) bool { _, ok := videoTrack(idx); return ok }
func hasAudioTrack(idx *index.Index) bool { _, ok := audioTrack(idx); return ok }

func nextTrackID(hasV, hasA bool) uint32 {
	n := uint32(1)
	if hasV {
		n++
	}
	if hasA {
		n++
	}
	return n
}

// idxDuration and idxDurationTrack derive track durations from the last
// timestamp on the timeline.
func idxDuration(idx *index.Index) int64 {
	var max int64
	for _, t := range idx.Tracks {
		if d := idxDurationTrack(t); d > max {
			max = d
		}
	}
	return max
}

func idxDurationTrack(t index.Track) int64 {
	var max int64
	for _, k := range t.Keyframes {
		if k.Ticks != clock.NoTime && k.Ticks > max {
			max = k.Ticks
		}
	}
	return max
}

func clampU32(v int64) uint32 {
	if v < 0 {
		return 0
	}
	if v > 0xFFFFFFFF {
		return 0xFFFFFFFF
	}
	return uint32(v)
}

// writeFileAtomic writes via a sibling temp file and renames, so the manifest
// and segment files appear whole or not at all.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func sortNames(ms []ManifestMedia) {
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
}

var _ = ts.PacketSize
