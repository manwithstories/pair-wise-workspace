package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"aircheck/internal/adapter"
	"aircheck/internal/checkpoint"
	"aircheck/internal/media"
	"aircheck/internal/render"
)

const (
	defaultMaxSlopeDev = 1e-3 // 漂移斜率异常阈值 ms/tick
	hashWorkers        = 4    // 哈希校验并发上限（有界）
	maxOutFrames       = 20000
)

// verifyHashes 有界并发校验源帧 SHA-256，返回坏帧下标集合。
func verifyHashes(ctx context.Context, root string, m *adapter.Manifest, maxBytes int64) (map[int]bool, error) {
	ok := make([]bool, len(m.Frames))
	errs := make([]error, len(m.Frames))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < hashWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				good, err := adapter.VerifySHA256(root, m.Frames[i].File, m.Frames[i].SHA256, maxBytes)
				ok[i] = good
				errs[i] = err
			}
		}()
	}
	for i := range m.Frames {
		if ctx.Err() != nil {
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	bad := map[int]bool{}
	for i := range m.Frames {
		if errs[i] != nil {
			return nil, fmt.Errorf("pipeline: verify %s: %w", m.Frames[i].File, errs[i])
		}
		if !ok[i] {
			bad[i] = true
		}
	}
	return bad, nil
}

// buildTracks 分轨构建时间线：漂移校正、回绕展开、VFR、GAP。
func buildTracks(m *adapter.Manifest, badIdx map[int]bool) ([]render.TrackSlots, int, error) {
	byTrack := map[string][]media.FrameInput{}
	for i, f := range m.Frames {
		byTrack[f.Track] = append(byTrack[f.Track], media.FrameInput{
			Track: f.Track, Seq: f.Seq, PTS: f.PTS, Dur: f.Dur,
			Hash: f.SHA256, File: f.File, Bad: badIdx[i],
		})
	}
	anchorsByTrack := map[string][]media.Anchor{}
	for _, a := range m.Anchors {
		anchorsByTrack[a.Track] = append(anchorsByTrack[a.Track], media.Anchor{PTS: a.PTS, RefMs: a.RefMs})
	}
	tracks := make([]render.TrackSlots, 0, len(m.Tracks))
	gapFrames := 0
	for _, tr := range m.Tracks {
		var dc *media.DriftCorrection
		if anc := anchorsByTrack[tr.ID]; len(anc) > 0 {
			anc, err := unwrapTrackAnchors(tr, anc, byTrack[tr.ID])
			if err != nil {
				return nil, 0, err
			}
			fit, err := media.FitDrift(m.Timebase, anc, defaultMaxSlopeDev)
			if err != nil {
				return nil, 0, fmt.Errorf("track %s: %w", tr.ID, err)
			}
			dc = fit
		}
		slots, err := media.BuildTrackTimeline(byTrack[tr.ID], m.Timebase, dc, tr.Modulus, 1000/tr.FPS)
		if err != nil {
			return nil, 0, err
		}
		for _, s := range slots {
			if s.Gap {
				gapFrames++
			}
		}
		tracks = append(tracks, render.TrackSlots{Track: tr.ID, Priority: tr.Priority, FPS: tr.FPS, Slots: slots})
	}
	sort.SliceStable(tracks, func(i, j int) bool {
		if tracks[i].Priority != tracks[j].Priority {
			return tracks[i].Priority < tracks[j].Priority
		}
		return tracks[i].Track < tracks[j].Track
	})
	return tracks, gapFrames, nil
}

// unwrapTrackAnchors 对回绕轨道的锚点 PTS 做与帧一致的展开对齐。
// 锚点序列独立展开后，按与首个锚点原始 PTS 最接近的帧对齐整数个周期。
func unwrapTrackAnchors(tr adapter.Track, anchors []media.Anchor, frames []media.FrameInput) ([]media.Anchor, error) {
	if tr.Modulus <= 0 || len(anchors) == 0 {
		return anchors, nil
	}
	rawA := make([]int64, len(anchors))
	for i, a := range anchors {
		rawA[i] = a.PTS
	}
	unA, err := media.UnwrapPTS(rawA, tr.Modulus)
	if err != nil {
		return nil, fmt.Errorf("track %s anchors: %w", tr.ID, err)
	}
	fs := make([]media.FrameInput, len(frames))
	copy(fs, frames)
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Seq < fs[j].Seq })
	rawF := make([]int64, len(fs))
	for i, f := range fs {
		rawF[i] = f.PTS
	}
	unF, err := media.UnwrapPTS(rawF, tr.Modulus)
	if err != nil {
		return nil, fmt.Errorf("track %s: %w", tr.ID, err)
	}
	j := 0
	for i := range rawF {
		if abs64s(rawF[i]-rawA[0]) < abs64s(rawF[j]-rawA[0]) {
			j = i
		}
	}
	shift := tr.Modulus * int64(math.Round(float64(unF[j]-unA[0])/float64(tr.Modulus)))
	out := make([]media.Anchor, len(anchors))
	for i, a := range anchors {
		out[i] = media.Anchor{PTS: unA[i] + shift, RefMs: a.RefMs}
	}
	return out, nil
}

func abs64s(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func sourcesDigest(m *adapter.Manifest) string {
	parts := make([]string, 0, len(m.Frames))
	for _, f := range m.Frames {
		parts = append(parts, f.Track+"|"+f.File+"|"+f.SHA256)
	}
	sort.Strings(parts)
	return checkpoint.Digest(parts...)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ensureOutDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("pipeline: empty out dir")
	}
	return os.MkdirAll(filepath.Join(dir, "frames"), 0o755)
}
