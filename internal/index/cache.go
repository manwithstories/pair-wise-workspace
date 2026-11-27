package index

import (
	"encoding/json"
	"os"

	"medpipe/internal/clock"
)

// WriteCache 把索引序列化到缓存文件。
func (idx *Index) WriteCache(path string) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return clock.WrapError(clock.CodeCheckpoint, -1, err, "索引缓存序列化失败")
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return clock.WrapError(clock.CodeCheckpoint, -1, err, "索引缓存写入失败")
	}
	return nil
}

// LoadOrRebuild 命中缓存且每个块的大小/摘要仍一致时复用，否则重建。
// 返回 rebuilt=true 表示检测到输入变化并已作废旧缓存。
func LoadOrRebuild(base, cachePath string, maxBlock int64) (idx *Index, rebuilt bool, err error) {
	if cached, perr := loadCache(cachePath); perr == nil {
		if cached.fresh(maxBlock) == nil {
			return cached, false, nil
		}
		rebuilt = true
	}
	fresh, err := ScanDir(base, maxBlock)
	if err != nil {
		return nil, rebuilt, err
	}
	if err := fresh.WriteCache(cachePath); err != nil {
		return nil, rebuilt, err
	}
	return fresh, rebuilt, nil
}

func loadCache(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, clock.WrapError(clock.CodeCheckpoint, -1, err, "索引缓存损坏")
	}
	return &idx, nil
}

// fresh 重新摘要缓存中的每个块，任一变化即返回 CodeStaleIndex。
func (idx *Index) fresh(maxBlock int64) error {
	if len(idx.Blocks) == 0 {
		return clock.NewError(clock.CodeStaleIndex, -1, "空缓存")
	}
	for _, want := range idx.Blocks {
		got, err := ScanBlock(want.ID, want.Path, maxBlock)
		if err != nil {
			return clock.WrapError(clock.CodeStaleIndex, want.ID, err, "块不可重读")
		}
		if got.SHA256 != want.SHA256 || got.Size != want.Size {
			return clock.NewError(clock.CodeStaleIndex, want.ID, "块内容已变化，作废索引")
		}
	}
	return nil
}
