package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"mediaroute/internal/buffer"
	"mediaroute/internal/checkpoint"
	"mediaroute/internal/manifest"
	"mediaroute/internal/model"
	"mediaroute/internal/mp4"
	"mediaroute/internal/packet"
	"mediaroute/internal/pipeline"
	"mediaroute/internal/recovery"
	"mediaroute/internal/sync"
)

var stderr = os.Stderr

// runDemo executes one demo run and returns the process exit code.
func runDemo(p Params, chkPath string, resume, expectError bool) int {
	start := time.Now()
	chk, w, err := setupRun(p, chkPath, resume)
	if err != nil {
		return fail(err, expectError)
	}
	p = w.Params
	ctx := pipeline.NewContext()
	reused := 0
	ctx.Values["world"], ctx.Values["chk"], ctx.Values["reused"] = w, chk, &reused

	pl, err := buildPipeline(chkPath)
	if err != nil {
		return fail(err, expectError)
	}
	if err := pl.Run(ctx); err != nil {
		return fail(err, expectError)
	}
	if expectError {
		fmt.Println("expected_error=none outcome=unexpected_success")
		return model.ExitInternal
	}
	if resume {
		fmt.Printf("resume_elapsed_ms=%d\n", time.Since(start).Milliseconds())
		fmt.Printf("reused_segments=%d\n", reused)
		fmt.Printf("output_sha256=%s\n", ctx.Values["final"])
		return model.ExitOK
	}
	fmt.Printf("manifest_segments=%d\n", ctx.Values["manifest_segments"])
	fmt.Printf("duplicate_packets=%d\n", ctx.Values["duplicates"])
	fmt.Printf("reordered_packets=%d\n", ctx.Values["reordered"])
	fmt.Printf("recovered_packets=%d\n", ctx.Values["recovered"])
	fmt.Printf("av_skew_ms=%d\n", ctx.Values["skew_ms"])
	fmt.Printf("output_sha256=%s\n", ctx.Values["final"])
	return model.ExitOK
}

// setupRun prepares the checkpoint (fresh or resumed) and the world.
func setupRun(p Params, chkPath string, resume bool) (*checkpoint.Checkpoint, *World, error) {
	if !resume {
		pj, _ := json.Marshal(p)
		return &checkpoint.Checkpoint{
			InputDigest: p.Digest(),
			Cursors:     map[string]uint64{},
			Extra:       map[string]string{"params": string(pj)},
		}, GenWorld(p), nil
	}
	chk, err := checkpoint.Load(chkPath)
	if err != nil {
		return nil, nil, err
	}
	if chk == nil {
		return nil, nil, &model.MediaError{Code: model.ErrCheckpoint,
			Module: "checkpoint", Msg: "no checkpoint at " + chkPath}
	}
	var stored Params
	if err := json.Unmarshal([]byte(chk.Extra["params"]), &stored); err != nil {
		return nil, nil, &model.MediaError{Code: model.ErrCheckpoint,
			Module: "checkpoint", Msg: "checkpoint params unreadable", Cause: err}
	}
	if stored.Seed != p.Seed {
		return nil, nil, &model.MediaError{Code: model.ErrCheckpoint,
			Module: "checkpoint",
			Msg:    fmt.Sprintf("seed changed: checkpoint=%d run=%d", stored.Seed, p.Seed)}
	}
	if err := chk.VerifyDigest(stored.Digest()); err != nil {
		return nil, nil, err
	}
	return chk, GenWorld(stored), nil
}

// fail prints a structured error and returns the mapped exit code.
func fail(err error, expectError bool) int {
	if me, ok := err.(*model.MediaError); ok {
		if me.Code == model.ErrCorrupt {
			fmt.Printf("first_bad_segment=%d\n", me.Seq)
			fmt.Printf("error_offset_byte=%d\n", me.Timestamp)
		}
		fmt.Printf("error_code=%s\n", me.Code)
		fmt.Fprintf(stderr, "error: %s\n", me.Error())
	} else {
		fmt.Printf("error_code=%s\n", model.ErrInternal)
		fmt.Fprintf(stderr, "error: %v\n", err)
	}
	if expectError {
		return model.ExitOK
	}
	return model.ExitCode(err)
}

func buildPipeline(chkPath string) (*pipeline.Pipeline, error) {
	dep := func(id string, deps ...string) model.Node {
		return model.Node{ID: id, Deps: deps}
	}
	return pipeline.New(
		pipeline.Stage{Node: dep("manifest"), Run: stageManifest},
		pipeline.Stage{Node: dep("reorder", "manifest"), Run: stageReorder},
		pipeline.Stage{Node: dep("buffer", "reorder"), Run: stageBuffer},
		pipeline.Stage{Node: dep("recovery", "buffer"), Run: stageRecovery},
		pipeline.Stage{Node: dep("sync", "recovery"), Run: stageSync},
		pipeline.Stage{Node: dep("inspect", "sync"), Run: func(c *pipeline.Context) error {
			return stageInspect(c, chkPath)
		}},
	)
}

func stageManifest(ctx *pipeline.Context) error {
	w := ctx.Values["world"].(*World)
	segs, err := manifest.ParseHLS(strings.NewReader(w.Playlist), "v1")
	if err != nil {
		return err
	}
	ctx.Values["manifest_segments"] = len(segs)
	return nil
}

func stageReorder(ctx *pipeline.Context) error {
	w := ctx.Values["world"].(*World)
	vr := packet.NewReorderer("v1", 8)
	for i := range w.VideoArrival {
		if _, err := vr.Push(&w.VideoArrival[i]); err != nil {
			return err
		}
	}
	_, vstats := vr.Flush()
	ar := packet.NewReorderer("a1", 8)
	for i := range w.AudioArrival {
		if _, err := ar.Push(&w.AudioArrival[i]); err != nil {
			return err
		}
	}
	ar.Flush()
	ctx.Values["duplicates"], ctx.Values["reordered"] = vstats.Duplicates, vstats.Reordered
	ctx.Values["gaps"] = vstats.Gaps
	return nil
}

func stageBuffer(ctx *pipeline.Context) error {
	w := ctx.Values["world"].(*World)
	est := buffer.NewEstimator(1000)
	buf := buffer.NewBuffer(est, 20, 500, 120, float64(w.Bandwidth[0]))
	var t int64
	for _, rate := range w.Bandwidth {
		for k := 0; k < 20; k++ {
			buf.Update(t, rate/8/10) // 100 ms worth of bytes
			t += 100
		}
	}
	ctx.Values["audio_priority_seen"] = buf.AudioPriority
	return nil
}

func stageRecovery(ctx *pipeline.Context) error {
	w := ctx.Values["world"].(*World)
	rec := recovery.New("v1", videoSkew, videoStep)
	for _, pa := range w.Parity {
		rec.AddParity(pa)
	}
	recs, err := rec.Recover(ctx.Values["gaps"].([]packet.Gap))
	if err != nil {
		return err // unrecoverable gap: stop, never compress the timeline
	}
	ctx.Values["recovered"] = len(recs)
	return nil
}

func stageSync(ctx *pipeline.Context) error {
	w := ctx.Values["world"].(*World)
	pairs := make([]sync.Pair, w.Segments)
	for i := range pairs {
		pairs[i] = sync.Pair{
			Audio: audioTS(i * w.PktsPerSeg),
			Video: videoTS(i * w.PktsPerSeg),
		}
	}
	al := (&sync.Aligner{ThresholdMs: 40, AudioRate: 48000, VideoRate: 90000}).Align(pairs)
	ctx.Values["skew_ms"] = al.ResidualMs
	return nil
}

func stageInspect(ctx *pipeline.Context, chkPath string) error {
	w := ctx.Values["world"].(*World)
	chk := ctx.Values["chk"].(*checkpoint.Checkpoint)
	reused := ctx.Values["reused"].(*int)
	var hashes []string
	for i := 0; i < w.Segments; i++ {
		if rec, ok := chk.CommittedIndex(i); ok { // resume: skip validated data
			hashes = append(hashes, rec.SHA256)
			*reused++
			continue
		}
		data := buildSegment(w, i)
		if i == w.CorruptSegment {
			data = data[:len(data)-7] // truncated fragment
		}
		if err := mp4.Inspect(i, data); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])
		hashes = append(hashes, h)
		chk.Committed = append(chk.Committed, checkpoint.SegmentRecord{Index: i, SHA256: h})
		chk.Cursors["v1"] = uint64((i + 1) * w.PktsPerSeg)
		chk.Cursors["a1"] = uint64((i + 1) * w.PktsPerSeg)
		if err := checkpoint.Save(chkPath, chk); err != nil {
			return err
		}
	}
	fin := sha256.New()
	for _, h := range hashes {
		fin.Write([]byte(h))
	}
	chk.FinalSHA256 = hex.EncodeToString(fin.Sum(nil))
	if err := checkpoint.Save(chkPath, chk); err != nil {
		return err
	}
	ctx.Values["final"] = chk.FinalSHA256
	return nil
}

// buildSegment assembles a deterministic fMP4-style fragment for one
// segment from the recovered timeline payloads.
func buildSegment(w *World, i int) []byte {
	var data []byte
	data = mp4.AppendBox(data, "styp", []byte("msdhmsix"))
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(i))
	data = mp4.AppendBox(data, "moof", hdr[:])
	var mdat []byte
	for k := 0; k < w.PktsPerSeg; k++ {
		seq := i*w.PktsPerSeg + k
		for _, pl := range [][]byte{w.VideoPayloads[seq], w.AudioPayloads[seq]} {
			var l [4]byte
			binary.BigEndian.PutUint32(l[:], uint32(len(pl)))
			mdat = append(mdat, l[:]...)
			mdat = append(mdat, pl...)
		}
	}
	return mp4.AppendBox(data, "mdat", mdat)
}
