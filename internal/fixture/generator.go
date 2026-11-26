// Package fixture generates deterministic CMAF/fMP4 sample streams:
// a clean stream and a faulty stream with injected gap, rollback,
// duplicate, truncate, loss and map-conflict faults. All sample data is
// generated in code; no external files are referenced.
package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"relaycheck/internal/remux"
)

// Stream layout constants shared by the generator and the tests.
const (
	VideoTrackID   = 1
	AudioTrackID   = 2
	VideoTimescale = 90000
	AudioTimescale = 48000
	VideoPerSeg    = 30 // 30 x 3000 = 90000 (1s)
	AudioPerSeg    = 50 // 50 x 960 = 48000 (1s)
	VideoSampleDur = 3000
	AudioSampleDur = 960
	SegDurVideo    = VideoPerSeg * VideoSampleDur
	SegDurAudio    = AudioPerSeg * AudioSampleDur
)

// Options controls fixture generation.
type Options struct {
	Out      string
	Seed     int64
	Segments int
	Faults   []string
}

// FaultSpec describes one injected fault.
type FaultSpec struct {
	Type string `json:"type"`
	Seq  uint32 `json:"seq"`
}

// Manifest records what was generated; it is written to faulty/faults.json.
type Manifest struct {
	Seed     int64       `json:"seed"`
	Segments int         `json:"segments"`
	Faults   []FaultSpec `json:"faults"`
}

// SegmentName is the canonical media-segment file name for a position.
func SegmentName(seq uint32) string {
	return fmt.Sprintf("seg_%06d.m4s", seq)
}

func buildSegment(rng *rand.Rand, seq uint32) ([]byte, error) {
	video := make([]remux.OutSample, VideoPerSeg)
	for j := range video {
		size := 800 + rng.Intn(3200)
		data := make([]byte, size)
		rng.Read(data)
		var cto int32
		switch j % 3 {
		case 0:
			cto = 2 * VideoSampleDur
		case 2:
			cto = VideoSampleDur
		}
		flags := uint32(0x1010000)
		if j == 0 {
			flags = 0x2000000
		}
		video[j] = remux.OutSample{Duration: VideoSampleDur, Size: uint32(size), Flags: flags, CTO: cto, Data: data}
	}
	audio := make([]remux.OutSample, AudioPerSeg)
	for j := range audio {
		size := 100 + rng.Intn(200)
		data := make([]byte, size)
		rng.Read(data)
		audio[j] = remux.OutSample{Duration: AudioSampleDur, Size: uint32(size), Flags: 0x2000000, Data: data}
	}
	frags := []remux.OutFrag{
		{TrackID: VideoTrackID, TFDT: uint64(seq-1) * SegDurVideo, Samples: video},
		{TrackID: AudioTrackID, TFDT: uint64(seq-1) * SegDurAudio, Samples: audio},
	}
	return remux.SegmentBytes(seq, frags)
}

// patchUint32 rewrites the uint32 field that follows the box header and
// version/flags of the n-th box of the given type (mfhd seq, tfhd track).
func patchUint32(data []byte, typ string, n int, value uint32) {
	needle := []byte(typ)
	off := 0
	for i := 0; i <= n; i++ {
		idx := bytes.Index(data[off:], needle)
		if idx < 0 {
			panic("fixture: box not found: " + typ)
		}
		off += idx
		if i == n {
			binary.BigEndian.PutUint32(data[off+8:], value)
			return
		}
		off += 4
	}
}

// patchUint64 rewrites the uint64 field of the n-th box (tfdt v1 value).
func patchUint64(data []byte, typ string, n int, value uint64) {
	needle := []byte(typ)
	off := 0
	for i := 0; i <= n; i++ {
		idx := bytes.Index(data[off:], needle)
		if idx < 0 {
			panic("fixture: box not found: " + typ)
		}
		off += idx
		if i == n {
			binary.BigEndian.PutUint64(data[off+8:], value)
			return
		}
		off += 4
	}
}

func writeFile(dir, name string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

// DigestFile hashes one generated file; used by tests.
func DigestFile(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Generate builds the clean and faulty streams under opts.Out.
func Generate(opts Options) (*Manifest, error) {
	if opts.Segments < 16 {
		return nil, fmt.Errorf("need at least 16 segments")
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	segs := make([][]byte, opts.Segments+1)
	for i := 1; i <= opts.Segments; i++ {
		b, err := buildSegment(rng, uint32(i))
		if err != nil {
			return nil, err
		}
		segs[i] = b
	}
	cleanDir := filepath.Join(opts.Out, "clean")
	faultyDir := filepath.Join(opts.Out, "faulty")
	for _, d := range []string{cleanDir, faultyDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	initBytes, err := BuildInit()
	if err != nil {
		return nil, err
	}
	for _, d := range []string{cleanDir, faultyDir} {
		if err := writeFile(d, "init.mp4", initBytes); err != nil {
			return nil, err
		}
	}
	for i := 1; i <= opts.Segments; i++ {
		if err := writeFile(cleanDir, SegmentName(uint32(i)), segs[i]); err != nil {
			return nil, err
		}
	}
	// pick distinct fault positions away from head and tail
	lo := opts.Segments/8 + 1
	hi := opts.Segments * 7 / 8
	perm := rand.New(rand.NewSource(opts.Seed + 1)).Perm(hi - lo + 1)
	m := &Manifest{Seed: opts.Seed, Segments: opts.Segments}
	lost := map[int]bool{}
	dupe := map[int]bool{}
	next := 0
	per := opts.Segments / 64
	if per < 1 {
		per = 1
	}
	take := func() []int {
		out := make([]int, per)
		for k := range out {
			out[k] = lo + perm[next]
			next++
		}
		return out
	}
	for _, f := range opts.Faults {
		for _, pos := range take() {
			m.Faults = append(m.Faults, FaultSpec{Type: f, Seq: uint32(pos)})
			switch f {
			case "loss":
				lost[pos] = true
			case "duplicate":
				dupe[pos] = true
			}
		}
	}
	for _, spec := range m.Faults {
		i := int(spec.Seq)
		data := segs[i]
		switch spec.Type {
		case "gap":
			data = bytes.Clone(data)
			patchUint32(data, "mfhd", 0, uint32(i)+1)
		case "rollback":
			data = bytes.Clone(data)
			back := uint64(i - 6)
			patchUint64(data, "tfdt", 0, back*SegDurVideo)
			patchUint64(data, "tfdt", 1, back*SegDurAudio)
		case "truncate":
			data = data[:len(data)*2/5]
		case "map-conflict":
			data = bytes.Clone(data)
			patchUint32(data, "tfhd", 1, 99)
		case "loss":
			continue
		case "duplicate":
			data = segs[i]
		default:
			return nil, fmt.Errorf("unknown fault type %q", spec.Type)
		}
		if err := writeFile(faultyDir, SegmentName(uint32(i)), data); err != nil {
			return nil, err
		}
	}
	for i := 1; i <= opts.Segments; i++ {
		if lost[i] {
			continue
		}
		name := SegmentName(uint32(i))
		if _, err := os.Stat(filepath.Join(faultyDir, name)); os.IsNotExist(err) {
			if err := writeFile(faultyDir, name, segs[i]); err != nil {
				return nil, err
			}
		}
		if dupe[i] {
			if err := writeFile(faultyDir, fmt.Sprintf("seg_%06d_dup1.m4s", i), segs[i]); err != nil {
				return nil, err
			}
		}
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFile(faultyDir, "faults.json", mb); err != nil {
		return nil, err
	}
	return m, nil
}
