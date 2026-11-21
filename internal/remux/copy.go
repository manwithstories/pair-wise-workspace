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
