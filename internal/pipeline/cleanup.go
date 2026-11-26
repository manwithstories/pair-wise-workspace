// cleanup.go 负责在续跑开始时清理未被检查点引用的孤儿片段文件。
//
// 模拟中断可能在“片段文件已落盘、检查点尚未提交”之间发生，留下孤儿。
// 这些文件不属于任何已原子提交的记录，续跑时安全删除，保证 segments/
// 目录与清单严格一致。
package pipeline

import (
	"os"
	"path/filepath"

	"archivemedia/internal/journal"
	"archivemedia/internal/model"
)

// cleanupOrphans 删除 segments/ 下未被已提交记录引用的 .arc 文件。
func (r *Runner) cleanupOrphans() error {
	committed := r.jrnl.Committed()
	referenced := map[string]bool{}
	for _, rec := range committed {
		referenced[rec.FilePath] = true
	}
	segDir, err := journal.SafePath(r.cfg.Root, "segments")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(segDir)
	if err != nil {
		return model.Wrap(model.KindCheckpoint, err, "pipeline: read segments dir")
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".arc" {
			continue
		}
		rel := filepath.Join("segments", e.Name())
		if referenced[rel] {
			continue
		}
		abs, err := journal.SafePath(r.cfg.Root, rel)
		if err != nil {
			return err
		}
		if err := os.Remove(abs); err != nil {
			return model.Wrap(model.KindCheckpoint, err, "pipeline: remove orphan %s", rel)
		}
	}
	return nil
}
