// Package pairflow generates the deterministic local sample data the
// acceptance run consumes: one frame-record file plus SHA-256 metadata sidecar
// per camera, an optional defect file set, and a small JSON description the
// CLI prints.
//
// Everything is derived from a seed. No network and no external media files
// are involved, and the same seed always produces byte-identical output.
package pairflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/mediamend/mediamend/internal/clock"
	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/frame"
	"github.com/mediamend/mediamend/internal/segment"
)

// Camera describes one fixed capture position.
type Camera struct {
	ID        string `json:"id"`
	Rate      uint64 `json:"rate"`
	FrameStep uint64 `json:"frame_step"`
	Frames    uint32 `json:"frames"`
	// WrapAt is the sequence number after which the 32-bit device clock
	// rolls over (0 disables the rollover sample).
	WrapAt uint32 `json:"wrap_at,omitempty"`
	// BatchResetAt restarts sequence numbering at zero (0 disables it).
	BatchResetAt uint32 `json:"batch_reset_at,omitempty"`
	// DuplicateAt repeats one sequence (0 disables it).
	DuplicateAt uint32 `json:"duplicate_at,omitempty"`
	// MissingKeyAt drops the keyframe flag at this sequence (0 disables it).
	MissingKeyAt uint32 `json:"missing_key_at,omitempty"`
}

// Options controls generation.
type Options struct {
	Root string
	// Frames is the default per-camera frame count.
	Frames uint32
	// CorruptEvery marks every Nth frame as corrupt (0 disables corruption).
	CorruptEvery uint32
	// KeyEvery is the keyframe cadence.
	KeyEvery uint32
	// Ladder is the delivery ladder written into the descriptor.
	Ladder []segment.Representation
	// CameraCount selects the standard six-camera event layout when zero.
	CameraCount int
}

// Manifest describes the generated dataset.
type Manifest struct {
	Version  int                      `json:"version"`
	Seed     int64                    `json:"seed"`
	Cameras  []Camera                 `json:"cameras"`
	KeyEvery uint32                   `json:"key_every"`
	Ladder   []segment.Representation `json:"ladder"`
	// CorruptSeqs lists, per camera, every sequence whose metadata digest was
	// deliberately corrupted. The acceptance run compares this against the
	// reported faults to prove location precision.
	CorruptSeqs map[string][]uint64 `json:"corrupt_seqs"`
	Files       map[string]string   `json:"files"`
}

// Six cameras with deliberately different rates, precision and batch
// boundaries, matching a real fixed-camera capture rig.
func defaultCameras(frames uint32) []Camera {
	step := func(fps uint64) uint64 { return clock.MasterRate / fps }
	return []Camera{
		{ID: "camA", Rate: clock.MasterRate, FrameStep: step(30), Frames: frames,
			WrapAt: frames / 2},
		{ID: "camB", Rate: clock.MasterRate, FrameStep: step(25), Frames: frames,
			BatchResetAt: frames * 2 / 3},
		{ID: "camC", Rate: 48000, FrameStep: 1600, Frames: frames,
			DuplicateAt: 55},
		{ID: "camD", Rate: 1000, FrameStep: 33, Frames: frames,
			MissingKeyAt: 90},
		{ID: "camE", Rate: 90000, FrameStep: step(60), Frames: frames},
		{ID: "camF", Rate: 90000, FrameStep: step(24), Frames: frames,
			WrapAt: frames / 3, DuplicateAt: 17},
	}
}

// CorruptSeq returns the sequence numbers that receive a bad digest for one
// camera under the given stride.
func CorruptSeq(frames, every uint32) []uint64 {
	if every == 0 {
		return nil
	}
	var out []uint64
	for seq := uint64(every); seq < uint64(frames); seq += uint64(every) {
		out = append(out, seq)
	}
	return out
}

// Generate writes the dataset and returns its manifest plus any faults
// encountered while writing. A non-nil fatal fault list means generation
// failed and the directory must not be used.
func Generate(opts Options) (*Manifest, fault.List) {
	if opts.Root == "" {
		return nil, fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			Message: "fixture: output root is required", Fatal: true}}
	}
	if opts.Frames == 0 {
		opts.Frames = 1200
	}
	if opts.KeyEvery == 0 {
		opts.KeyEvery = 30
	}
	if len(opts.Ladder) == 0 {
		opts.Ladder = segment.DefaultLadder
	}
	cams := defaultCameras(opts.Frames)
	if opts.CameraCount > 0 && opts.CameraCount <= len(cams) {
		cams = cams[:opts.CameraCount]
	}
	if err := os.MkdirAll(filepath.Join(opts.Root, "input"), 0o755); err != nil {
		return nil, fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			Message: err.Error(), Fatal: true}}
	}

	m := &Manifest{
		Version: 1, Seed: 1, Cameras: cams, KeyEvery: opts.KeyEvery,
		Ladder: opts.Ladder, CorruptSeqs: map[string][]uint64{}, Files: map[string]string{},
	}
	var faults fault.List
	for _, cam := range cams {
		corrupt := CorruptSeq(cam.Frames, opts.CorruptEvery)
		if len(corrupt) > 0 {
			m.CorruptSeqs[cam.ID] = corrupt
		}
		file, meta, fs := writeCamera(opts, cam, corrupt)
		faults = append(faults, fs...)
		if fs.Fatal() {
			return m, faults
		}
		m.Files[cam.ID] = file
		m.Files[cam.ID+".meta"] = meta
	}

	// Defect samples for the boundary self-check.
	samples, fs := writeBoundarySamples(opts.Root)
	faults = append(faults, fs...)
	if fs.Fatal() {
		return m, faults
	}
	m.Files["boundaries"] = samples

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, append(faults, fault.Fault{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			Message: err.Error(), Fatal: true})
	}
	if err := atomicWrite(filepath.Join(opts.Root, "fixture.json"), data); err != nil {
		return m, append(faults, fault.Fault{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			Message: err.Error(), Fatal: true})
	}
	return m, faults
}

// writeCamera emits one camera's frame file and metadata sidecar.
func writeCamera(opts Options, cam Camera, corrupt []uint64) (string, string, fault.List) {
	path := filepath.Join(opts.Root, "input", cam.ID+".frm")
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cam-*")
	if err != nil {
		return "", "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			File: path, Message: err.Error(), Fatal: true}}
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	bad := map[uint64]bool{}
	for _, s := range corrupt {
		bad[s] = true
	}
	keyEvery := uint64(opts.KeyEvery)
	// A camera that wraps starts its device clock just before the 32-bit
	// modulus so the capture genuinely rolls over mid-stream.
	wrapBase := uint64(0)
	if cam.WrapAt > 0 {
		wrapBase = clock.WrapMod - uint64(cam.WrapAt)*cam.FrameStep
	}
	var meta bytes.Buffer
	buf := make([]byte, 0, 256)
	offset := int64(0)
	for seq := uint64(0); seq < uint64(cam.Frames); seq++ {
		deviceTS := seq * cam.FrameStep
		if cam.WrapAt > 0 {
			deviceTS = (wrapBase + seq*cam.FrameStep) % clock.WrapMod
		}
		wireSeq := seq
		if cam.BatchResetAt > 0 && seq >= uint64(cam.BatchResetAt) {
			wireSeq = seq - uint64(cam.BatchResetAt)
		}
		flags := frame.Flags(0)
		if seq%keyEvery == 0 && !(cam.MissingKeyAt > 0 && seq == uint64(cam.MissingKeyAt)) {
			flags |= frame.FlagKeyframe
		}
		// Payload content is deterministic per camera and sequence.
		n := 24 + int(seq%43)
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte((seq*31 + uint64(i)*17 + uint64(len(cam.ID))) & 0xff)
		}
		buf = buf[:0]
		buf = frame.Encode(buf, frame.Header{
			Camera: cam.ID, Seq: wireSeq, DeviceTS: deviceTS,
			Rate: uint32(cam.Rate), Flags: flags,
		}, payload)
		writeOne := func(at int64) {
			if _, err := tmp.Write(buf); err != nil {
				return
			}
			offset = at + int64(len(buf))
			sum := sha256.Sum256(payload)
			digest := hex.EncodeToString(sum[:])
			// Corruption is declared against the source sequence, which is
			// what the manifest reports; the wire sequence may differ after
			// a batch reset but the frame is still the same one.
			if bad[seq] {
				digest = hex.EncodeToString(make([]byte, 32))
			}
			fmt.Fprintf(&meta,
				"{\"camera_id\":%q,\"seq\":%d,\"device_ts\":%d,\"body_length\":%d,\"offset\":%d,\"sha256\":%q,\"keyframe\":%t}\n",
				cam.ID, wireSeq, deviceTS, len(payload), at, digest, flags.Has(frame.FlagKeyframe))
		}
		writeOne(offset)
		if cam.DuplicateAt > 0 && seq == uint64(cam.DuplicateAt) {
			// A retransmitted copy: identical bytes, identical metadata.
			writeOne(offset)
		}
	}
	if err := tmp.Sync(); err != nil {
		return "", "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			File: path, Message: err.Error(), Fatal: true}}
	}
	if err := tmp.Close(); err != nil {
		return "", "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			File: path, Message: err.Error(), Fatal: true}}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			File: path, Message: err.Error(), Fatal: true}}
	}

	metaPath := filepath.Join(opts.Root, "input", cam.ID+".meta")
	if err := atomicWrite(metaPath, meta.Bytes()); err != nil {
		return "", "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			File: metaPath, Message: err.Error(), Fatal: true}}
	}
	return path, metaPath, nil
}

// BoundarySamples describes the three defect fixtures the self-check reads.
type BoundarySamples struct {
	Truncated        string `json:"truncated"`
	ChecksumMismatch string `json:"checksum_mismatch"`
	TimestampWrap    string `json:"timestamp_wrap"`
	Duplicate        string `json:"duplicate"`
}

// writeBoundarySamples emits the small defect files used by
// -self-check-boundaries. Each file is self-describing so the checker only
// needs to parse records.
func writeBoundarySamples(root string) (string, fault.List) {
	dir := filepath.Join(root, "boundaries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO,
			Message: err.Error(), Fatal: true}}
	}
	base := func(seq uint64) (frame.Header, []byte) {
		h := frame.Header{Camera: "check", Seq: seq, DeviceTS: seq * 3000,
			Rate: uint32(clock.MasterRate)}
		p := []byte{byte(seq), byte(seq >> 8), 0xAA, 0x55}
		return h, p
	}

	// Truncated: two good records then half of a third.
	var trunc []byte
	for seq := uint64(0); seq < 2; seq++ {
		h, p := base(seq)
		trunc = frame.Encode(trunc, h, p)
	}
	{
		h, p := base(2)
		full := frame.Encode(nil, h, p)
		trunc = append(trunc, full[:len(full)/2]...)
	}
	if err := atomicWrite(filepath.Join(dir, "truncated.frm"), trunc); err != nil {
		return "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO, Message: err.Error(), Fatal: true}}
	}

	// Checksum mismatch: metadata digest disagrees with the payload.
	var good []byte
	var metaBuf bytes.Buffer
	for seq := uint64(0); seq < 3; seq++ {
		h, p := base(seq)
		good = frame.Encode(good, h, p)
		sum := sha256.Sum256(p)
		digest := hex.EncodeToString(sum[:])
		if seq == 1 {
			digest = hex.EncodeToString(make([]byte, 32))
		}
		fmt.Fprintf(&metaBuf, "{\"camera_id\":\"check\",\"seq\":%d,\"sha256\":%q}\n", seq, digest)
	}
	if err := atomicWrite(filepath.Join(dir, "checksum.frm"), good); err != nil {
		return "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO, Message: err.Error(), Fatal: true}}
	}
	if err := atomicWrite(filepath.Join(dir, "checksum.meta"), metaBuf.Bytes()); err != nil {
		return "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO, Message: err.Error(), Fatal: true}}
	}

	// Timestamp wrap: a 32-bit device clock crossing zero.
	var wrap []byte
	for i, seq := range []uint64{65533, 65534, 0, 1} {
		ts := (clock.WrapMod - 2*3000 + uint64(i)*3000) % clock.WrapMod
		payload := []byte{byte(i)}
		wrap = frame.Encode(wrap, frame.Header{
			Camera: "check", Seq: uint64(i), DeviceTS: ts, Rate: uint32(clock.MasterRate),
		}, payload)
		_ = seq
	}
	if err := atomicWrite(filepath.Join(dir, "wrap.frm"), wrap); err != nil {
		return "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO, Message: err.Error(), Fatal: true}}
	}

	// Duplicate: the same record twice.
	var dup []byte
	h, p := base(0)
	dup = frame.Encode(dup, h, p)
	dup = frame.Encode(dup, h, p)
	if err := atomicWrite(filepath.Join(dir, "duplicate.frm"), dup); err != nil {
		return "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO, Message: err.Error(), Fatal: true}}
	}

	samples := BoundarySamples{
		Truncated:        filepath.Join(dir, "truncated.frm"),
		ChecksumMismatch: filepath.Join(dir, "checksum.frm"),
		TimestampWrap:    filepath.Join(dir, "wrap.frm"),
		Duplicate:        filepath.Join(dir, "duplicate.frm"),
	}
	data, err := json.MarshalIndent(samples, "", "  ")
	if err != nil {
		return "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO, Message: err.Error(), Fatal: true}}
	}
	path := filepath.Join(dir, "samples.json")
	if err := atomicWrite(path, data); err != nil {
		return "", fault.List{{Kind: fault.KindWriteFailure, Stage: fault.StageIO, Message: err.Error(), Fatal: true}}
	}
	return path, nil
}

// atomicWrite writes data through a temporary file and renames it.
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// SortCorruptSeqs normalises the manifest for deterministic JSON output.
func (m *Manifest) SortCorruptSeqs() {
	for k := range m.CorruptSeqs {
		sort.Slice(m.CorruptSeqs[k], func(i, j int) bool {
			return m.CorruptSeqs[k][i] < m.CorruptSeqs[k][j]
		})
	}
}

// TotalCorrupt sums corrupted sequences across cameras.
func (m *Manifest) TotalCorrupt() int {
	n := 0
	for _, v := range m.CorruptSeqs {
		n += len(v)
	}
	return n
}

// TotalFrames sums the declared frame counts.
func (m *Manifest) TotalFrames() int {
	n := 0
	for _, c := range m.Cameras {
		n += int(c.Frames)
	}
	return n
}
