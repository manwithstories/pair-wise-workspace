package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mediamend/mediamend/internal/clock"
	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/frame"
)

// streamSpec describes one synthetic camera stream written for a test.
type streamSpec struct {
	camera   string
	n        int
	startTS  uint64
	step     uint64
	corrupt  map[uint64]bool // seq -> payload does not match metadata digest
	dup      []uint64        // sequence numbers written twice
	outOfOrd []uint64        // sequence numbers written swapped with the next
}

// metaWire is the on-disk metadata line used by these tests.
type metaWire struct {
	Camera   string `json:"camera_id"`
	Seq      uint64 `json:"seq"`
	DeviceTS uint64 `json:"device_ts"`
	Length   uint32 `json:"body_length"`
	Offset   int64  `json:"offset"`
	SHA256   string `json:"sha256"`
	Keyframe bool   `json:"keyframe,omitempty"`
}

// writeStream emits a frame file and its metadata sidecar. Corrupted
// sequence numbers get a deliberately wrong digest in the sidecar.
func writeStream(t *testing.T, dir string, spec streamSpec) Source {
	t.Helper()
	path := filepath.Join(dir, spec.camera+".frm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var meta bytes.Buffer
	enc := json.NewEncoder(&meta)

	order := make([]int, 0, spec.n)
	for i := 0; i < spec.n; i++ {
		order = append(order, i)
	}
	for _, seq := range spec.outOfOrd {
		for i := 0; i < len(order)-1; i++ {
			if uint64(order[i]) == seq && order[i+1] == int(seq)+1 {
				order[i], order[i+1] = order[i+1], order[i]
				break
			}
		}
	}
	var buf []byte
	offset := int64(0)
	for _, i := range order {
		seq := uint64(i)
		payload := bytes.Repeat([]byte{byte(0x40 + i%16)}, 32+i%40)
		h := frame.Header{
			Camera: spec.camera, Seq: seq, DeviceTS: spec.startTS + seq*spec.step,
			Rate: uint32(clock.MasterRate),
		}
		if i%10 == 0 {
			h.Flags = frame.FlagKeyframe
		}
		buf = buf[:0]
		buf = frame.Encode(buf, h, payload)
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		digest := hex.EncodeToString(sum[:])
		if spec.corrupt[seq] {
			digest = strings.Repeat("00", 32)
		}
		if err := enc.Encode(metaWire{
			Camera: spec.camera, Seq: seq, DeviceTS: h.DeviceTS,
			Length: uint32(len(payload)), Offset: offset, SHA256: digest,
			Keyframe: h.Flags.Has(frame.FlagKeyframe),
		}); err != nil {
			t.Fatal(err)
		}
		offset += int64(len(buf))
		for _, dupSeq := range spec.dup {
			if dupSeq == seq {
				if _, err := f.Write(buf); err != nil {
					t.Fatal(err)
				}
				offset += int64(len(buf))
			}
		}
	}
	metaPath := filepath.Join(dir, spec.camera+".meta")
	if err := os.WriteFile(metaPath, meta.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	md, err := frame.LoadMetadata(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	return Source{
		Camera: spec.camera,
		File:   path,
		Meta:   md,
		Clock: clock.Config{
			Camera: spec.camera, Rate: clock.MasterRate, FrameStep: spec.step,
			AnchorSeq: 0, AnchorDeviceTS: spec.startTS, AnchorMasterTS: 0,
		},
	}
}

func TestRunnerEndToEnd(t *testing.T) {
	dir := t.TempDir()
	src := writeStream(t, dir, streamSpec{camera: "cam1", n: 200, step: 3000})
	r := New(FlowConfig{QueueCap: 4, Workers: 2, MaxInflight: 8, MaxPayloadBytes: 1 << 20})
	res := r.Run(context.Background(), []Source{src})

	if res.Canceled {
		t.Fatal("unexpected cancellation")
	}
	if len(res.Items) != 200 {
		t.Fatalf("items=%d want 200 (faults=%v)", len(res.Items), res.Faults)
	}
	if res.Metrics.Parsed != 200 || res.Metrics.Verified != 200 {
		t.Fatalf("metrics=%+v", res.Metrics)
	}
	if res.Metrics.PeakInflight > 8 {
		t.Fatalf("peak inflight=%d exceeds MaxInflight 8", res.Metrics.PeakInflight)
	}
	// Timestamps must be strictly increasing and aligned to the 3000-tick step.
	for i := 1; i < len(res.Items); i++ {
		if res.Items[i].MasterTS <= res.Items[i-1].MasterTS {
			t.Fatalf("timeline not increasing at %d", i)
		}
		if res.Items[i].MasterTS-res.Items[i-1].MasterTS != 3000 {
			t.Fatalf("alignment error at %d: %d", i, res.Items[i].MasterTS-res.Items[i-1].MasterTS)
		}
	}
	if len(res.Faults) != 0 {
		t.Fatalf("unexpected faults: %v", res.Faults)
	}
}

func TestRunnerBackpressureSlowConsumer(t *testing.T) {
	dir := t.TempDir()
	src := writeStream(t, dir, streamSpec{camera: "cam1", n: 60, step: 3000})
	r := New(FlowConfig{
		QueueCap: 2, Workers: 2, MaxInflight: 4, MaxPayloadBytes: 1 << 20,
		SlowConsumer: 2 * time.Millisecond,
	})
	res := r.Run(context.Background(), []Source{src})
	if len(res.Items) != 60 {
		t.Fatalf("items=%d want 60", len(res.Items))
	}
	if res.Metrics.BackpressureHits == 0 {
		t.Fatalf("slow consumer produced no backpressure: %+v", res.Metrics)
	}
	if res.Metrics.WaitNanos <= 0 {
		t.Fatalf("no wait recorded: %+v", res.Metrics)
	}
	if res.Metrics.PeakInflight > 4 {
		t.Fatalf("peak inflight=%d exceeds 4", res.Metrics.PeakInflight)
	}
}

func TestRunnerCancelKeepsCommitted(t *testing.T) {
	dir := t.TempDir()
	src := writeStream(t, dir, streamSpec{camera: "cam1", n: 300, step: 3000})
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	r := New(FlowConfig{
		QueueCap: 2, Workers: 1, MaxInflight: 4, MaxPayloadBytes: 1 << 20,
		SlowConsumer: time.Millisecond,
		Progress: func(e Event) {
			if e.Stage == StageVerify && e.Seq >= 20 {
				once.Do(cancel)
			}
		},
	})
	res := r.Run(ctx, []Source{src})
	if !res.Canceled {
		t.Fatal("run was not marked canceled")
	}
	if len(res.Items) == 0 || len(res.Items) >= 300 {
		t.Fatalf("committed=%d want a nonzero prefix", len(res.Items))
	}
	if res.Metrics.Committed != len(res.Items) {
		t.Fatalf("committed metric=%d items=%d", res.Metrics.Committed, len(res.Items))
	}
	if res.Faults.Count(fault.KindCanceled) != 1 {
		t.Fatalf("expected one canceled fault: %v", res.Faults)
	}
	// Everything committed is still in order.
	for i := 1; i < len(res.Items); i++ {
		if res.Items[i].MasterTS <= res.Items[i-1].MasterTS {
			t.Fatalf("committed prefix not increasing at %d", i)
		}
	}
}

func TestRunnerChecksumMismatchReported(t *testing.T) {
	dir := t.TempDir()
	src := writeStream(t, dir, streamSpec{
		camera: "cam1", n: 40, step: 3000,
		corrupt: map[uint64]bool{7: true, 23: true},
	})
	r := New(FlowConfig{QueueCap: 4, Workers: 2, MaxInflight: 8, MaxPayloadBytes: 1 << 20})
	res := r.Run(context.Background(), []Source{src})
	found := map[uint64]bool{}
	for _, f := range res.Faults {
		if f.Kind == fault.KindChecksumMismatch {
			found[f.Seq] = true
			if f.Offset < 0 {
				t.Fatalf("checksum fault has no byte offset: %+v", f)
			}
		}
	}
	for _, seq := range []uint64{7, 23} {
		if !found[seq] {
			t.Fatalf("corruption at seq %d not reported; faults=%v", seq, res.Faults)
		}
	}
	if len(res.Items) != 40 {
		t.Fatalf("items=%d want 40", len(res.Items))
	}
}

func TestRunnerDuplicateAndReordering(t *testing.T) {
	dir := t.TempDir()
	src := writeStream(t, dir, streamSpec{
		camera: "cam1", n: 50, step: 3000,
		dup: []uint64{10}, outOfOrd: []uint64{20},
	})
	r := New(FlowConfig{QueueCap: 4, Workers: 2, MaxInflight: 8, MaxPayloadBytes: 1 << 20})
	res := r.Run(context.Background(), []Source{src})
	if len(res.Items) != 50 {
		t.Fatalf("items=%d want 50 after duplicate suppression (faults=%v)", len(res.Items), res.Faults)
	}
	if res.Faults.Count(fault.KindDuplicateFrame) == 0 {
		t.Fatalf("duplicate not reported: %v", res.Faults)
	}
	for i := 1; i < len(res.Items); i++ {
		if res.Items[i].MasterTS <= res.Items[i-1].MasterTS {
			t.Fatalf("reorder failed at %d: %d -> %d", i, res.Items[i-1].MasterTS, res.Items[i].MasterTS)
		}
	}
}
