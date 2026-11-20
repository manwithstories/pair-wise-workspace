package fixture

import (
	"os"
	"path/filepath"

	"github.com/mediamend/mediamend/internal/model"
)

// Generate writes the full dataset described by opts and returns the project
// manifest and the exact list of sequence numbers whose checksum was corrupted.
// The dataset is deterministic for a fixed (seed, framesPerTrack, every).
func Generate(opts Options) (*Project, []uint64, error) {
	if opts.FramesPerTrack == 0 {
		opts.FramesPerTrack = DefaultFramesPerTrack
	}
	if err := os.MkdirAll(filepath.Join(opts.Root, "input"), 0o755); err != nil {
		return nil, nil, err
	}
	proj := defaultProject(opts.Root, opts.FramesPerTrack)
	corruptSet := CorruptSeq(opts.FramesPerTrack, opts.CorruptEvery)
	corrupt := map[uint64]bool{}
	for _, s := range corruptSet {
		corrupt[s] = true
	}

	for ti := range proj.Tracks {
		spec := &proj.Tracks[ti]
		if err := writeTrack(opts, spec, corrupt); err != nil {
			return nil, nil, err
		}
	}
	path, err := writeProject(opts.Root, proj)
	if err != nil {
		return nil, nil, err
	}
	_ = path
	return proj, corruptSet, nil
}

// writeTrack emits one JSONL track and applies the controlled defects.
func writeTrack(opts Options, spec *TrackSpec, corrupt map[uint64]bool) error {
	f, err := os.Create(spec.File)
	if err != nil {
		return err
	}
	defer f.Close()

	ssrc := trackSSRC(spec.ID)
	rolloverAt := opts.FramesPerTrack / 2
	dupAt := uint64(77)
	backAt := uint64(133)
	missAt := uint64(210) // skip this seq to create a gap
	writtenSeq := uint64(0)

	for seq := uint64(0); seq < opts.FramesPerTrack; seq++ {
		if opts.InjectMissing && seq == missAt && spec.Kind == "video" && spec.ID == "v1" {
			continue // gap: seq is simply absent
		}
		deviceTS := seq * spec.FrameStep
		// Simulate a 32-bit device clock: wrap near the modulus boundary.
		if opts.InjectRollover && spec.Kind == "video" && seq >= rolloverAt {
			base := model.RTPMod - (rolloverAt * spec.FrameStep)
			deviceTS = (base + seq*spec.FrameStep) % model.RTPMod
		}
		if opts.InjectBackwards && seq == backAt && spec.ID == "v1" {
			deviceTS = (seq - 10) * spec.FrameStep // jump backwards
		}

		pkt := rtpPacket(ssrc, uint16(writtenSeq), uint32(deviceTS), seq)
		sha := checksum(pkt)
		if corrupt[seq] {
			sha = "0000000000000000000000000000000000000000000000000000000000000000"
		}
		rec := record{
			TrackID: spec.ID, Seq: seq, DeviceTS: deviceTS, Rate: spec.Rate,
			RTPB64: b64(pkt), SHA: sha,
		}
		if opts.InjectColorFault && spec.ID == "v1" && seq%500 == 0 && seq != 0 {
			rec.Color = &ColorDecl{Primaries: 9, Transfer: 16, Matrix: 9, FullRange: true}
		}
		if err := writeLine(f, rec); err != nil {
			return err
		}
		writtenSeq++

		// Duplicate the chosen frame once (same seq, same payload).
		if opts.InjectDuplicate && seq == dupAt && spec.ID == "v1" {
			dup := rec
			if err := writeLine(f, dup); err != nil {
				return err
			}
		}
	}
	return nil
}

// ProjectPath returns the manifest path inside a run root.
func ProjectPath(root string) string { return filepath.Join(root, "project.json") }
