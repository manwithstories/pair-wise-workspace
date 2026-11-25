package index

import (
	"fmt"
	"os"

	"mediaaudit/internal/model"
)

// ValidateConfig rejects a resume when the running configuration differs from
// the one recorded in the index, preventing a wrong-continuation bug.
func (f *File) ValidateConfig(cfg Config) error {
	got := f.Payload.Config
	if got.PackagePath != cfg.PackagePath {
		return indexFault(fmt.Sprintf("package path changed: %q vs %q", got.PackagePath, cfg.PackagePath))
	}
	if got.ChunkFrames != cfg.ChunkFrames {
		return indexFault("chunk-frames changed; cannot resume")
	}
	if got.CheckpointEvery != cfg.CheckpointEvery {
		return indexFault("checkpoint-every changed; cannot resume")
	}
	if got.TileFrames != cfg.TileFrames {
		return indexFault("tile-frames changed; cannot resume")
	}
	if got.InputFingerprint != cfg.InputFingerprint {
		return indexFault("input package fingerprint changed; cannot resume")
	}
	return nil
}

// NextSeq returns the seq of the next chunk to process (0-based).
func (f *File) NextSeq() int64 { return f.Payload.CommittedSeq + 1 }

// AppendChunk validates the record's ordering and persists it atomically. The
// cursor only ever advances over a fully verified chunk.
func (f *File) AppendChunk(rec ChunkRecord) error {
	if rec.Seq != f.NextSeq() {
		return indexFault(fmt.Sprintf("chunk seq %d out of order (next %d)", rec.Seq, f.NextSeq()))
	}
	if rec.LastFrame <= rec.FirstFrame {
		return indexFault("refusing empty chunk")
	}
	if rec.Status != "ok" && rec.Status != "damaged" {
		return indexFault("invalid chunk status: " + rec.Status)
	}
	p := f.clonePayload()
	p.Chunks = append(p.Chunks, rec)
	p.CommittedSeq = rec.Seq
	p.CheckpointNo++
	if err := writeAtomic(f.Path, p); err != nil {
		return err
	}
	return f.reload(p)
}

// MarkFinalized flips the finalized flag after all chunks are committed.
func (f *File) MarkFinalized() error {
	p := f.clonePayload()
	p.Finalized = true
	if err := writeAtomic(f.Path, p); err != nil {
		return err
	}
	return f.reload(p)
}

// Rollback discards chunks after checkpoint seq `keep`, restoring the last
// complete checkpoint. A torn tail is removed so resume restarts there.
func (f *File) Rollback(keep int64) error {
	p := f.clonePayload()
	if keep < -1 {
		keep = -1
	}
	trimmed := make([]ChunkRecord, 0)
	for _, c := range p.Chunks {
		if c.Seq <= keep {
			trimmed = append(trimmed, c)
		}
	}
	p.Chunks = trimmed
	if len(trimmed) > 0 {
		p.CommittedSeq = trimmed[len(trimmed)-1].Seq
	} else {
		p.CommittedSeq = -1
		p.Chunks = []ChunkRecord{}
	}
	p.Finalized = false
	if err := writeAtomic(f.Path, p); err != nil {
		return err
	}
	return f.reload(p)
}

// IsFinalized reports whether the full package has been processed.
func (f *File) IsFinalized() bool { return f.Payload.Finalized }

// DamageCount sums damaged chunks (each located fault range is counted once).
func (f *File) DamageCount() int {
	n := 0
	for _, c := range f.Payload.Chunks {
		n += c.DamageCount
	}
	return n
}

func (f *File) clonePayload() Payload {
	p := f.Payload
	chunks := make([]ChunkRecord, len(f.Payload.Chunks))
	copy(chunks, f.Payload.Chunks)
	p.Chunks = chunks
	return p
}

func (f *File) reload(p Payload) error {
	loaded, err := Load(f.Path)
	if err != nil {
		return err
	}
	f.Payload = loaded.Payload
	f.sha = loaded.sha
	return nil
}

// Tear truncates the index mid-file to simulate a crash during a write. It is
// used only by the fixture's checkpoint-tear scenario.
func Tear(path string, keepBytes int) error {
	b, err := readBytes(path)
	if err != nil {
		return err
	}
	if keepBytes > len(b) {
		keepBytes = len(b)
	}
	if keepBytes < headerSize {
		return model.NewFault(model.KindIndex, "", model.FrameRange{}, -1,
			"tear would remove the whole header")
	}
	return os.WriteFile(path, b[:keepBytes], 0o644)
}

func readBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}
