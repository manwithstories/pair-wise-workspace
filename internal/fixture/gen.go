// Package fixture generates deterministic multi-representation fMP4
// fixtures: distinct timescales, divergent first-segment offsets, one
// sequence-number gap, one segment without tfdt and a short tail.
package fixture

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
)

const (
	segmentCount  = 60
	samplesPerSeg = 6
	nominalMs     = 6000.0
	lastMs        = 3000.0 // short tail on the final segment
	gapRep        = 1      // r2 loses one segment file
	gapSeq        = 17
	noTfdtRep     = 2 // r3 has one segment without tfdt
	noTfdtSeq     = 30
)

type repSpec struct {
	id        string
	bandwidth uint64
	timescale uint32
	offsetMs  float64 // first-segment decode offset, differs per rep
	ampMs     float64 // boundary jitter amplitude
	freq      float64 // boundary jitter angular frequency
}

var reps = []repSpec{
	{id: "r1", bandwidth: 400000, timescale: 1000, offsetMs: 0, ampMs: 0.9, freq: 0.7},
	{id: "r2", bandwidth: 800000, timescale: 12800, offsetMs: 500, ampMs: 0.7, freq: 0.9},
	{id: "r3", bandwidth: 1500000, timescale: 90000, offsetMs: 1250, ampMs: 0.5, freq: 1.1},
	{id: "r4", bandwidth: 3000000, timescale: 90000, offsetMs: 2750, ampMs: 0.3, freq: 1.3},
}

// Generate writes the fixture tree under out. The seed perturbs the jitter
// frequencies so different seeds yield different but valid fixtures.
func Generate(out string, seed int64) error {
	rng := rand.New(rand.NewSource(seed))
	type meta struct {
		ID        string `json:"id"`
		Dir       string `json:"dir"`
		Bandwidth uint64 `json:"bandwidth"`
	}
	metas := make([]meta, 0, len(reps))
	for ri, spec := range reps {
		freq := spec.freq + 0.05*rng.Float64()
		dir := filepath.Join(out, spec.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("gen: %w", err)
		}
		bounds := boundaries(spec, freq)
		if err := writeInit(dir, spec, bounds); err != nil {
			return err
		}
		for seq := 1; seq <= segmentCount; seq++ {
			if ri == gapRep && seq == gapSeq {
				continue // sequence-number gap: file never written
			}
			withTfdt := !(ri == noTfdtRep && seq == noTfdtSeq)
			if err := writeSegment(dir, spec, seq, bounds, withTfdt); err != nil {
				return err
			}
		}
		metas = append(metas, meta{ID: spec.id, Dir: spec.id, Bandwidth: spec.bandwidth})
	}
	idx, err := json.MarshalIndent(map[string]any{"representations": metas}, "", "  ")
	if err != nil {
		return err
	}
	idx = append(idx, '\n')
	if err := os.WriteFile(filepath.Join(out, "fixtures.json"), idx, 0o644); err != nil {
		return fmt.Errorf("gen: %w", err)
	}
	return nil
}

// boundaries returns segment start ticks for every boundary 0..segmentCount
// in the representation timescale, including the per-rep decode offset.
func boundaries(spec repSpec, freq float64) []uint64 {
	offset := spec.offsetMs * float64(spec.timescale) / 1000
	out := make([]uint64, segmentCount+1)
	for i := 0; i <= segmentCount; i++ {
		ms := float64(i) * nominalMs
		if i == segmentCount {
			ms = float64(segmentCount-1)*nominalMs + lastMs
		}
		ms += spec.ampMs * math.Sin(freq*float64(i))
		out[i] = uint64(math.Round(offset + ms*float64(spec.timescale)/1000))
	}
	return out
}

func writeInit(dir string, spec repSpec, bounds []uint64) error {
	total := bounds[segmentCount] - bounds[0]
	mvhd := fullBox("mvhd", 0, 0, concat(
		u32(0), u32(0), // creation + modification time
		u32(spec.timescale), u32(uint32(total)),
		u32(0x00010000),     // rate 1.0
		u16(0x0100), u16(0), // volume 1.0 + reserved
		u32(0), u32(0), // reserved
		matrix(),
		make([]byte, 24), // pre_defined
		u32(2),           // next_track_ID
	))
	trex := fullBox("trex", 0, 0, concat(
		u32(1), u32(1),
		u32(uint32(nominalMs*float64(spec.timescale)/1000)), // default sample duration
		u32(0), u32(0),
	))
	moov := box("moov", concat(mvhd, box("mvex", trex)))
	ftyp := box("ftyp", concat([]byte("isom"), u32(0), []byte("isomiso6mp41")))
	data := concat(ftyp, moov)
	path := filepath.Join(dir, fmt.Sprintf("init-%s.mp4", spec.id))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("gen: %w", err)
	}
	return nil
}

func writeSegment(dir string, spec repSpec, seq int, bounds []uint64, withTfdt bool) error {
	start := bounds[seq-1]
	dur := bounds[seq] - bounds[seq-1]
	mfhd := fullBox("mfhd", 0, 0, u32(uint32(seq)))
	tfhd := fullBox("tfhd", 0, 0x020000, u32(1)) // default-base-is-moof
	var tfdt []byte
	if withTfdt {
		tfdt = fullBox("tfdt", 1, 0, u64(start))
	}
	trun := buildTrun(dur)
	moof := box("moof", concat(mfhd, box("traf", concat(tfhd, tfdt, trun))))
	mdat := box("mdat", make([]byte, samplesPerSeg*16))
	name := fmt.Sprintf("seg-%s-%d.m4s", spec.id, start)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, concat(moof, mdat), 0o644); err != nil {
		return fmt.Errorf("gen: %w", err)
	}
	return nil
}

// buildTrun writes a trun with per-sample durations summing to dur.
func buildTrun(dur uint64) []byte {
	per := dur / samplesPerSeg
	rem := dur % samplesPerSeg
	payload := u32(samplesPerSeg)
	for i := 0; i < samplesPerSeg; i++ {
		d := per
		if uint64(i) < rem {
			d++
		}
		payload = append(payload, u32(uint32(d))...)
	}
	return fullBox("trun", 0, 0x000100, payload) // sample_duration_present
}

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(head, payload...))
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func matrix() []byte {
	vals := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	var out []byte
	for _, v := range vals {
		out = append(out, u32(v)...)
	}
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
