package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// Builder runs the fan-out -> RTP probe -> MP4 segment pipeline with
// checkpointed, atomic recovery and deterministic hashing.
type Builder struct {
	Dir          string
	Seed         uint64
	programTicks uint64
	streams      []*Stream
	plan         *Plan
	cp           *Checkpoint
	faults       []*base.Fault
	segments     []*Segment
	RTP          map[string]*RTPProbe
}

// NewBuilder prepares an output directory and loads any existing checkpoint.
// programTicks is the total program length on the 90 kHz clock; it fixes the
// segment duration so each rendition yields TargetSegmentsPerRendition.
func NewBuilder(dir string, seed uint64, programTicks uint64, streams []*Stream) (*Builder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b := &Builder{
		Dir: dir, Seed: seed, programTicks: programTicks, streams: streams,
		cp:  &Checkpoint{Seed: seed, NextSegment: map[string]int{}, SegmentHashes: map[string]string{}},
		RTP: map[string]*RTPProbe{},
	}
	for _, s := range streams {
		b.cp.NextSegment[s.Rendition] = 0
		b.RTP[s.Rendition] = NewRTPProbe(rtpSSRC(s.Rendition), 32)
	}
	if err := b.loadCheckpoint(); err != nil {
		return nil, err
	}
	b.plan = FanOutWithDuration(streams, b.segmentDuration())
	return b, nil
}

// segmentDuration divides the program into the target per-rendition count,
// rounding up so the final fragment absorbs the remainder.
func (b *Builder) segmentDuration() uint64 {
	if b.programTicks == 0 {
		return SegmentDuration90k
	}
	d := b.programTicks / TargetSegmentsPerRendition
	if d == 0 {
		d = b.programTicks
	}
	return d
}

// Plan exposes the shared boundary plan for tests and reporting.
func (b *Builder) Plan() *Plan { return b.plan }

// Faults returns every fault observed during the build.
func (b *Builder) Faults() []*base.Fault { return b.faults }

// Segments returns all successfully committed segments in build order.
func (b *Builder) Segments() []*Segment { return b.segments }

// Build executes every rendition while preserving a deterministic global
// order: boundary index first, then rendition name.
func (b *Builder) Build() error {
	names := make([]string, 0, len(b.streams))
	byName := map[string]*Stream{}
	for _, s := range b.streams {
		names = append(names, s.Rendition)
		byName[s.Rendition] = s
	}
	for bi, bound := range b.plan.Boundaries {
		for _, name := range names {
			s := byName[name]
			rng := b.plan.Ranges[name][bi]
			if rng[0] == rng[1] {
				continue // rendition has no units in this boundary window
			}
			if bi < b.cp.NextSegment[name] {
				continue // already committed in a previous run
			}
			units := s.Units[rng[0]:rng[1]]
			// A fragment whose first unit is not a keyframe can only exist
			// after a fault-driven resync; label it as recovery.
			recovery := len(units) > 0 && !units[0].Keyframe
			seg := &Segment{
				Camera: s.Camera, Rendition: name, Codec: s.Codec, Index: bi,
				StartPTS: bound.PTS90k, EndPTS: b.endPTS(name, bi),
				Gap: bound.Gap, Recovery: recovery, Color: s.Color, Units: units,
			}
			if err := b.probeAndCommit(seg); err != nil {
				return err
			}
		}
	}
	return b.persistCheckpoint()
}

func (b *Builder) endPTS(name string, bi int) uint64 {
	if bi+1 < len(b.plan.Boundaries) {
		return b.plan.Boundaries[bi+1].PTS90k
	}
	rng := b.plan.Ranges[name][bi]
	units := b.streamByName(name).Units
	if rng[1] > rng[0] {
		last := units[rng[1]-1]
		return last.PTS90k + base.ClockRate/30
	}
	return b.plan.Boundaries[bi].PTS90k
}

func (b *Builder) streamByName(name string) *Stream {
	for _, s := range b.streams {
		if s.Rendition == name {
			return s
		}
	}
	return nil
}

// probeAndCommit runs the in-process RTP check, writes the segment to a temp
// file, hashes it and atomically renames it into place with the checkpoint.
func (b *Builder) probeAndCommit(seg *Segment) error {
	probe := b.RTP[seg.Rendition]
	var seq uint16
	for _, u := range seg.Units {
		loc := base.Locator{Camera: seg.Camera, Rendition: seg.Rendition, PTS: u.PTS90k, Offset: u.Offset}
		for _, pkt := range SendRTP(probe.ssrc, &seq, uint32(u.PTS90k), payloadType(seg.Codec), u.Payload, 1200) {
			_, faults := probe.Push(pkt, loc)
			for _, f := range faults {
				b.faults = append(b.faults, f)
				b.cp.Faults = append(b.cp.Faults, Record(f))
			}
		}
	}

	tmp, err := os.CreateTemp(b.Dir, ".seg-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := WriteSegment(tmp, seg); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	raw, err := os.ReadFile(tmpName)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	seg.Hash = sum[:]

	finalName := filepath.Join(b.Dir, SegmentFileName(seg.Rendition, seg.Index))
	if err := os.Rename(tmpName, finalName); err != nil {
		return err
	}
	seg.Path = finalName
	b.segments = append(b.segments, seg)

	key := hashKey(seg.Rendition, seg.Index)
	if prev, ok := b.cp.SegmentHashes[key]; ok && prev != hex.EncodeToString(seg.Hash) {
		return errors.Join(errRecheck, base.NewFault(base.FaultBoundary,
			base.Locator{Camera: seg.Camera, Rendition: seg.Rendition, PTS: seg.StartPTS},
			false, "segment %s hash changed across resume", key))
	}
	b.cp.SegmentHashes[key] = hex.EncodeToString(seg.Hash)
	b.cp.NextSegment[seg.Rendition] = seg.Index + 1
	return b.persistCheckpoint()
}

// SegmentFileName is the deterministic on-disk naming convention.
func SegmentFileName(rendition string, index int) string {
	return filepath.Base(rendition) + "-seg-" + padIndex(index) + ".m4s"
}

func padIndex(i int) string {
	digits := []byte("000000")
	s := strconvItoa(i)
	if len(s) >= len(digits) {
		return s
	}
	return string(digits[:len(digits)-len(s)]) + s
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func hashKey(rendition string, index int) string {
	return rendition + "/" + strconvItoa(index)
}

func payloadType(c ingest.Codec) uint8 {
	switch c {
	case ingest.CodecH264:
		return 96
	case ingest.CodecH265:
		return 98
	case ingest.CodecAV1:
		return 99
	}
	return 96
}

func rtpSSRC(rendition string) uint32 {
	h := sha256.Sum256([]byte(rendition))
	return uint32(h[0])<<24 | uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3])
}
