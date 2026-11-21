// Package remux rewrites a delivery container without decoding or re-encoding
// any media sample. Sample payloads are copied verbatim from the source, so
// input and output byte hashes match by construction.
package remux

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediameld/internal/model"
)

// DeliveryTimescale is the normalized timescale of the delivery container:
// one tick per nanosecond. Storing unified instants directly avoids any
// rational conversion on the output side.
const DeliveryTimescale int64 = 1_000_000_000

// InputSample binds a sample's identity to the physical bytes to copy.
type InputSample struct {
	SourcePath  string
	Offset      int64
	Size        int32
	Hash        [32]byte
	TrackID     string
	TrackKind   model.TrackKind
	Timescale   int64
	DTS, PTS    int64
	Duration    int64
	Description string
	CodedWidth  int
	CodedHeight int
	Rotation    int
}

// Input is one source container's samples grouped by track.
type Input struct {
	Tracks []TrackInput
}

type TrackInput struct {
	ID        string
	Kind      model.TrackKind
	Timescale int64
	Desc      string
	Width     int
	Height    int
	Rotation  int
	Samples   []InputSample
}

// Result reports the written container and the proof of byte preservation.
type Result struct {
	Path         string
	Bytes        int64
	Written      int
	HashMismatch int
	Matched      int
	SampleHashes []SampleHash
	TrackSummary []TrackSummary
}

type SampleHash struct {
	TrackID string
	Index   int
	Hash    [32]byte
	Size    int32
}

type TrackSummary struct {
	ID        string
	Kind      model.TrackKind
	Timescale int64
	Samples   int
}

// Checkpoint records the resumable stage of a remux.
type Checkpoint struct {
	Fingerprint string
	Stage       string // "sources", "mdat", "moov", "done"
	WriterPos   int64
	SamplesDone int
}

// Remux copies the declared samples into a fresh MP4. The output is a valid,
// non-fragmented file: an mdat holding the concatenated sample bytes and a
// moov carrying a track table for each input track.
func Remux(outPath string, in Input, fingerprint string) (Result, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return Result{}, model.WrapError(model.CodeInternal, err, "create output")
	}
	defer f.Close()

	res := Result{Path: outPath}
	// Build mdat payload by streaming each source region exactly once.
	mdatPayloadSize := int64(0)
	for _, t := range in.Tracks {
		for _, s := range t.Samples {
			mdatPayloadSize += int64(s.Size)
		}
	}
	ftyp := buildFtyp()
	// Header sizes: ftyp + mdat header (8) + payload + moov.
	mdatStart := int64(len(ftyp))
	mdatPayloadStart := mdatStart + 8
	mdat := &mp4.BoxInfo{Type: mp4.StrToBoxType("mdat"), Size: uint64(8 + mdatPayloadSize)}

	// Write placeholder mdat header, then stream sample bytes.
	if _, err := f.Write(ftyp); err != nil {
		return res, err
	}
	if _, err := mp4.WriteBoxInfo(f, mdat); err != nil {
		return res, err
	}
	absOffsets, err := copySamples(f, in, mdatPayloadStart)
	if err != nil {
		return res, err
	}
	res.Written = 0
	for _, offs := range absOffsets {
		res.Written += len(offs)
	}

	// Verify every copied sample re-hashes to its declared hash.
	verify, mismatches, err := verifySamples(f, in, absOffsets, mdatPayloadStart)
	if err != nil {
		return res, err
	}
	res.SampleHashes = verify
	res.HashMismatch = mismatches
	res.Matched = len(verify) - mismatches

	// Build and append moov with absolute chunk offsets computed from the
	// mdat layout.
	moov := buildMoov(in, absOffsets, mdatPayloadStart)
	if _, err := f.Write(moov); err != nil {
		return res, err
	}
	st, err := f.Stat()
	if err != nil {
		return res, err
	}
	res.Bytes = st.Size()
	res.TrackSummary = summarize(in)
	return res, nil
}

func summarize(in Input) []TrackSummary {
	out := make([]TrackSummary, len(in.Tracks))
	for i, t := range in.Tracks {
		out[i] = TrackSummary{ID: t.ID, Kind: t.Kind, Timescale: t.Timescale, Samples: len(t.Samples)}
	}
	return out
}

// copySamples streams each declared source region into the mdat, returning the
// absolute file offset of every sample in input order.
func copySamples(dst io.Writer, in Input, payloadStart int64) (map[string][]int64, error) {
	offsets := make(map[string][]int64)
	pos := payloadStart
	open := map[string]*os.File{}
	defer func() {
		for _, f := range open {
			f.Close()
		}
	}()
	buf := make([]byte, 64*1024)
	for _, t := range in.Tracks {
		for _, s := range t.Samples {
			src, ok := open[s.SourcePath]
			if !ok {
				f, err := os.Open(s.SourcePath)
				if err != nil {
					return nil, model.WrapError(model.CodeInternal, err, "open source for copy").WithTrack(t.ID)
				}
				open[s.SourcePath] = f
				src = f
			}
			if _, err := src.Seek(s.Offset, io.SeekStart); err != nil {
				return nil, err
			}
			n, err := io.CopyBuffer(struct{ io.Writer }{dst}, io.LimitReader(src, int64(s.Size)), buf)
			if err != nil {
				return nil, model.WrapError(model.CodeInternal, err, "copy sample bytes").WithTrack(t.ID)
			}
			if n != int64(s.Size) {
				return nil, model.NewError(model.CodeFingerprint,
					"source sample shrank to %d of %d bytes", n, s.Size).WithTrack(t.ID)
			}
			offsets[t.ID] = append(offsets[t.ID], pos)
			pos += int64(s.Size)
		}
	}
	return offsets, nil
}

// verifySamples re-reads each copied region from the output file and compares
// its hash to the declared source hash.
func verifySamples(f *os.File, in Input, offsets map[string][]int64, payloadStart int64) ([]SampleHash, int, error) {
	var out []SampleHash
	mismatches := 0
	for _, t := range in.Tracks {
		for i, s := range t.Samples {
			off := offsets[t.ID][i]
			if _, err := f.Seek(off, io.SeekStart); err != nil {
				return nil, 0, err
			}
			h := sha256.New()
			if _, err := io.CopyN(h, f, int64(s.Size)); err != nil {
				return nil, 0, model.WrapError(model.CodeInternal, err, "verify copy").WithTrack(t.ID)
			}
			var sum [32]byte
			copy(sum[:], h.Sum(nil))
			if sum != s.Hash {
				mismatches++
				return nil, 0, model.NewError(model.CodeFingerprint,
					"sample %d of track %s changed during remux", i, t.ID).WithTrack(t.ID)
			}
			out = append(out, SampleHash{TrackID: t.ID, Index: i, Hash: sum, Size: s.Size})
		}
	}
	return out, mismatches, nil
}

// Fingerprint derives a stable identity of the remux inputs, used as the
// resume checkpoint key.
func Fingerprint(in Input) string {
	h := sha256.New()
	for _, t := range in.Tracks {
		fmt.Fprintf(h, "%s:%s:%d:%d;", t.ID, t.Kind, t.Timescale, len(t.Samples))
		for _, s := range t.Samples {
			h.Write(s.Hash[:])
			fmt.Fprintf(h, ":%d:%d:%d;", s.PTS, s.Duration, s.Size)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Resume validates a prior checkpoint against the current input fingerprint.
// A mismatch is refused rather than silently reusing intermediate output.
func Resume(cp Checkpoint, currentFingerprint string) error {
	if cp.Fingerprint != currentFingerprint {
		return model.NewError(model.CodeFingerprint,
			"checkpoint fingerprint %s does not match input %s", cp.Fingerprint, currentFingerprint)
	}
	switch cp.Stage {
	case "sources", "mdat", "moov", "done":
		return nil
	default:
		return model.NewError(model.CodeCheckpoint, "unknown checkpoint stage %q", cp.Stage)
	}
}

// SaveCheckpoint persists a checkpoint atomically next to the output.
func SaveCheckpoint(path string, cp Checkpoint) error {
	data := []byte(fmt.Sprintf("%s\n%s\n%d\n%d\n", cp.Fingerprint, cp.Stage, cp.WriterPos, cp.SamplesDone))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return model.WrapError(model.CodeCheckpoint, err, "write checkpoint")
	}
	return os.Rename(tmp, path)
}

// LoadCheckpoint reads a checkpoint sidecar.
func LoadCheckpoint(path string) (Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Checkpoint{}, model.WrapError(model.CodeCheckpoint, err, "read checkpoint")
	}
	var cp Checkpoint
	lines := splitLines(string(data))
	if len(lines) < 2 {
		return Checkpoint{}, model.NewError(model.CodeCheckpoint, "malformed checkpoint")
	}
	cp.Fingerprint = lines[0]
	cp.Stage = lines[1]
	if len(lines) > 2 {
		fmt.Sscan(lines[2], &cp.WriterPos)
	}
	if len(lines) > 3 {
		fmt.Sscan(lines[3], &cp.SamplesDone)
	}
	return cp, nil
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// ErrInterrupted signals a deliberately simulated crash at the requested
// sample boundary. It is not a failure of the remux itself.
var ErrInterrupted = model.NewError(model.CodeCheckpoint, "remux interrupted for resume test")

// RemuxResumable copies samples into outPath with a stage checkpoint. If
// interruptAfter > 0 the run stops after writing that many samples and
// persists a checkpoint. A later call with the same fingerprint resumes at
// that exact sample; a different fingerprint is refused.
func RemuxResumable(outPath, checkpointPath string, in Input, fp string, interruptAfter int) (Result, error) {
	// Total sample count and per-track offsets are deterministic from input.
	type pos struct {
		track, index int
	}
	var order []pos
	offsets := map[string][]int64{}
	payloadStart := int64(len(buildFtyp())) + 8
	cursor := payloadStart
	for ti, t := range in.Tracks {
		for si, s := range t.Samples {
			order = append(order, pos{ti, si})
			offsets[t.ID] = append(offsets[t.ID], cursor)
			cursor += int64(s.Size)
		}
	}
	total := len(order)

	done := 0
	var f *os.File
	var err error
	if cp, cpErr := LoadCheckpoint(checkpointPath); cpErr == nil {
		if err := Resume(cp, fp); err != nil {
			return Result{}, err
		}
		switch cp.Stage {
		case "done":
			return verifyExisting(outPath, in, offsets)
		case "mdat":
			done = cp.SamplesDone
			f, err = os.OpenFile(outPath, os.O_RDWR, 0o644)
			if err != nil {
				return Result{}, model.WrapError(model.CodeCheckpoint, err, "open partial output")
			}
		default:
			return Result{}, model.NewError(model.CodeCheckpoint, "cannot resume stage %q", cp.Stage)
		}
	} else {
		f, err = os.Create(outPath)
		if err != nil {
			return Result{}, model.WrapError(model.CodeInternal, err, "create output")
		}
		ftyp := buildFtyp()
		if _, err := f.Write(ftyp); err != nil {
			return Result{}, err
		}
		mdatSize := cursor - payloadStart
		bi := &mp4.BoxInfo{Type: mp4.StrToBoxType("mdat"), Size: uint64(8 + mdatSize)}
		if _, err := mp4.WriteBoxInfo(f, bi); err != nil {
			return Result{}, err
		}
		if err := SaveCheckpoint(checkpointPath, Checkpoint{Fingerprint: fp, Stage: "mdat", SamplesDone: 0, WriterPos: payloadStart}); err != nil {
			return Result{}, err
		}
	}
	defer f.Close()

	// Continue copying from the recorded sample boundary.
	if _, err := f.Seek(offsets[in.Tracks[order[done].track].ID][order[done].index], io.SeekStart); err != nil && done < total {
		return Result{}, err
	}
	open := map[string]*os.File{}
	defer func() {
		for _, src := range open {
			src.Close()
		}
	}()
	buf := make([]byte, 64*1024)
	written := done
	for written < total {
		p := order[written]
		t := in.Tracks[p.track]
		s := t.Samples[p.index]
		src, ok := open[s.SourcePath]
		if !ok {
			src, err = os.Open(s.SourcePath)
			if err != nil {
				return Result{}, model.WrapError(model.CodeInternal, err, "open source").WithTrack(t.ID)
			}
			open[s.SourcePath] = src
		}
		if _, err := src.Seek(s.Offset, io.SeekStart); err != nil {
			return Result{}, err
		}
		n, err := io.CopyBuffer(f, io.LimitReader(src, int64(s.Size)), buf)
		if err != nil {
			return Result{}, model.WrapError(model.CodeInternal, err, "copy sample bytes").WithTrack(t.ID)
		}
		if n != int64(s.Size) {
			return Result{}, model.NewError(model.CodeFingerprint,
				"source sample shrank to %d of %d bytes", n, s.Size).WithTrack(t.ID)
		}
		written++
		if written%16 == 0 || written == total {
			if err := SaveCheckpoint(checkpointPath, Checkpoint{Fingerprint: fp, Stage: "mdat", SamplesDone: written, WriterPos: offsets[t.ID][p.index] + int64(s.Size)}); err != nil {
				return Result{}, err
			}
		}
		if interruptAfter > 0 && written == interruptAfter && written < total {
			// Persist the exact boundary before stopping so resume is exact.
			if err := SaveCheckpoint(checkpointPath, Checkpoint{Fingerprint: fp, Stage: "mdat", SamplesDone: written, WriterPos: offsets[t.ID][p.index] + int64(s.Size)}); err != nil {
				return Result{}, err
			}
			return Result{Written: written, Path: outPath}, ErrInterrupted
		}
	}

	// Append moov and finish.
	moov := buildMoov(in, offsets, payloadStart)
	if _, err := f.Write(moov); err != nil {
		return Result{}, err
	}
	if err := SaveCheckpoint(checkpointPath, Checkpoint{Fingerprint: fp, Stage: "done", SamplesDone: total, WriterPos: 0}); err != nil {
		return Result{}, err
	}
	return verifyExisting(outPath, in, offsets)
}

// verifyExisting re-hashes an output file against the declared sample hashes.
func verifyExisting(outPath string, in Input, offsets map[string][]int64) (Result, error) {
	f, err := os.Open(outPath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	res, mismatches, err := verifySamples(f, in, offsets, 0)
	if err != nil {
		return Result{}, err
	}
	st, err := f.Stat()
	if err != nil {
		return Result{}, err
	}
	written := 0
	for _, o := range offsets {
		written += len(o)
	}
	return Result{
		Path: outPath, Bytes: st.Size(), Written: written,
		HashMismatch: mismatches, Matched: len(res) - mismatches,
		SampleHashes: res, TrackSummary: summarize(in),
	}, nil
}
