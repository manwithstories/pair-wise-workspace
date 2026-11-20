package fixture

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"

	"mediarepair/internal/fault"
)

// CorruptKinds 列出全部损坏样例种类。
var CorruptKinds = []string{"truncate", "badlength", "badmarker", "overflow"}

// 每种损坏样例破坏的位置语义：
//   - truncate  截断 mdat 尾部 → 箱超出文件末尾
//   - badlength 篡改 stts 长度 → 箱长度非法
//   - badmarker 时间线 version 改为未知 → 缺少必需标记
//   - overflow  轨道 timescale 改为 0 → 时间基准非法/溢出

// writeCorruptSet 写出全部损坏样例，并返回 path->kind 映射。
func writeCorruptSet(dir string, validMP4 string, validSidecar string) (map[string]string, error) {
	out := make(map[string]string, len(CorruptKinds))
	for _, kind := range CorruptKinds {
		path := filepath.Join(dir, "corrupt_"+kind+".mp4")
		side := filepath.Join(dir, "corrupt_"+kind+".json")
		var err error
		switch kind {
		case "truncate":
			err = writeTruncated(path, validMP4)
			side = validSidecar
		case "badlength":
			err = writeBadLength(path, validMP4)
			side = validSidecar
		case "badmarker":
			err = copyFile(path, validMP4)
			side, err = writeSidecarVariant(dir, validSidecar, kind)
		case "overflow":
			err = copyFile(path, validMP4)
			side, err = writeSidecarVariant(dir, validSidecar, kind)
		}
		if err != nil {
			return nil, err
		}
		out[path] = kind
		out[side] = kind
	}
	return out, nil
}

func copyFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "读取源夹具失败", err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "写损坏样例失败", err)
	}
	return nil
}

// writeTruncated 截掉文件尾部，模拟输入截断。箱结构检查会先在 mdat 头部
// 报告越界，因此这里同时生成一个“箱头完整、样本越界”的变体，用于演示
// 可恢复错误下已验证片段的输出。
func writeTruncated(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "读取源夹具失败", err)
	}
	if len(data) < 64 {
		return fault.New(fault.StageIO, 0, -1, false, "源夹具过小，无法构造截断样例")
	}
	keep := len(data) * 2 / 3
	if err := os.WriteFile(dst, data[:keep], 0o644); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "写截断样例失败", err)
	}
	return nil
}

// writeTailTruncated 截掉 mdat 负载尾部但重写 mdat 长度字段，使箱结构校验
// 通过，样本读取时以可恢复错误报告越界位置，从而演示已验证片段仍可输出。
func writeTailTruncated(dst, src string) (int, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return 0, fault.Wrap(fault.StageIO, 0, -1, false, "读取源夹具失败", err)
	}
	idx := indexOfBox(data, "mdat")
	if idx < 4 {
		return 0, fault.New(fault.StageIO, 0, -1, false, "源夹具缺少 mdat 箱")
	}
	boxStart := idx - 4
	origSize := int(binary.BigEndian.Uint32(data[boxStart:]))
	if origSize < 16 || boxStart+origSize > len(data) {
		return 0, fault.New(fault.StageIO, 0, int64(boxStart), false, "mdat 长度异常，无法构造样例")
	}
	keep := boxStart + origSize*97/100
	if keep <= idx+4 {
		return 0, fault.New(fault.StageIO, 0, int64(boxStart), false, "截断位置早于 mdat 负载")
	}
	out := append([]byte(nil), data[:keep]...)
	binary.BigEndian.PutUint32(out[boxStart:], uint32(keep-boxStart))
	if err := os.WriteFile(dst, out, 0o644); err != nil {
		return 0, fault.Wrap(fault.StageIO, 0, -1, false, "写截断样例失败", err)
	}
	return keep, nil
}

// indexOfBox 返回箱类型字符串首次出现的偏移（即 type 字段起始）。
func indexOfBox(data []byte, boxType string) int {
	want := []byte(boxType)
	for i := 0; i+4 <= len(data); i++ {
		if string(data[i:i+4]) == string(want) {
			return i
		}
	}
	return -1
}

// writeBadLength 找到第一个 stts 箱，把其 size 字段改成小于头部的非法值。
func writeBadLength(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "读取源夹具失败", err)
	}
	idx := indexOfBox(data, "stts")
	if idx < 4 {
		return fault.New(fault.StageIO, 0, -1, false, "源夹具缺少 stts 箱")
	}
	binary.BigEndian.PutUint32(data[idx-4:], 3) // size=3 < 最小 8
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "写非法长度样例失败", err)
	}
	return nil
}

// writeSidecarVariant 读取源 sidecar，按 kind 篡改必需标记或时间基准。
func writeSidecarVariant(dir, src, kind string) (string, error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", fault.Wrap(fault.StageIO, 0, -1, false, "读取源时间线失败", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fault.Wrap(fault.StageIO, 0, -1, false, "解析源时间线失败", err)
	}
	switch kind {
	case "badmarker":
		doc["version"] = "mediarepair-timeline/v9"
	case "overflow":
		tracks, _ := doc["tracks"].([]any)
		if len(tracks) > 0 {
			if t0, ok := tracks[0].(map[string]any); ok {
				t0["timescale"] = 0
			}
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fault.Wrap(fault.StageIO, 0, -1, false, "编码损坏时间线失败", err)
	}
	out = append(out, '\n')
	path := filepath.Join(dir, "corrupt_"+kind+".json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", fault.Wrap(fault.StageIO, 0, -1, false, "写损坏时间线失败", err)
	}
	return path, nil
}
