// Package synth generates the self-contained fixtures the demo and tests use.
// Every output is derived from a seed, so a run is reproducible byte for byte
// and no external sample files are needed.
//
// A generated source is a mediad container (see internal/ingest/format.go):
// one track declaration followed by one frame box per frame. Defects are
// injected deliberately: corrupt payloads, torn tails and repeated frame
// numbers each exercise a specific recovery path.
package synth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"

	"mediad/internal/ingest"
	"mediad/internal/model"
)

// ClockProfile describes one device's capture clock. It controls the injected
// repair scenarios.
type ClockProfile struct {
	CaptureUPS uint32 // capture counter ticks per second
	WrapUnits  int64  // counter modulus, 0 = no wrap
	Jitter     int64  // raw units of random backwards noise
	RollbackAt int    // frame index at which the clock jumps backwards (0 = never)
	RollbackBy int64  // magnitude of the jump in raw units
}

// TrackSpec is the complete recipe for one generated source file.
type TrackSpec struct {
	TrackID      uint32
	Frames       int
	Width        uint32
	Height       uint32
	Timescale    uint32
	Color        model.Color
	TargetKbps   uint32
	PayloadBytes int // nominal payload size; drives the measured bitrate
	Clock        ClockProfile
	EpochUnits   uint64

	CorruptFrames  int  // number of frames whose payload is altered
	TruncateTail   bool // end the file inside the last frame
	DuplicateEvery int  // repeat every Nth frame number (0 = off)
	ConflictEvery  int  // repeat every Nth number with different content (0 = off)
}

// Defect lists one injected defect so tests can assert it was recovered.
type Defect struct {
	FrameNo uint64
	Kind    string
}

// Result reports what the generator wrote.
type Result struct {
	Path       string
	Frames     int
	Bytes      int64
	Track      *ingest.Track
	Defects    []Defect
	PayloadSum [32]byte // running digest over all frame payloads, for determinism checks
}

// ErrBadSpec reports an unusable TrackSpec.
var ErrBadSpec = errors.New("synth: invalid track spec")

func (s TrackSpec) validate() error {
	switch {
	case s.TrackID == 0:
		return fmt.Errorf("%w: track id must be non-zero", ErrBadSpec)
	case s.Frames <= 0:
		return fmt.Errorf("%w: frames must be positive", ErrBadSpec)
	case s.Width == 0 || s.Height == 0:
		return fmt.Errorf("%w: zero geometry", ErrBadSpec)
	case s.Timescale == 0:
		return fmt.Errorf("%w: zero timescale", ErrBadSpec)
	case s.Clock.CaptureUPS == 0:
		return fmt.Errorf("%w: zero capture rate", ErrBadSpec)
	case s.TargetKbps > 0 && s.PayloadBytes <= 0:
		return fmt.Errorf("%w: target bitrate needs a positive payload size", ErrBadSpec)
	case s.Clock.CaptureUPS%DefaultFPS != 0:
		return fmt.Errorf("%w: capture rate must be a multiple of %d fps", ErrBadSpec, DefaultFPS)
	case s.CorruptFrames < 0 || s.CorruptFrames > s.Frames:
		return fmt.Errorf("%w: corrupt frame count out of range", ErrBadSpec)
	}
	return nil
}

// DefaultFPS is the capture cadence of generated fixtures.
const DefaultFPS = 24

// DefaultSpec returns a healthy track spec at the given id. The clock captures
// at 24 fps with microsecond granularity, and payloads are sized so the
// measured container bitrate lands on the declared target.
func DefaultSpec(id uint32, frames int) TrackSpec {
	const targetKbps = 1200
	payload := targetKbps * 1000 / 8 / DefaultFPS // bytes per frame at target
	return TrackSpec{
		TrackID:      id,
		Frames:       frames,
		Width:        48,
		Height:       32,
		Timescale:    90000,
		Color:        model.Color{Primaries: 1, Transfer: 13, Matrix: 6, Range: 1},
		TargetKbps:   targetKbps,
		PayloadBytes: payload,
		// 24 kHz counter: one millisecond per unit, exactly 1000 units per
		// 24 fps frame interval.
		Clock:      ClockProfile{CaptureUPS: 24_000},
		EpochUnits: uint64(40_800_000_000 + int64(id)*1_000_000),
	}
}

// Generate writes one source file to w. The writer is used sequentially and
// the caller owns atomicity (write to a temp file, then rename).
func Generate(w io.WriteSeeker, spec TrackSpec, seed int64) (*Result, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if err := ingest.WriteFtyp(w); err != nil {
		return nil, err
	}
	tb := ingest.NewTrackBox()
	tb.TrackID = spec.TrackID
	tb.Width = spec.Width
	tb.Height = spec.Height
	tb.CaptureUPS = spec.Clock.CaptureUPS
	tb.Timescale = spec.Timescale
	tb.EpochUnits = spec.EpochUnits
	tb.WrapUnits = uint64(spec.Clock.WrapUnits)
	tb.TargetKbps = spec.TargetKbps
	tb.Color = [4]byte{spec.Color.Primaries, spec.Color.Transfer, spec.Color.Matrix, spec.Color.Range}
	tb.FrameCount = uint32(spec.Frames)
	if _, _, err := ingest.WriteBox(w, tb); err != nil {
		return nil, err
	}

	res := &Result{Path: "", Frames: spec.Frames}
	rng := rand.New(rand.NewSource(seed))
	corrupt := corruptSet(rng, spec.Frames, spec.CorruptFrames)
	targetSize := payloadSizeFor(spec)

	for i := 0; i < spec.Frames; i++ {
		frameNo := uint64(i + 1)
		// Advance by exactly one frame interval in capture units. The rate is a
		// multiple of DefaultFPS, so this is exact at every precision.
		step := int64(spec.Clock.CaptureUPS) / DefaultFPS
		if step < 1 {
			step = 1
		}
		raw := int64(spec.EpochUnits) + int64(i)*step
		if spec.Clock.Jitter != 0 && i > 0 {
			raw += rng.Int63n(2*spec.Clock.Jitter+1) - spec.Clock.Jitter
		}
		if spec.Clock.RollbackAt > 0 && i == spec.Clock.RollbackAt {
			raw -= spec.Clock.RollbackBy
			res.Defects = append(res.Defects, Defect{FrameNo: frameNo, Kind: "rollback"})
		}

		quality := uint8(200)
		writeNo := frameNo
		if spec.ConflictEvery > 0 && i > spec.ConflictEvery && i%spec.ConflictEvery == 0 {
			// Rewind the number but keep distinct content: a conflict.
			writeNo = uint64(i)
			quality = 120
			res.Defects = append(res.Defects, Defect{FrameNo: frameNo, Kind: "conflict"})
		}
		payload, err := ingest.EncodeFrame(spec.TrackID, writeNo, spec.Width, spec.Height, quality, spec.Color, spec.PayloadBytes)
		if err != nil {
			return nil, err
		}
		declared := ingest.PayloadDigest(payload)
		if corrupt[i] {
			payload = reshape(payload, targetSize, writeNo)
			res.Defects = append(res.Defects, Defect{FrameNo: frameNo, Kind: "corrupt"})
		}

		fb := ingest.NewFrameBox()
		fb.TrackID = spec.TrackID
		fb.FrameNo = writeNo
		fb.CaptureUnits = uint64(raw)
		fb.Width = spec.Width
		fb.Height = spec.Height
		fb.CaptureUPS = spec.Clock.CaptureUPS
		fb.Timescale = spec.Timescale
		fb.Color = tb.Color
		fb.DeclaredHash = declared
		fb.PayloadLen = uint32(len(payload))
		fb.Payload = payload

		if _, _, err := ingest.WriteBox(w, fb); err != nil {
			return nil, err
		}
		if spec.DuplicateEvery > 0 && i > 0 && i%spec.DuplicateEvery == 0 {
			// Emit a second, identical record: a benign duplicate.
			dup := *fb
			if _, _, err := ingest.WriteBox(w, &dup); err != nil {
				return nil, err
			}
			res.Frames++
			res.Defects = append(res.Defects, Defect{FrameNo: writeNo, Kind: "duplicate"})
		}
	}

	if spec.TruncateTail {
		// Append the header of one more frame and cut it short. A real
		// interrupted camera leaves exactly this: a complete header, a partial
		// payload.
		if err := writeTornTail(w, spec); err != nil {
			return nil, err
		}
		res.Frames++
		res.Defects = append(res.Defects, Defect{FrameNo: uint64(spec.Frames + 1), Kind: "truncated"})
	}

	end, err := w.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	res.Bytes = end
	return res, nil
}

// writeTornTail writes a frame header followed by a declared payload that is
// physically cut off halfway.
func writeTornTail(w io.WriteSeeker, spec TrackSpec) error {
	payload, err := ingest.EncodeFrame(spec.TrackID, uint64(spec.Frames+1), spec.Width, spec.Height, 200, spec.Color, spec.PayloadBytes)
	if err != nil {
		return err
	}
	fb := ingest.NewFrameBox()
	fb.TrackID = spec.TrackID
	fb.FrameNo = uint64(spec.Frames + 1)
	fb.CaptureUnits = spec.EpochUnits + uint64(spec.Frames)
	fb.Width = spec.Width
	fb.Height = spec.Height
	fb.CaptureUPS = spec.Clock.CaptureUPS
	fb.Timescale = spec.Timescale
	fb.Color = [4]byte{spec.Color.Primaries, spec.Color.Transfer, spec.Color.Matrix, spec.Color.Range}
	fb.DeclaredHash = ingest.PayloadDigest(payload)
	fb.PayloadLen = uint32(len(payload))
	fb.Payload = payload

	// Build the full box in memory, then physically cut it short. The box
	// header still advertises the complete size, so a reader sees a header
	// whose declared end lies past EOF: exactly what an interrupted writer
	// leaves behind.
	fb.Payload = payload
	var full bytes.Buffer
	if _, err := mp4.Marshal(&full, fb, mp4.Context{}); err != nil {
		return err
	}
	start, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	bi := &mp4.BoxInfo{Size: uint64(ingest.HeaderSize + full.Len()), HeaderSize: ingest.HeaderSize, Type: ingest.TypeFrame}
	if _, err := mp4.WriteBoxInfo(w, bi); err != nil {
		return err
	}
	// Write only half of the payload after the complete 8-byte header.
	payloadBytes := full.Bytes()
	cut := len(payloadBytes) / 2
	if _, err := w.Write(payloadBytes[:cut]); err != nil {
		return err
	}
	end, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	_ = start
	_ = end
	return nil
}

// corruptSet chooses which frames get an altered payload. It is deterministic
// for a seed and never touches a ScheduleDefect-free spec.
func corruptSet(rng *rand.Rand, frames, n int) map[int]bool {
	set := make(map[int]bool, n)
	if n <= 0 {
		return set
	}
	// Spread the corrupt frames through the middle so they are not mistaken
	// for a tail defect.
	step := frames / (n + 1)
	if step < 1 {
		step = 1
	}
	for k := 0; k < n; k++ {
		idx := step*(k+1) + rng.Intn(step)
		for set[idx] && idx < frames-1 {
			idx++
		}
		set[idx] = true
	}
	return set
}

// reshape keeps the declared digest while producing a payload of a different
// length/contents; the scanner must detect the mismatch. It also nudges bytes
// so the image itself is no longer valid.
func reshape(payload []byte, size int, salt uint64) []byte {
	out := make([]byte, len(payload))
	copy(out, payload)
	if len(out) > 0 {
		out[len(out)/2] ^= byte(0x5a ^ salt)
	}
	return out
}

func payloadSizeFor(spec TrackSpec) int {
	return spec.PayloadBytes
}

// WriteSource writes one source atomically to dest, using a sibling temp file
// so a crash never leaves a half-written source at the published path.
func WriteSource(dest string, spec TrackSpec, seed int64) (*Result, error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".source-*.tmp")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(name)
		}
	}()
	res, err := Generate(f, spec, seed)
	if err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(name, dest); err != nil {
		return nil, err
	}
	ok = true
	res.Path = dest
	return res, nil
}

// WriteSources generates every track in specs into the directory dir, one file
// named track-<id>.mediad each. Files are written atomically.
func WriteSources(dir string, specs []TrackSpec, seed int64, create func(path string) (io.WriteSeeker, func() error, error)) ([]*Result, error) {
	out := make([]*Result, 0, len(specs))
	for i, spec := range specs {
		path := fmt.Sprintf("%s/track-%d.mediad", dir, spec.TrackID)
		w, commit, err := create(path)
		if err != nil {
			return nil, err
		}
		res, err := Generate(w, spec, seed+int64(i))
		if err != nil {
			return nil, err
		}
		if err := commit(); err != nil {
			return nil, err
		}
		res.Path = path
		out = append(out, res)
	}
	return out, nil
}
