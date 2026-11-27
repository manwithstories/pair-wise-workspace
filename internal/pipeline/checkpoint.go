package pipeline

import (
	"os"

	"relay/internal/checkpoint"
	"relay/internal/media"
	"relay/internal/remux"
	"relay/internal/timeline"
)

// initFingerprints 为全新运行建立输入/映射指纹基线与检查点骨架。
func (e *Engine) initFingerprints(format remux.Format) error {
	inSHA, inSize, err := checkpoint.FileSHA256(e.opts.InputPath)
	if err != nil {
		return media.NewFault(media.StageConfig, 0, 0, 0, "fingerprint input: %v", err)
	}
	mapSHA, _, err := checkpoint.FileSHA256(e.opts.MapPath)
	if err != nil {
		return media.NewFault(media.StageConfig, 0, 0, 0, "fingerprint map: %v", err)
	}
	e.ckpt = &checkpoint.Data{
		InputPath: e.opts.InputPath, InputSize: inSize, InputSHA256: inSHA,
		MapPath: e.opts.MapPath, MapSHA256: mapSHA,
		OutputPath: e.opts.OutputPath, OutputFormat: string(format),
		Watermarks: map[uint32]int64{},
		GraceNS:    int64(media.GraceWindow),
	}
	return nil
}

// checkpoint 在安全点（消费者已排空投递帧）原子提交恢复点。
func (e *Engine) checkpoint(inputOffset int64) error {
	e.mu.Lock()
	e.ckpt.InputOffset = inputOffset
	e.ckpt.FramesConsumed = e.stats.Consumed
	e.ckpt.Duplicates = e.stats.Duplicates
	e.ckpt.Quarantined = e.stats.Quarantined
	e.ckpt.FramesReleased = e.stats.Released
	dedupSnap := checkpoint.EncodeDedup(e.dedup.Snapshot())
	wm, recs := checkpoint.FromTimeline(e.tl.ExportState(), e.tl.Quarantine())
	e.mu.Unlock()

	e.ckpt.CommittedOutputLen = e.writer.WrittenBytes()
	e.ckpt.Dedup = dedupSnap
	e.ckpt.Watermarks = wm
	e.ckpt.Quarantine = recs
	e.ckpt.Entries = e.writer.Entries()
	return checkpoint.Save(e.opts.CheckpointPath, e.ckpt)
}

// restore 校验检查点、截断输出并重建全部内存状态。
func (e *Engine) restore(format remux.Format) error {
	d, err := checkpoint.Load(e.opts.CheckpointPath)
	if err != nil {
		return err
	}
	inSHA, inSize, err := checkpoint.FileSHA256(e.opts.InputPath)
	if err != nil {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "re-fingerprint input: %v", err)
	}
	mapSHA, _, err := checkpoint.FileSHA256(e.opts.MapPath)
	if err != nil {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "re-fingerprint map: %v", err)
	}
	if err := checkpoint.VerifyFingerprints(d, inSHA, mapSHA, inSize); err != nil {
		return err
	}
	if d.OutputPath != e.opts.OutputPath || d.OutputFormat != string(format) {
		return media.NewFault(media.StageCheckpoint, 0, 0, 0, "output target changed since checkpoint")
	}
	// 按已提交长度截断未提交尾部，再续写。
	if err := checkpoint.TruncateOutput(e.opts.OutputPath, d.CommittedOutputLen); err != nil {
		return err
	}
	w, err := remux.ResumeWriter(e.opts.OutputPath, format, d.CommittedOutputLen, d.Entries)
	if err != nil {
		return err
	}
	e.writer = w
	e.ckpt = d

	// 重建去重账本。
	ledger, err := checkpoint.DecodeDedup(d.Dedup)
	if err != nil {
		return err
	}
	e.dedup.Restore(ledger)

	// 重建时间线水位（maxEvent = watermark + grace）。
	st := timeline.State{GraceNS: d.GraceNS, MaxEvent: map[uint32]int64{}}
	for track, wm := range d.Watermarks {
		st.MaxEvent[track] = wm + d.GraceNS
	}
	e.tl.RestoreState(st)

	// 恢复计数器，使后续统计连续。
	e.stats.Consumed = d.FramesConsumed
	e.stats.Released = d.FramesReleased
	e.stats.Duplicates = d.Duplicates
	e.stats.Quarantined = d.Quarantined
	return nil
}

// CheckpointExists 报告恢复点文件是否存在（CLI resume 前置检查）。
func CheckpointExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
