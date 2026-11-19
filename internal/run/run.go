// Package run executes the review batch pipeline: load inputs, align
// timelines, render blocks with checkpoints, resume interrupted batches
// and aggregate the final verification hash.
package run

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"review/internal/batch"
	"review/internal/clock"
	"review/internal/issues"
	"review/internal/media"
	"review/internal/render"
	"review/internal/report"
	"review/internal/safepath"
	"review/internal/subtitle"
	"review/internal/timeline"
)

// Config is the run configuration persisted in the work directory.
type Config struct {
	Cams         int      `json:"cams"`
	FPS          string   `json:"fps"`
	BlockFrames  int64    `json:"block_frames"`
	FramesPerCam int64    `json:"frames_per_cam"`
	DayWrap      bool     `json:"day_wrap"`
	Inject       []string `json:"inject,omitempty"`
}

const maxInFlight = 4 // bounds concurrent blocks and pending writes

type camData struct {
	blocks   []timeline.Block
	rtp      []clock.RTPRecord
	rtpFirst uint64
	seqs     []uint16
	hasRTP   bool
}

func loadCam(root, work string, cam int, cfg Config, r clock.Rate, sum *report.Summary, inputs *[]string) (camData, error) {
	var cd camData
	base := fmt.Sprintf("inputs/cam%02d", cam)
	read := func(ext string) ([]byte, string, error) {
		p, err := safepath.Join(work, base+ext)
		if err != nil {
			return nil, "", err
		}
		data, err := os.ReadFile(p)
		*inputs = append(*inputs, p)
		return data, p, err
	}
	var cues []subtitle.Cue
	for _, ext := range []string{".srt", ".vtt"} {
		data, p, err := read(ext)
		if err != nil {
			return cd, err
		}
		c, iss := subtitle.Parse(data, filepath.Base(p), p)
		cues = append(cues, c...)
		sum.Issues = append(sum.Issues, iss...)
	}
	tcData, tcPath, err := read(".tc.csv")
	if err != nil {
		return cd, err
	}
	tcs, iss := timeline.ParseTimecodeList(tcData, tcPath, r, cfg.DayWrap)
	sum.Issues = append(sum.Issues, iss...)

	rtpPath, err := safepath.Join(work, base+".rtp")
	if err != nil {
		return cd, err
	}
	*inputs = append(*inputs, rtpPath)
	raw, seqs, err := clock.ReadRTPStream(rtpPath)
	if err != nil {
		return cd, err
	}
	delta := uint32(90000 * r.Den / r.Num)
	cd.rtp, iss = clock.UnwrapRTP(raw, delta)
	sum.Issues = append(sum.Issues, iss...)
	if len(cd.rtp) > 0 {
		cd.hasRTP = true
		cd.rtpFirst = cd.rtp[0].Unwrapped
		cd.seqs = seqs
	}

	mp4Path, err := safepath.Join(work, base+".mp4")
	if err != nil {
		return cd, err
	}
	*inputs = append(*inputs, mp4Path)
	mrep, miss := media.Inspect(mp4Path)
	sum.Issues = append(sum.Issues, miss...)
	if mrep.DamageOffset >= 0 && (sum.DamageOffsetBytes < 0 || mrep.DamageOffset < sum.DamageOffsetBytes) {
		sum.DamageOffsetBytes = mrep.DamageOffset
	}

	cd.blocks = timeline.BuildBlocks(cam, cfg.FramesPerCam, cfg.BlockFrames, cues, tcs, r)
	return cd, nil
}

// alignmentErrors counts frames whose RTP-implied time disagrees with the
// MP4 decode time by half a frame or more.
func alignmentErrors(cams []camData, r clock.Rate) int {
	n := 0
	for _, cd := range cams {
		if !cd.hasRTP {
			continue
		}
		for i, rec := range cd.rtp {
			frame := float64(int(cd.seqs[i]) - int(cd.seqs[0])) // sequence number maps to frame index
			tRTP := float64(rec.Unwrapped-cd.rtpFirst) / 90000
			if math.Abs(tRTP*float64(r.Num)/float64(r.Den)-frame) >= 0.5 {
				n++
			}
		}
	}
	return n
}

// Run executes (or resumes) the batch and returns the summary.
func Run(work string, cfg Config, interruptAfter int, sig *atomic.Bool) (report.Summary, error) {
	start := time.Now()
	sum := report.Summary{Status: "complete", DamageOffsetBytes: -1}
	rate, err := clock.ParseRate(cfg.FPS)
	if err != nil {
		return sum, err
	}
	root, err := safepath.Join(work, ".")
	if err != nil {
		return sum, err
	}
	cams := make([]camData, cfg.Cams)
	var inputs []string
	for cam := 0; cam < cfg.Cams; cam++ {
		cams[cam], err = loadCam(root, work, cam, cfg, rate, &sum, &inputs)
		if err != nil {
			return sum, err
		}
	}
	fp, err := batch.FingerprintFiles(inputs)
	if err != nil {
		return sum, err
	}
	digest, err := batch.DigestConfig(cfg)
	if err != nil {
		return sum, err
	}
	total := 0
	for _, cd := range cams {
		total += len(cd.blocks)
	}
	sum.BlocksTotal = total

	cp, err := batch.Load(work)
	if err != nil {
		sum.VerificationFailed = true
		sum.Status = "failed"
		sum.Issues = append(sum.Issues, issues.Issue{Kind: issues.CheckpointCorrupt, Msg: err.Error()})
		return sum, nil
	}
	if cp == nil || cp.InputFingerprint != fp || cp.ConfigDigest != digest {
		cp = &batch.Checkpoint{Version: 1, InputFingerprint: fp, ConfigDigest: digest, TotalBlocks: total}
	}
	// Verify completed blocks; recompute from the last safe block.
	safe := 0
	for _, b := range cp.Done {
		ok, err := render.VerifyBlock(work, b.Cam, b.Index, b.SHA256)
		if err != nil || !ok {
			break
		}
		safe++
	}
	cp.Done = cp.Done[:safe]
	sum.RecoveredBlocks = safe

	type task struct{ cam, idx int }
	var tasks []task
	for cam := 0; cam < cfg.Cams; cam++ {
		for _, blk := range cams[cam].blocks {
			if !cp.Has(cam, blk.Index) {
				tasks = append(tasks, task{cam, blk.Index})
			}
		}
	}
	type result struct {
		cam, idx, frames int
		sha              string
		err              error
	}
	results := make(chan result, maxInFlight)
	done0 := len(cp.Done)
	next, inflight, stopped := 0, 0, false
	for next < len(tasks) || inflight > 0 {
		for next < len(tasks) && inflight < maxInFlight && !stopped && !sig.Load() {
			t := tasks[next]
			next++
			inflight++
			go func() {
				man, err := render.RenderBlock(work, cams[t.cam].blocks[t.idx], rate, rate.DropSupported(), digest)
				if err != nil {
					results <- result{err: err}
					return
				}
				results <- result{cam: t.cam, idx: t.idx, frames: len(man.Frames), sha: man.SHA256}
			}()
		}
		if inflight == 0 {
			break
		}
		res := <-results
		inflight--
		if res.err != nil {
			return sum, res.err
		}
		cp.Add(batch.BlockState{Cam: res.cam, Index: res.idx, Frames: res.frames, SHA256: res.sha})
		if err := cp.Save(work); err != nil {
			return sum, err
		}
		if interruptAfter > 0 && len(cp.Done)-done0 >= interruptAfter {
			stopped = true
		}
		if sig.Load() {
			stopped = true
		}
	}
	if next < len(tasks) {
		sum.Status = "interrupted"
	}
	sum.BlocksDone = len(cp.Done)

	// Aggregate the final hash over all manifests in deterministic order.
	sort.Slice(cp.Done, func(i, j int) bool {
		if cp.Done[i].Cam != cp.Done[j].Cam {
			return cp.Done[i].Cam < cp.Done[j].Cam
		}
		return cp.Done[i].Index < cp.Done[j].Index
	})
	var mans []*render.Manifest
	for _, b := range cp.Done {
		man, err := render.LoadManifest(work, b.Cam, b.Index)
		if err != nil {
			sum.VerificationFailed = true
			sum.Status = "failed"
			return sum, nil
		}
		mans = append(mans, man)
	}
	sum.SHA256, sum.FramesExported = render.Aggregate(mans)
	sum.AlignmentErrorFrames = alignmentErrors(cams, rate)
	sum.ResumeMs = time.Since(start).Milliseconds()
	return sum, nil
}
