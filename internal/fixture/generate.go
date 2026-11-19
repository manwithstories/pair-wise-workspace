// Package fixture generates deterministic test inputs for mediq.
//
// Every record is produced by the same encoder used by the production
// pipeline, so fixtures exercise the real format rather than a lookalike.
package fixture

import (
	"bufio"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"

	"mediq/internal/model"
	"mediq/internal/validate"
)

// Config describes a fixture set to generate.
type Config struct {
	Dir          string
	Frames       int
	FPS          int
	Timebase     model.Timebase
	FrameBytes   int
	Corrupt      int  // 1-based frame ordinal to corrupt; 0 = none
	WrapAt       int  // 1-based ordinal after which timestamps wrap; 0 = none
	Duplicate    int  // 1-based ordinal to duplicate; 0 = none
	TruncateTail int  // records to cut from the end; 0 = none
	OutOfOrder   int  // ordinal moved one position later; 0 = none
	SlowConsumer bool // also emit slow.seq used by the demonstration
	Seed         int64
	// CorruptionByte is the byte index inside the corrupted frame's payload
	// that is flipped. Defaults to 0.
	CorruptionByte int
}

// Result reports what was written.
type Result struct {
	Files          map[string]string
	Frames         int
	CorruptSeq     uint64
	WrapSeq        uint64
	DuplicateSeq   uint64
	TruncatedBytes int64
	SlowFile       string
}

// frameRecord is one planned record. Payload is filled lazily when writing so
// even large fixtures never hold the whole sequence in memory.
type frameRecord struct {
	seq       uint64
	timestamp uint64
	dup       bool
}

// Generate writes the fixture set described by cfg.
func Generate(cfg Config) (*Result, error) {
	cfg = withDefaults(cfg)
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	res := &Result{Files: map[string]string{}}

	normal := filepath.Join(cfg.Dir, "normal.seq")
	if err := writeSequence(normal, cfg, 0); err != nil {
		return nil, err
	}
	res.Files["normal"] = normal

	damaged := filepath.Join(cfg.Dir, "damaged.seq")
	corruptOrdinal := cfg.Corrupt
	if corruptOrdinal <= 0 || corruptOrdinal > cfg.Frames {
		corruptOrdinal = cfg.Frames / 2
	}
	if err := writeSequence(damaged, cfg, corruptOrdinal); err != nil {
		return nil, err
	}
	res.CorruptSeq = uint64(corruptOrdinal)
	res.Files["damaged"] = damaged

	if cfg.WrapAt > 0 && cfg.WrapAt <= cfg.Frames {
		p := filepath.Join(cfg.Dir, "wrap.seq")
		if err := writeSequence(p, cfg.with(func(c *Config) { c.Corrupt, c.WrapAt = 0, cfg.WrapAt }), 0); err != nil {
			return nil, err
		}
		res.WrapSeq = uint64(cfg.WrapAt)
		res.Files["wrap"] = p
	}
	if cfg.Duplicate > 0 && cfg.Duplicate <= cfg.Frames {
		p := filepath.Join(cfg.Dir, "duplicate.seq")
		wcfg := cfg.with(func(c *Config) { c.Corrupt, c.Duplicate = 0, cfg.Duplicate })
		if err := writeSequence(p, wcfg, 0); err != nil {
			return nil, err
		}
		res.DuplicateSeq = uint64(cfg.Duplicate)
		res.Files["duplicate"] = p
	}
	if cfg.TruncateTail > 0 {
		p := filepath.Join(cfg.Dir, "truncated.seq")
		n, err := truncateCopy(damaged, p, cfg.TruncateTail, cfg)
		if err != nil {
			return nil, err
		}
		res.TruncatedBytes = n
		res.Files["truncated"] = p
	}
	if cfg.SlowConsumer {
		p := filepath.Join(cfg.Dir, "slow.seq")
		if err := writeSequence(p, cfg, 0); err != nil {
			return nil, err
		}
		res.SlowFile = p
		res.Files["slow"] = p
	}
	res.Frames = cfg.Frames
	return res, nil
}

func (c Config) with(mut func(*Config)) Config {
	mut(&c)
	return c
}

func withDefaults(c Config) Config {
	if c.FPS <= 0 {
		c.FPS = 30
	}
	if c.Timebase == (model.Timebase{}) {
		c.Timebase = model.Timebase{Num: 90000, Den: 1}
	}
	if c.FrameBytes <= 0 {
		c.FrameBytes = 512
	}
	if c.Seed == 0 {
		c.Seed = 1
	}
	return c
}

// writeSequence emits Frames records to path. The plan is generated up front
// (ordinal -> raw timestamp and emission order) but payloads are produced one
// at a time, so memory stays O(frames) in small integers, not frame bytes.
func writeSequence(path string, cfg Config, corruptOrdinal int) error {
	step := cfg.Timebase.Num / uint32(cfg.FPS)
	if step == 0 {
		step = 1
	}
	plan := planSequence(cfg, step)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 1<<20)
	rng := rand.New(rand.NewSource(cfg.Seed))
	for _, rec := range plan {
		payload := makePayload(rng, rec.seq, cfg.FrameBytes)
		hash := validate.HashFrame(payload)
		if rec.ordinal == corruptOrdinal {
			// Flip a byte after hashing: the recorded hash describes the
			// pristine payload, so the checker must catch the difference.
			payload[len(payload)-1] ^= 0xFF
		}
		if err := writeRecord(bw, rec.seq, rec.timestamp, cfg.Timebase, payload, hash); err != nil {
			return err
		}
	}
	return bw.Flush()
}

type planned struct {
	ordinal   int
	seq       uint64
	timestamp uint64
}

// planSequence lays out records in emission order: a duplicate is inserted
// right after its original, and an out-of-order frame is emitted just before
// the one it should precede.
func planSequence(cfg Config, step uint32) []planned {
	base := make([]planned, 0, cfg.Frames+1)
	var ts uint64
	for i := 1; i <= cfg.Frames; i++ {
		raw := ts
		if cfg.WrapAt > 0 && i == cfg.WrapAt+1 {
			// The capture clock wraps to zero although real time advanced.
			raw = 0
		}
		// Sequence numbers are 1-based so --corrupt N, --duplicate N and
		// --wrap-at N refer to the same frame the terminal reports.
		base = append(base, planned{ordinal: i, seq: uint64(i), timestamp: raw})
		ts += uint64(step)
	}
	out := make([]planned, 0, len(base)+1)
	for _, rec := range base {
		if cfg.OutOfOrder > 0 && rec.ordinal == cfg.OutOfOrder+1 && len(out) > 0 {
			// Swap with the previous record: emit this one first. The swap
			// target is the previous planned record, not the last emitted
			// one (a duplicate may have been appended after it).
			prevIdx := len(out) - 1
			if cfg.Duplicate > 0 && out[prevIdx].ordinal == cfg.Duplicate {
				prevIdx--
			}
			prev := out[prevIdx]
			out[prevIdx] = rec
			out = append(out, prev)
			continue
		}
		out = append(out, rec)
		if cfg.Duplicate > 0 && rec.ordinal == cfg.Duplicate {
			out = append(out, rec)
		}
	}
	return out
}

func makePayload(rng *rand.Rand, seq uint64, n int) []byte {
	buf := make([]byte, n)
	rng.Read(buf)
	// Embed the sequence number so a payload is self-identifying in a hex dump.
	for i := 0; i < 8 && i < n; i++ {
		buf[i] = byte(seq >> (8 * uint(i)))
	}
	return buf
}

func writeRecord(w io.Writer, seq, ts uint64, tb model.Timebase, payload []byte, hash [32]byte) error {
	meta := validate.EncodeMetadata(seq, ts, tb)
	hdr := model.RecordHeader{
		Version:   model.RecordVersion,
		Flags:     model.FlagMetadataPresent,
		Seq:       seq,
		Timestamp: ts,
		Timebase:  tb,
		MetaLen:   uint32(len(meta)),
		FrameLen:  uint32(len(payload)),
		Hash:      hash,
	}
	raw, err := hdr.MarshalBinary()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if _, err := w.Write(meta); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// truncateCopy copies src to dst but removes the last n records, leaving the
// final copied record's body cut in half. It returns how many bytes were cut.
func truncateCopy(src, dst string, n int, cfg Config) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	// Find the record boundaries by scanning, keeping only the offsets.
	var offsets []int64
	var off int64
	rd := bufio.NewReaderSize(in, 1<<20)
	for {
		start := off
		hdr := make([]byte, model.RecordHeaderSize)
		m, err := io.ReadFull(rd, hdr)
		if err == io.EOF && m == 0 {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("scan %s: %w", src, err)
		}
		rh, err := model.ParseRecordHeader(hdr)
		if err != nil {
			return 0, fmt.Errorf("scan %s at %d: %w", src, start, err)
		}
		off += int64(model.RecordHeaderSize) + int64(rh.MetaLen) + int64(rh.FrameLen)
		if _, err := rd.Discard(int(rh.MetaLen) + int(rh.FrameLen)); err != nil {
			return 0, err
		}
		offsets = append(offsets, start)
	}
	if len(offsets) == 0 {
		return 0, fmt.Errorf("source %s has no records", src)
	}
	if n >= len(offsets) {
		n = len(offsets) - 1
	}
	// Cut the final record's body in half and drop the n-1 records after it, so
	// --truncate-tail 1 leaves exactly one incomplete record.
	keepEnd := offsets[len(offsets)-n]
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	if _, err := io.CopyN(out, in, keepEnd); err != nil {
		return 0, err
	}
	// Copy the next record's header plus half of its body.
	hdr := make([]byte, model.RecordHeaderSize)
	if _, err := io.ReadFull(in, hdr); err != nil {
		return 0, err
	}
	if _, err := out.Write(hdr); err != nil {
		return 0, err
	}
	rh, _ := model.ParseRecordHeader(hdr)
	body := int64(rh.MetaLen) + int64(rh.FrameLen)
	half := body / 2
	if half == 0 {
		half = 1
	}
	if _, err := io.CopyN(out, in, half); err != nil {
		return 0, err
	}
	fi, err := in.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	return fi - keepEnd - int64(model.RecordHeaderSize) - half, nil
}
