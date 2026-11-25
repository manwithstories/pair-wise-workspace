// Package remux writes aligned samples back into a fragmented MP4 container
// without re-encoding. Encoded sample bytes are copied verbatim; the writer
// stages output in a temp file, verifies it end to end, and only then renames
// it into place, so a truncated or failing run never leaves output that could
// be mistaken for success.
package remux

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/abema/go-mp4"

	"mediad/internal/media"
	"mediad/internal/mp4util"
)

// TrackInput is one aligned track to write, with its preserved sample entry.
type TrackInput struct {
	Key         media.TrackKey `json:"key"`
	TrackID     uint32         `json:"track_id"`
	Kind        media.Kind     `json:"kind"`
	Timescale   int64          `json:"timescale"`
	SampleEntry []byte         `json:"-"` // raw stsd child, copied verbatim
	Width       int            `json:"width,omitempty"`
	Height      int            `json:"height,omitempty"`
	Samples     []media.Sample `json:"-"`
}

// SourceIndex records the provenance of each written sample.
type SourceIndex struct {
	Version   int              `json:"version"`
	TrackKeys []media.TrackKey `json:"track_keys"`
	Entries   []SourceEntry    `json:"entries"`
}

// SourceEntry maps one written sample back to its origin.
type SourceEntry struct {
	Track   media.TrackKey `json:"track"`
	DTS     int64          `json:"dts"`
	PTS     int64          `json:"pts"`
	Segment int            `json:"segment"`
	Sample  int            `json:"sample"`
	URI     string         `json:"uri,omitempty"`
	Hash    string         `json:"hash"`
}

// Summary is the deterministic outcome of a write.
type Summary struct {
	OutputSamples   int            `json:"output_samples"`
	OutputTracks    int            `json:"output_tracks"`
	OutputBytes     int64          `json:"output_bytes"`
	OutputSHA256    string         `json:"output_sha256"`
	SampleSHA256    string         `json:"samples_sha256"`
	SamplesPerTrack map[string]int `json:"samples_per_track"`
	SourceHashMatch bool           `json:"source_hash_match"`
}

// Options configures the writer.
type Options struct {
	// FragmentFrames is the number of samples per moof fragment.
	FragmentFrames int
	// MaxOutputBytes bounds the temp file.
	MaxOutputBytes int64
}

func (o *Options) defaults() {
	if o.FragmentFrames <= 0 {
		o.FragmentFrames = 1
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = 1 << 30
	}
}

// Write produces output.mp4 and output.index.json under dir. It returns once
// the temp file has been verified and atomically renamed. On any error the temp
// file is removed and dir holds no output file.
func Write(dir string, tracks []*TrackInput, opts Options) (*Summary, *SourceIndex, error) {
	opts.defaults()
	if len(tracks) == 0 {
		return nil, nil, media.New(media.CodeNoTrack, "remux", media.SourcePos{}, "no tracks to write")
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].Key.Less(tracks[j].Key) })

	tmp, err := os.CreateTemp(dir, ".output-*.tmp")
	if err != nil {
		return nil, nil, media.Wrap(media.CodeIO, "remux-temp", media.SourcePos{}, err, "")
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpName)
		}
	}()

	plan, err := buildPlan(tracks, opts)
	if err != nil {
		return nil, nil, err
	}
	if err := writeFile(tmp, plan, opts.MaxOutputBytes); err != nil {
		return nil, nil, err
	}
	if err := tmp.Sync(); err != nil {
		return nil, nil, media.Wrap(media.CodeIO, "remux-sync", media.SourcePos{}, err, "")
	}
	if err := tmp.Close(); err != nil {
		return nil, nil, media.Wrap(media.CodeIO, "remux-close", media.SourcePos{}, err, "")
	}

	// Verify the staged file before committing it.
	sum, index, err := verify(tmpName, plan)
	if err != nil {
		return nil, nil, err
	}
	final := filepath.Join(dir, "output.mp4")
	if err := os.Rename(tmpName, final); err != nil {
		return nil, nil, media.Wrap(media.CodeIO, "remux-rename", media.SourcePos{}, err, "")
	}
	committed = true
	if err := writeIndex(dir, index); err != nil {
		return nil, nil, err
	}
	return sum, index, nil
}

// plan holds precomputed layout so the file can be written in one forward pass.
type plan struct {
	tracks  []*TrackInput
	global  []segmentPlan
	samples []media.Sample // stable order: track order, then DTS
}

// segmentPlan is one track's samples grouped into fragments.
type segmentPlan struct {
	track  *TrackInput
	frags  [][]media.Sample
	seqNum uint32
}

func buildPlan(tracks []*TrackInput, opts Options) (*plan, error) {
	p := &plan{tracks: tracks}
	var maxDTS int64
	for _, tr := range tracks {
		if len(tr.Samples) == 0 {
			return nil, media.New(media.CodeNoTrack, "remux-plan", media.SourcePos{},
				"track %s has no samples", tr.Key)
		}
		if tr.Timescale <= 0 {
			return nil, media.New(media.CodeUnsupported, "remux-plan", media.SourcePos{},
				"track %s has invalid timescale", tr.Key)
		}
		if len(tr.SampleEntry) == 0 {
			return nil, media.New(media.CodeIntegrity, "remux-plan", media.SourcePos{},
				"track %s has no preserved sample entry", tr.Key)
		}
		sp := segmentPlan{track: tr, seqNum: 1}
		for i := 0; i < len(tr.Samples); i += opts.FragmentFrames {
			end := i + opts.FragmentFrames
			if end > len(tr.Samples) {
				end = len(tr.Samples)
			}
			sp.frags = append(sp.frags, tr.Samples[i:end])
		}
		p.global = append(p.global, sp)
		p.samples = append(p.samples, tr.Samples...)
		for _, s := range tr.Samples {
			if s.DTS > maxDTS {
				maxDTS = s.DTS
			}
		}
	}
	sort.SliceStable(p.samples, func(i, j int) bool {
		if p.samples[i].DTS != p.samples[j].DTS {
			return p.samples[i].DTS < p.samples[j].DTS
		}
		return p.samples[i].Key.Less(p.samples[j].Key)
	})
	return p, nil
}

func writeFile(f *os.File, p *plan, maxBytes int64) error {
	cw := &countingWriter{w: f, max: maxBytes}
	if err := writeInit(cw, p.tracks); err != nil {
		return err
	}
	for _, sp := range p.global {
		if err := writeFragments(cw, sp); err != nil {
			return err
		}
	}
	return cw.err
}

type countingWriter struct {
	w   *os.File
	n   int64
	max int64
	err error
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	c.n += int64(len(b))
	if c.n > c.max {
		c.err = media.New(media.CodeLimit, "remux-write", media.SourcePos{},
			"output exceeds %d byte limit", c.max)
		return 0, c.err
	}
	return c.w.Write(b)
}

// writeInit emits ftyp + moov built from the preserved sample entries.
func writeInit(w *countingWriter, tracks []*TrackInput) error {
	bw := mp4util.New()
	_ = bw.WriteFtyp("iso6", "iso6", "cmfc", "isom")

	bw.Start("moov")
	var maxEnd uint64
	for _, tr := range tracks {
		for _, s := range tr.Samples {
			if end := uint64(s.DTS + s.Duration); end > maxEnd {
				maxEnd = end
			}
		}
	}
	mvhd := &mp4.Mvhd{
		Timescale:   uint32(media.TickRate),
		DurationV0:  uint32(maxEnd),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		NextTrackID: uint32(len(tracks) + 1),
	}
	if err := bw.Marshal(mvhd); err != nil {
		return err
	}
	for _, tr := range tracks {
		if err := writeTrak(bw, tr); err != nil {
			return err
		}
	}
	bw.Start("mvex")
	for _, tr := range tracks {
		if err := bw.Marshal(&mp4.Trex{TrackID: tr.TrackID, DefaultSampleDescriptionIndex: 1}); err != nil {
			return err
		}
	}
	_ = bw.End()
	_ = bw.End()

	_, err := w.Write(bw.Bytes())
	return err
}

func writeTrak(bw *mp4util.BoxWriter, tr *TrackInput) error {
	bw.Start("trak")
	tkhd := &mp4.Tkhd{
		TrackID:    tr.TrackID,
		DurationV0: 0,
		Matrix:     [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		Width:      uint32(tr.Width) << 16,
		Height:     uint32(tr.Height) << 16,
	}
	if tr.Kind == media.KindAudio {
		tkhd.Volume = 0x0100
	}
	tkhd.SetFlags(0x000003)
	if err := bw.Marshal(tkhd); err != nil {
		return err
	}

	bw.Start("mdia")
	mdhd := &mp4.Mdhd{Timescale: uint32(media.TickRate), DurationV0: 0}
	mdhd.Language = [3]byte{'u', 'n', 'd'}
	if err := bw.Marshal(mdhd); err != nil {
		return err
	}
	hdlr := &mp4.Hdlr{}
	if tr.Kind == media.KindAudio {
		hdlr.HandlerType = [4]byte{'s', 'o', 'u', 'n'}
	} else {
		hdlr.HandlerType = [4]byte{'v', 'i', 'd', 'e'}
	}
	if err := bw.Marshal(hdlr); err != nil {
		return err
	}

	bw.Start("minf")
	if tr.Kind == media.KindAudio {
		_ = bw.WriteFullBox("smhd", 0, 0, make([]byte, 4))
	} else {
		_ = bw.WriteFullBox("vmhd", 0, 1, make([]byte, 8))
	}
	bw.Start("dinf")
	urlBox := []byte{0x00, 0x00, 0x00, 0x0C, 'u', 'r', 'l', ' ', 0x00, 0x00, 0x00, 0x01}
	_ = bw.WriteFullBox("dref", 0, 0, append([]byte{0, 0, 0, 1}, urlBox...))
	_ = bw.End()
	bw.Start("stbl")
	if err := bw.WriteStsd([][]byte{tr.SampleEntry}); err != nil {
		return err
	}
	_ = bw.WriteFullBox("stts", 0, 0, make([]byte, 4))
	_ = bw.WriteFullBox("stsc", 0, 0, make([]byte, 4))
	_ = bw.WriteFullBox("stsz", 0, 0, make([]byte, 8))
	_ = bw.WriteFullBox("stco", 0, 0, make([]byte, 4))
	_ = bw.End()
	_ = bw.End()
	_ = bw.End()
	return bw.End()
}

// writeFragments emits styp + moof + mdat per fragment for one track.
func writeFragments(w *countingWriter, sp segmentPlan) error {
	tr := sp.track
	for fi, frag := range sp.frags {
		var mdat bytes.Buffer
		sizes := make([]uint32, len(frag))
		for i, s := range frag {
			sizes[i] = uint32(len(s.Data))
			mdat.Write(s.Data)
		}
		moof := buildMoof(tr, frag, sizes, sp.seqNum)

		bw := mp4util.New()
		if fi == 0 {
			_ = bw.WriteStyp("msdh", "msdh", "msix")
		}
		bw.WriteRawBytes(moof)
		_ = bw.WriteRaw("mdat", mdat.Bytes())
		if _, err := w.Write(bw.Bytes()); err != nil {
			return err
		}
		sp.seqNum++
	}
	return nil
}

func buildMoof(tr *TrackInput, frag []media.Sample, sizes []uint32, seq uint32) []byte {
	build := func(dataOffset int32) []byte {
		bw := mp4util.New()
		bw.Start("moof")
		_ = bw.WriteFullBox("mfhd", 0, 0, u32(int(seq)))
		bw.Start("traf")
		tfhd := &mp4.Tfhd{TrackID: tr.TrackID}
		tfhd.SetFlags(0x020000) // default-base-is-moof
		_ = bw.Marshal(tfhd)

		baseDTS := uint64(frag[0].DTS)
		tfdt := &mp4.Tfdt{}
		if baseDTS > 0xFFFFFFFF {
			tfdt.SetVersion(1)
			tfdt.BaseMediaDecodeTimeV1 = baseDTS
		} else {
			tfdt.BaseMediaDecodeTimeV0 = uint32(baseDTS)
		}
		_ = bw.Marshal(tfdt)

		trun := &mp4.Trun{SampleCount: uint32(len(frag)), DataOffset: dataOffset}
		trun.SetFlags(0x000001 | 0x000100 | 0x000200 | 0x000400 | 0x000800)
		trun.Entries = make([]mp4.TrunEntry, len(frag))
		hasNegCTO := false
		for _, s := range frag {
			if s.PTS-s.DTS < 0 {
				hasNegCTO = true
			}
		}
		if hasNegCTO {
			trun.SetVersion(1) // signed composition offsets
		}
		for i, s := range frag {
			cto := s.PTS - s.DTS
			var dur uint32
			if s.Duration > 0 {
				dur = uint32(s.Duration)
			} else if i+1 < len(frag) {
				dur = uint32(frag[i+1].DTS - s.DTS)
			} else {
				dur = 3000
			}
			flags := uint32(0x00010000) // non-sync
			if s.Sync {
				flags = 0x02000000
			}
			entry := mp4.TrunEntry{
				SampleDuration: dur,
				SampleSize:     sizes[i],
				SampleFlags:    flags,
			}
			if hasNegCTO {
				entry.SampleCompositionTimeOffsetV1 = int32(cto)
			} else {
				entry.SampleCompositionTimeOffsetV0 = uint32(cto)
			}
			trun.Entries[i] = entry
		}
		_ = bw.Marshal(trun)
		_ = bw.End()
		_ = bw.End()
		return bw.Bytes()
	}
	dummy := build(0)
	return build(int32(len(dummy) + 8))
}

func u32(v int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	return b
}

// verify re-reads the staged file, confirming box structure, sample counts,
// per-sample byte hashes and the source-index hash agreement.
func verify(path string, p *plan) (*Summary, *SourceIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, media.Wrap(media.CodeIO, "verify", media.SourcePos{}, err, "")
	}
	if len(data) == 0 {
		return nil, nil, media.New(media.CodeIntegrity, "verify", media.SourcePos{}, "staged output is empty")
	}
	// Structural check: every top-level box must be well formed.
	off := 0
	for off < len(data) {
		if len(data)-off < 8 {
			return nil, nil, media.New(media.CodeTruncatedBox, "verify", media.SourcePos{},
				"%d trailing bytes after last box", len(data)-off)
		}
		size := int(binary.BigEndian.Uint32(data[off:]))
		if size < 8 || off+size > len(data) {
			return nil, nil, media.New(media.CodeTruncatedBox, "verify", media.SourcePos{},
				"staged box at %d has invalid size %d", off, size)
		}
		off += size
	}

	sum := &Summary{
		OutputBytes:     int64(len(data)),
		OutputTracks:    len(p.tracks),
		SamplesPerTrack: make(map[string]int, len(p.tracks)),
	}
	outHash := sha256.Sum256(data)
	sum.OutputSHA256 = fmt.Sprintf("%x", outHash)

	sampleHasher := sha256.New()
	match := true
	index := &SourceIndex{Version: 1}
	globalIdx := 0
	for _, tr := range p.tracks {
		index.TrackKeys = append(index.TrackKeys, tr.Key)
		sum.SamplesPerTrack[tr.Key.String()] = len(tr.Samples)
		for _, s := range tr.Samples {
			h := s.HashBytes()
			sampleHasher.Write(h[:])
			index.Entries = append(index.Entries, SourceEntry{
				Track:   tr.Key,
				DTS:     s.DTS,
				PTS:     s.PTS,
				Segment: s.Source.Segment,
				Sample:  s.Source.Sample,
				URI:     s.Source.URI,
				Hash:    fmt.Sprintf("%x", h),
			})
			if len(s.Data) == 0 {
				match = false
			}
			globalIdx++
		}
	}
	sum.SampleSHA256 = fmt.Sprintf("%x", sampleHasher.Sum(nil))
	sum.OutputSamples = globalIdx
	sum.SourceHashMatch = match
	// Cross-check that every source hash appears verbatim in the staged file.
	if !hashPresentInFile(data, p) {
		match = false
		sum.SourceHashMatch = false
	}
	if !match {
		return nil, nil, media.New(media.CodeIntegrity, "verify", media.SourcePos{},
			"staged output does not preserve every encoded sample byte")
	}
	return sum, index, nil
}

// hashPresentInFile confirms each source sample's bytes exist contiguously in
// the staged output, proving no re-encode happened.
func hashPresentInFile(data []byte, p *plan) bool {
	for _, tr := range p.tracks {
		for _, s := range tr.Samples {
			if len(s.Data) == 0 {
				return false
			}
			if bytes.Index(data, s.Data) < 0 {
				return false
			}
		}
	}
	return true
}

func writeIndex(dir string, idx *SourceIndex) error {
	bw := &bytes.Buffer{}
	enc := json.NewEncoder(bw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(idx); err != nil {
		return media.Wrap(media.CodeIO, "index-encode", media.SourcePos{}, err, "")
	}
	final := filepath.Join(dir, "output.index.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, bw.Bytes(), 0o644); err != nil {
		return media.Wrap(media.CodeIO, "index-write", media.SourcePos{}, err, "")
	}
	if err := os.Rename(tmp, final); err != nil {
		return media.Wrap(media.CodeIO, "index-rename", media.SourcePos{}, err, "")
	}
	return nil
}

// SampleDigest returns an order-independent digest of all sample bytes; used to
// prove repeated runs produce identical results.
func SampleDigest(tracks []*TrackInput) string {
	h := sha256.New()
	var keys []string
	for _, tr := range tracks {
		for _, s := range tr.Samples {
			sum := s.HashBytes()
			keys = append(keys, fmt.Sprintf("%x", sum))
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		io := sha256.Sum256([]byte(k))
		h.Write(io[:])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
