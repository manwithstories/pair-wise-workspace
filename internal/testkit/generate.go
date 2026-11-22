// Package testkit generates every fixture medialine uses. Nothing is read
// from disk or the network: normal, corrupt and boundary-case samples are all
// produced deterministically from a seed.
package testkit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
)

// SampleMarker is the first four bytes of every synthetic sample payload. Its
// presence lets the reader prove a sample was not silently replaced.
var SampleMarker = [4]byte{'M', 'L', 'N', '1'}

// TrackSpec describes one media track to synthesise.
type TrackSpec struct {
	ID         int
	Kind       string // "video" or "audio"
	Timescale  uint32
	DurationUS int64
	// FrameDurUS is the nominal sample duration.
	FrameDurUS int64
	// GOP is the keyframe interval in samples (video only).
	GOP int
	// SampleBytes is the nominal payload size.
	SampleBytes int
}

// ClipSpec is one source clip to generate.
type ClipSpec struct {
	Name   string
	Seed   int64
	Tracks []TrackSpec
}

// DefaultVideoTrack returns a 30fps video track spec.
func DefaultVideoTrack(id int, durationUS int64, gop int) TrackSpec {
	return TrackSpec{ID: id, Kind: "video", Timescale: 90_000, DurationUS: durationUS, FrameDurUS: 33_333, GOP: gop, SampleBytes: 512}
}

// DefaultAudioTrack returns a 48kHz audio track spec.
func DefaultAudioTrack(id int, durationUS int64) TrackSpec {
	return TrackSpec{ID: id, Kind: "audio", Timescale: 48_000, DurationUS: durationUS, FrameDurUS: 21_333, GOP: 0, SampleBytes: 192}
}

// Clip is a generated source file plus its layout metadata.
type Clip struct {
	Spec    ClipSpec
	Path    string
	Size    int64
	Samples int
	payload []byte
	offsets []int64
	sizes   []int
}

// GenerateClip writes a minimal but structurally valid MP4 (ftyp + mdat +
// moov + mfra) for the spec into dir.
func GenerateClip(dir string, spec ClipSpec) (*Clip, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	build := newBuilder(spec)
	if err := build.layout(); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, spec.Name+".mp4")
	data := build.assemble()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	return &Clip{Spec: spec, Path: path, Size: int64(len(data)), Samples: build.sampleCount, payload: build.payload}, nil
}

// MarkerObserved reports whether the on-disk sample at offset begins with the
// fixture marker.
func MarkerObserved(path string, offset int64) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var b [4]byte
	if _, err := f.ReadAt(b[:], offset); err != nil {
		return false, err
	}
	return b == SampleMarker, nil
}

type samplePlan struct {
	durTicks uint32
	size     int
	keyframe bool
}

type builder struct {
	spec        ClipSpec
	plans       [][]samplePlan
	payload     []byte
	sampleCount int
}

func newBuilder(spec ClipSpec) *builder {
	return &builder{spec: spec}
}

func (b *builder) layout() error {
	rng := rand.New(rand.NewSource(b.spec.Seed))
	b.plans = make([][]samplePlan, len(b.spec.Tracks))
	// Payload region is filled after mdat header placement; record plans only.
	for i, ts := range b.spec.Tracks {
		if ts.Timescale == 0 || ts.FrameDurUS <= 0 || ts.DurationUS <= 0 {
			return fmt.Errorf("track %d has invalid timing", ts.ID)
		}
		tickDur := int64(ts.Timescale) * ts.FrameDurUS / 1_000_000
		if tickDur <= 0 {
			tickDur = 1
		}
		n := int(ts.DurationUS / ts.FrameDurUS)
		if n < 1 {
			n = 1
		}
		for k := 0; k < n; k++ {
			kf := ts.Kind == "audio" || ts.GOP <= 0 || k%ts.GOP == 0
			size := ts.SampleBytes
			if k > 0 {
				// Deterministic size jitter keeps hashes sensitive to content.
				size += rng.Intn(ts.SampleBytes/8 + 1)
			}
			b.plans[i] = append(b.plans[i], samplePlan{durTicks: uint32(tickDur), size: size, keyframe: kf})
			b.sampleCount++
		}
	}
	return nil
}

// box serialisation helpers

func box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	return out
}

func fullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	hdr := make([]byte, 4)
	hdr[0] = version
	hdr[1] = byte(flags >> 16)
	hdr[2] = byte(flags >> 8)
	hdr[3] = byte(flags)
	return box(typ, append([][]byte{hdr}, payload...)...)
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

func (b *builder) assemble() []byte {
	ftyp := box("ftyp", []byte("isom"), u32(0x200), []byte("isom"), []byte("iso6"), []byte("mp41"))
	dataStart := len(ftyp) + 8
	offsets, _ := b.chunkOffsets(dataStart)
	moov := b.buildMoov(dataStart, offsets)
	mdatPayload := b.buildPayload(offsets)
	mdat := make([]byte, 8+len(mdatPayload))
	binary.BigEndian.PutUint32(mdat, uint32(len(mdat)))
	copy(mdat[4:8], "mdat")
	copy(mdat[8:], mdatPayload)

	var out bytes.Buffer
	out.Write(ftyp)
	out.Write(mdat)
	out.Write(moov)
	out.Write(b.buildMfra(len(ftyp) + len(mdat) + len(moov)))
	return out.Bytes()
}

// chunkOffsets places every sample in its own chunk so offsets are explicit.
func (b *builder) chunkOffsets(dataStart int) ([][]int64, [][]int) {
	offsets := make([][]int64, len(b.plans))
	sizes := make([][]int, len(b.plans))
	cur := dataStart
	for i := range b.plans {
		for _, p := range b.plans[i] {
			offsets[i] = append(offsets[i], int64(cur))
			sizes[i] = append(sizes[i], p.size)
			cur += p.size
		}
	}
	return offsets, sizes
}

func (b *builder) buildPayload(offsets [][]int64) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, 1))
	rng := rand.New(rand.NewSource(b.spec.Seed ^ 0x5eed))
	for i := range b.plans {
		for _, p := range b.plans[i] {
			body := make([]byte, p.size)
			rng.Read(body)
			copy(body, SampleMarker[:])
			buf.Write(body)
		}
	}
	return buf.Bytes()
}

func (b *builder) buildMoov(dataStart int, offsets [][]int64) []byte {
	mvhd := fullBox("mvhd", 0, 0,
		u32(0), u32(0), u32(1000), u32(uint32(b.spanMS())),
		u32(0x00010000), u16(0x0100), u16(0), u32(0), u32(0),
		identityMatrix(), make([]byte, 24), u32(uint32(b.maxTrackID()+1)))

	trakParts := make([][]byte, 0, len(b.spec.Tracks))
	for i, ts := range b.spec.Tracks {
		trakParts = append(trakParts, b.buildVideoOrAudioTrak(i, ts, dataStart, offsets[i]))
	}

	parts := make([][]byte, 0, len(trakParts)+1)
	parts = append(parts, mvhd)
	parts = append(parts, trakParts...)
	return box("moov", parts...)
}

func identityMatrix() []byte {
	vals := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	var out []byte
	for _, v := range vals {
		out = append(out, u32(v)...)
	}
	return out
}

func (b *builder) spanMS() int64 {
	var max int64
	for _, ts := range b.spec.Tracks {
		if ts.DurationUS > max {
			max = ts.DurationUS
		}
	}
	return max / 1000
}

func (b *builder) maxTrackID() int {
	max := 0
	for _, ts := range b.spec.Tracks {
		if ts.ID > max {
			max = ts.ID
		}
	}
	return max
}

func (b *builder) buildVideoOrAudioTrak(i int, ts TrackSpec, dataStart int, offsets []int64) []byte {
	plans := b.plans[i]
	durTicks := uint64(0)
	for _, p := range plans {
		durTicks += uint64(p.durTicks)
	}
	tkhd := fullBox("tkhd", 0, 0x3,
		u32(0), u32(0), u32(uint32(ts.ID)), u32(0),
		u32(uint32(durTicks)), u32(0), u32(0),
		u16(0), u16(0), u16(0x0100), u16(0),
		identityMatrix(), u32(uint32(1920)<<16), u32(uint32(1080)<<16))

	mdhd := fullBox("mdhd", 0, 0, u32(0), u32(0), u32(ts.Timescale), u32(uint32(durTicks)), u16(0x55c4), u16(0))
	hdlrType := "vide"
	if ts.Kind == "audio" {
		hdlrType = "soun"
	}
	hdlr := fullBox("hdlr", 0, 0, u32(0), []byte(hdlrType), make([]byte, 12), []byte("medialine\x00"))

	var stsd []byte
	if ts.Kind == "audio" {
		stsd = fullBox("stsd", 0, 0, u32(1), audioSampleEntry(ts.Timescale))
	} else {
		stsd = fullBox("stsd", 0, 0, u32(1), videoSampleEntry())
	}

	stts := b.buildStts(plans)
	stsc := b.buildStsc(len(plans))
	stsz := b.buildStsz(plans)
	var chunkOffsets []byte
	for _, o := range offsets {
		chunkOffsets = append(chunkOffsets, u32(uint32(o))...)
	}
	stco := fullBox("stco", 0, 0, u32(uint32(len(offsets))), chunkOffsets)
	stss := b.buildStss(plans)

	mediaHdr := fullBox("vmhd", 0, 1, u16(0), make([]byte, 6))
	dinf := box("dinf", fullBox("dref", 0, 0, u32(1), fullBox("url ", 0, 1)))
	if ts.Kind == "audio" {
		mediaHdr = fullBox("smhd", 0, 0, u16(0), u16(0))
	}
	minf := box("minf", mediaHdr, dinf, box("stbl", stsd, stts, stsc, stsz, stco, stss))
	mdia := box("mdia", mdhd, hdlr, minf)
	return box("trak", tkhd, mdia)
}

func (b *builder) buildStts(plans []samplePlan) []byte {
	// Collapse runs of equal duration into one entry.
	var entries []byte
	count := uint32(0)
	var runs int
	var prev uint32
	for i, p := range plans {
		if i == 0 || p.durTicks != prev {
			if i > 0 {
				entries = append(entries, u32(count)...)
				entries = append(entries, u32(prev)...)
				runs++
			}
			prev = p.durTicks
			count = 1
			continue
		}
		count++
	}
	if len(plans) > 0 {
		entries = append(entries, u32(count)...)
		entries = append(entries, u32(prev)...)
		runs++
	}
	return fullBox("stts", 0, 0, u32(uint32(runs)), entries)
}

func (b *builder) buildStsc(n int) []byte {
	if n == 0 {
		return fullBox("stsc", 0, 0, u32(0))
	}
	return fullBox("stsc", 0, 0, u32(1), u32(1), u32(1), u32(1))
}

func (b *builder) buildStsz(plans []samplePlan) []byte {
	var sizes []byte
	for _, p := range plans {
		sizes = append(sizes, u32(uint32(p.size))...)
	}
	return fullBox("stsz", 0, 0, u32(0), u32(uint32(len(plans))), sizes)
}

func (b *builder) buildStss(plans []samplePlan) []byte {
	var nums []byte
	n := 0
	for i, p := range plans {
		if p.keyframe {
			nums = append(nums, u32(uint32(i+1))...)
			n++
		}
	}
	return fullBox("stss", 0, 0, u32(uint32(n)), nums)
}

func (b *builder) buildMfra(mfraOffset int) []byte {
	var trafs []byte
	for i, ts := range b.spec.Tracks {
		if len(b.plans[i]) == 0 {
			continue
		}
		var entries []byte
		entries = append(entries, u64(0)...) // time
		entries = append(entries, u64(0)...) // moof offset (unused by medialine)
		entries = append(entries, []byte{0, 0, 0}...)
		tfra := fullBox("tfra", 1, 0, u32(uint32(ts.ID)), u32(0), u32(1), entries)
		trafs = append(trafs, box("traf", tfra)...)
	}
	mfro := fullBox("mfro", 0, 0, u32(0))
	body := box("mfra", append([][]byte{trafs}, mfro)...)
	// mfro size must equal the whole mfra box.
	binary.BigEndian.PutUint32(body[len(body)-4:], uint32(len(body)))
	return body
}

func videoSampleEntry() []byte {
	entry := make([]byte, 78)
	binary.BigEndian.PutUint32(entry, uint32(len(entry)))
	copy(entry[4:8], "avc1")
	binary.BigEndian.PutUint16(entry[8:10], 0)
	binary.BigEndian.PutUint16(entry[10:12], 1)
	binary.BigEndian.PutUint16(entry[12:14], 0)
	binary.BigEndian.PutUint16(entry[14:16], 0)
	binary.BigEndian.PutUint16(entry[24:26], 1920)
	binary.BigEndian.PutUint16(entry[26:28], 1080)
	binary.BigEndian.PutUint32(entry[28:32], 0x00480000)
	binary.BigEndian.PutUint32(entry[32:36], 0x00480000)
	binary.BigEndian.PutUint16(entry[40:42], 1)
	return entry
}

func audioSampleEntry(rate uint32) []byte {
	entry := make([]byte, 36)
	binary.BigEndian.PutUint32(entry, uint32(len(entry)))
	copy(entry[4:8], "mp4a")
	binary.BigEndian.PutUint16(entry[10:12], 1)
	binary.BigEndian.PutUint16(entry[16:18], 2)
	binary.BigEndian.PutUint16(entry[18:20], 16)
	binary.BigEndian.PutUint32(entry[24:28], rate<<16)
	return entry
}

// SortedTrackIDs returns track IDs in ascending order.
func (c *Clip) SortedTrackIDs() []int {
	ids := make([]int, 0, len(c.Spec.Tracks))
	for _, t := range c.Spec.Tracks {
		ids = append(ids, t.ID)
	}
	sort.Ints(ids)
	return ids
}

// CorruptTail truncates the file by n bytes, simulating a tail that lost an
// index or the final fragment.
func CorruptTail(path string, n int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if n <= 0 || n >= st.Size() {
		return fmt.Errorf("corrupt length %d invalid for file size %d", n, st.Size())
	}
	return os.Truncate(path, st.Size()-n)
}

// CorruptLength rewrites the declared size of the box starting at offset to
// an oversized value, exercising the invalid-length guard.
func CorruptLength(path string, offset int64, declared uint32) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], declared)
	_, err = f.WriteAt(b[:], offset)
	return err
}

// CorruptSampleBytes flips the marker of the sample at offset so integrity
// checking can detect it.
func CorruptSampleBytes(path string, offset int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteAt([]byte{0xde, 0xad, 0xbe, 0xef}, offset)
	return err
}
