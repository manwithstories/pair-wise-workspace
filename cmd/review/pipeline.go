// Command review 演示多机位现场后期交付链路：
// 清单解析 → 分片读取 → 丢包恢复 → 统一时基合成 → 帧导出，
// 全程 WAL 事务保护，输出验收指标。
package main

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"review/internal/fault"
	"review/internal/fixture"
	"review/internal/fragment"
	"review/internal/journal"
	"review/internal/manifest"
	"review/internal/metrics"
	"review/internal/packet"
	"review/internal/render"
	"review/internal/timeline"
)

type options struct {
	demo         bool
	injectLoss   int
	truncateTail bool
	dropIndex    int
	repeat       int
	workdir      string
}

// cameraMedia 单机位解析结果。
type cameraMedia struct {
	id       string
	videoHi  *manifest.Representation
	videoMid *manifest.Representation
	audio    *manifest.Representation
	hiSegs   []*fragment.Segment
	midSegs  []*fragment.Segment
	audSegs  []*fragment.Segment
}

type pipeState struct {
	layout   *fixture.Layout
	cams     []cameraMedia
	hls      *manifest.HLS
	hlsSegs  []*fragment.Segment
	payloads [][]byte // 恢复后的视频负载（按机位×帧）
	audio    [][]byte // 恢复后的音频负载
	tl       *timeline.Timeline
	lost     int
	recovered int
	segMs    []float64
	frames   []render.Frame
	sha      string
	digests  map[string]string
}

func fatalErr(errs []*fault.Error) error {
	for _, e := range errs {
		if e != nil && !e.Recoverable {
			return e
		}
	}
	return nil
}

// runPipeline 在 dir 下执行完整链路，返回报告与全部故障。
func runPipeline(dir string, opts options) (*metrics.Report, []*fault.Error, error) {
	st := &pipeState{digests: map[string]string{}}
	var faults []*fault.Error
	layout, err := fixture.Generate(filepath.Join(dir, "fixtures"))
	if err != nil {
		return nil, nil, err
	}
	st.layout = layout
	if opts.truncateTail {
		if _, err := layout.ApplyTruncateTail(); err != nil {
			return nil, nil, err
		}
	}
	if opts.dropIndex > 0 {
		if _, err := layout.ApplyDropIndex(opts.dropIndex); err != nil {
			return nil, nil, err
		}
	}
	walPath := filepath.Join(dir, "wal.log")
	wal, err := journal.Open(walPath)
	if err != nil {
		return nil, nil, err
	}
	defer wal.Close()
	tx := journal.NewTx(wal, filepath.Join(dir, "out"))
	inputDigest, err := journal.HashDir(filepath.Join(dir, "fixtures"))
	if err != nil {
		return nil, nil, err
	}

	stages := []struct {
		name string
		fn   func(string) (string, error)
	}{
		{"parse", func(d string) (string, error) { return st.stageParse(d, &faults) }},
		{"recover", func(d string) (string, error) { return st.stageRecover(d, &faults, opts) }},
		{"mix", func(d string) (string, error) { return st.stageMix(d, &faults) }},
		{"render", func(d string) (string, error) { return st.stageRender(d) }},
	}
	in := inputDigest
	for _, s := range stages {
		digest, err := tx.Run(s.name, in, s.fn)
		if err != nil {
			return nil, faults, err
		}
		st.digests[s.name] = digest
		in = digest
	}
	if err := fatalErr(faults); err != nil {
		return nil, faults, err
	}
	// 回滚演练：模拟提交中断，须回滚到最后有效边界
	rollbackOK := drillRollback(tx, walPath, dir)
	rep := &metrics.Report{
		SyncMaxMs:       st.tl.SyncMaxMs,
		LossRecoveryPct: pct(st.recovered, st.lost),
		P95SegmentMs:    metrics.Percentile(st.segMs, 95),
		RollbackOK:      rollbackOK,
		Frames:          len(st.frames),
		SHA256:          st.sha,
		StageCommits:    tx.Commits,
	}
	if f := fault.First(faults); f != nil {
		rep.HasFirstErr, rep.FirstErrSource, rep.FirstErrOffset = true, f.Source, f.Offset
	}
	return rep, faults, nil
}

func pct(recovered, lost int) float64 {
	if lost == 0 {
		return 100
	}
	return float64(recovered) * 100 / float64(lost)
}

// drillRollback 注入一次提交中断并验证回滚边界。
func drillRollback(tx *journal.Tx, walPath, dir string) bool {
	_, err := tx.Run("drill", "drill-input", func(d string) (string, error) {
		os.WriteFile(filepath.Join(d, "partial.out"), []byte("partial"), 0o644)
		return "", errors.New("simulated commit interruption")
	})
	if err == nil {
		return false
	}
	committed, dangling, rerr := journal.Replay(walPath)
	if rerr != nil || dangling != 0 || len(committed) != 4 {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "out", ".tmp-drill")); !os.IsNotExist(err) {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "render")); err != nil {
		return false
	}
	ok, _ := journal.Verify(walPath, map[string]string{
		"parse": committed[0].Digest, "recover": committed[1].Digest,
		"mix": committed[2].Digest, "render": committed[3].Digest,
	})
	return ok
}

// stageParse 清单解析与分片读取。
func (st *pipeState) stageParse(dir string, faults *[]*fault.Error) (string, error) {
	var total int64
	summary := ""
	for _, cam := range st.layout.Cameras {
		mpd, errs := manifest.ParseDASH(cam.MPDPath)
		*faults = append(*faults, errs...)
		if err := fatalErr(errs); err != nil {
			return "", err
		}
		cm := cameraMedia{id: cam.ID}
		for i := range mpd.Representations {
			r := &mpd.Representations[i]
			switch {
			case r.Codecs == "jpeg" && cm.videoHi == nil:
				cm.videoHi = r
			case r.Codecs == "jpeg":
				cm.videoMid = r
			case r.Codecs == "lpcm":
				cm.audio = r
			}
		}
		if cm.videoHi == nil || cm.videoMid == nil || cm.audio == nil {
			return "", fault.Newf(fault.KindMissingInit, cam.ID, 0, false, "incomplete representations")
		}
		readSegs := func(paths []string, dst *[]*fragment.Segment) error {
			for _, p := range paths {
				start := time.Now()
				seg, errs := fragment.ParseSegment(p)
				st.segMs = append(st.segMs, float64(time.Since(start).Microseconds())/1000)
				*faults = append(*faults, errs...)
				if err := fatalErr(errs); err != nil {
					return err
				}
				if info, err := os.Stat(p); err == nil {
					total += info.Size()
					if total > fragment.MaxTotalBytes {
						return fault.Newf(fault.KindLimit, p, total, false, "total bytes exceed limit")
					}
				}
				*dst = append(*dst, seg)
			}
			return nil
		}
		if err := readSegs(cm.videoHi.MediaPaths, &cm.hiSegs); err != nil {
			return "", err
		}
		if err := readSegs(cm.videoMid.MediaPaths, &cm.midSegs); err != nil {
			return "", err
		}
		if err := readSegs(cm.audio.MediaPaths, &cm.audSegs); err != nil {
			return "", err
		}
		st.cams = append(st.cams, cm)
		summary += fmt.Sprintf("%s hi=%d mid=%d audio=%d\n", cam.ID,
			len(cm.hiSegs), len(cm.midSegs), len(cm.audSegs))
	}
	// HLS 节目轨
	h, errs := manifest.ParseHLS(st.layout.HLS.M3U8Path)
	*faults = append(*faults, errs...)
	if err := fatalErr(errs); err != nil {
		return "", err
	}
	st.hls = h
	for _, s := range h.Segments {
		start := time.Now()
		seg, errs := fragment.ParseSegment(s.Path)
		st.segMs = append(st.segMs, float64(time.Since(start).Microseconds())/1000)
		*faults = append(*faults, errs...)
		if err := fatalErr(errs); err != nil {
			return "", err
		}
		st.hlsSegs = append(st.hlsSegs, seg)
	}
	summary += fmt.Sprintf("hls segs=%d disco=%d\n", len(st.hlsSegs), h.DiscontinuityCount)
	if err := os.WriteFile(filepath.Join(dir, "parse-summary.txt"), []byte(summary), 0o644); err != nil {
		return "", err
	}
	return journal.HashDir(dir)
}

// stageRecover 丢包恢复：样本打包分组、注入丢失、校验恢复、码率故障转移。
func (st *pipeState) stageRecover(dir string, faults *[]*fault.Error, opts options) (string, error) {
	// 读取 hi 档负载，CRC/截断缺失时故障转移到 mid 档
	readVideo := func(cm *cameraMedia) ([][]byte, error) {
		var out [][]byte
		for si, seg := range cm.hiSegs {
			for i := 0; i < fixture.FramesPerSeg; i++ {
				data, ferr := seg.ReadData(i)
				if ferr != nil {
					*faults = append(*faults, ferr)
					if si >= len(cm.midSegs) {
						return nil, ferr
					}
					data, ferr = cm.midSegs[si].ReadData(i) // 故障转移
					if ferr != nil {
						*faults = append(*faults, ferr)
						return nil, ferr
					}
				}
				out = append(out, data)
			}
		}
		return out, nil
	}
	var video, audio [][]byte
	for ci := range st.cams {
		cm := &st.cams[ci]
		v, err := readVideo(cm)
		if err != nil {
			return "", err
		}
		video = append(video, v...)
		for _, seg := range cm.audSegs {
			for i := range seg.Samples {
				data, ferr := seg.ReadData(i)
				if ferr != nil {
					*faults = append(*faults, ferr)
					return "", ferr
				}
				audio = append(audio, data)
			}
		}
	}
	// CRC 损坏夹具：须检出且不得伪报恢复
	cseg, errs := fragment.ParseSegment(st.layout.CorruptPath)
	*faults = append(*faults, errs...)
	if cseg != nil {
		for i := range cseg.Samples {
			if _, ferr := cseg.ReadData(i); ferr != nil {
				*faults = append(*faults, ferr)
			}
		}
	}
	// 打包分组（视频+音频统一进入分组校验链）
	all := append(append([][]byte{}, video...), audio...)
	var pkts []packet.Packet
	for g := 0; g*packet.GroupSize < len(all); g++ {
		end := (g + 1) * packet.GroupSize
		if end > len(all) {
			end = len(all)
		}
		pkts = append(pkts, packet.MakeGroup(g, all[g*packet.GroupSize:end])...)
	}
	// 乱序
	rand.New(rand.NewSource(20261128)).Shuffle(len(pkts), func(i, j int) {
		pkts[i], pkts[j] = pkts[j], pkts[i]
	})
	// 注入丢失：每组至多丢 1 个数据包，保证可恢复
	nGroups := (len(all) + packet.GroupSize - 1) / packet.GroupSize
	dropped := 0
	if opts.injectLoss > nGroups {
		return "", fault.Newf(fault.KindPacketLoss, "inject", int64(opts.injectLoss), false,
			"loss %d exceeds groups %d", opts.injectLoss, nGroups)
	}
	var kept []packet.Packet
	for _, p := range pkts {
		if !p.Parity && dropped < opts.injectLoss && p.Group == dropped*7%nGroups && p.Seq == p.Group%packet.GroupSize {
			dropped++
			continue
		}
		kept = append(kept, p)
	}
	res, errs := packet.Recover(kept)
	*faults = append(*faults, errs...)
	if err := fatalErr(errs); err != nil {
		return "", err
	}
	st.lost, st.recovered = res.Lost, res.Recovered
	st.payloads = res.Payloads[:len(video)]
	st.audio = res.Payloads[len(video):]
	summary := fmt.Sprintf("lost=%d recovered=%d payloads=%d\n", res.Lost, res.Recovered, len(res.Payloads))
	if err := os.WriteFile(filepath.Join(dir, "recover-summary.txt"), []byte(summary), 0o644); err != nil {
		return "", err
	}
	return journal.HashDir(dir)
}
