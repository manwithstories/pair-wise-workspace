// Package index 扫描 PCM/WAV 多声道块，解析头与块边界并建立带摘要的索引。
//
// 扫描全程流式读取，不把整块音频载入内存；输入内容变化时通过大小与
// SHA-256 摘要检测出来并作废缓存、重建索引。
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"medpipe/internal/clock"
)

// DefaultMaxBlock 单块数据段默认上限（64 MiB），超限按 CodeOversize 拒绝。
const DefaultMaxBlock int64 = 64 << 20

// BlockIndex 是单个 WAV 块的扫描结果。
type BlockIndex struct {
	ID            int64  `json:"id"`
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	DataOffset    int64  `json:"data_offset"`
	DataBytes     int64  `json:"data_bytes"` // 磁盘上存在且为整帧的数据字节
	DeclaredData  int64  `json:"declared_data"`
	SampleRate    int64  `json:"sample_rate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
	BlockAlign    int    `json:"block_align"`
	Frames        int64  `json:"frames"`         // 完整采样帧数
	MissingFrames int64  `json:"missing_frames"` // 截断丢失的采样帧数
	Truncated     bool   `json:"truncated"`
	SHA256        string `json:"sha256"` // 整个文件的摘要，用于变更检测
}

// Index 是一个输入目录的全部块索引。
type Index struct {
	Base    string       `json:"base"`
	Blocks  []BlockIndex `json:"blocks"`
	MaxBlock int64       `json:"max_block"`
}

// SafeJoin 在 base 内解析 name，拒绝绝对路径与任何 ".." 越界段。
func SafeJoin(base, name string) (string, error) {
	if filepath.IsAbs(name) || filepath.IsAbs(base) {
		// 允许 base 绝对，但 name 永远只能是相对名
		if filepath.IsAbs(name) {
			return "", clock.NewError(clock.CodePathRejected, -1, "拒绝绝对路径: %s", name)
		}
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", clock.NewError(clock.CodePathRejected, -1, "路径越界: %s", name)
	}
	full := filepath.Join(base, clean)
	rel, err := filepath.Rel(filepath.Clean(base), full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", clock.NewError(clock.CodePathRejected, -1, "路径越界: %s", name)
	}
	return full, nil
}

// ScanBlock 流式扫描单个 WAV：校验头、定位数据段、计算帧数与摘要。
func ScanBlock(id int64, path string, maxBlock int64) (BlockIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return BlockIndex{}, clock.WrapError(clock.CodeInvalidHeader, id, err, "打开块失败")
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return BlockIndex{}, clock.WrapError(clock.CodeInvalidHeader, id, err, "stat 块失败")
	}
	if fi.Size() <= 12 {
		return BlockIndex{}, clock.NewError(clock.CodeInvalidHeader, id, "文件过小，不可能是合法 WAV")
	}

	h := sha256.New()
	r := io.TeeReader(f, h)
	le := binary.LittleEndian
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return BlockIndex{}, clock.NewError(clock.CodeInvalidHeader, id, "RIFF 头不完整")
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return BlockIndex{}, clock.NewError(clock.CodeInvalidHeader, id, "不是 RIFF/WAVE 文件")
	}

	bi := BlockIndex{ID: id, Path: path, Size: fi.Size()}
	chunkHdr := make([]byte, 8)
	dataSeen := false
	for {
		n, err := io.ReadFull(r, chunkHdr)
		if errors.Is(err, io.EOF) || (errors.Is(err, io.ErrUnexpectedEOF) && n == 0) {
			break
		}
		if err != nil {
			return BlockIndex{}, clock.NewError(clock.CodeInvalidHeader, id, "块头读取失败")
		}
		chunkID := string(chunkHdr[0:4])
		chunkSize := int64(le.Uint32(chunkHdr[4:8]))
		switch chunkID {
		case "fmt ":
			if err := readFmt(r, chunkSize, &bi); err != nil {
				return BlockIndex{}, err
			}
		case "data":
			if err := readData(r, f, chunkSize, maxBlock, &bi); err != nil {
				return BlockIndex{}, err
			}
			dataSeen = true
		default:
			// 跳过 LIST 等非关键块；奇数长度带 1 字节填充
			skip := chunkSize + chunkSize&1
			if _, err := io.CopyN(io.Discard, r, skip); err != nil {
				return BlockIndex{}, clock.NewError(clock.CodeInvalidHeader, id, "块 %q 被截断", chunkID)
			}
		}
	}
	if !dataSeen || bi.BlockAlign == 0 {
		return BlockIndex{}, clock.NewError(clock.CodeInvalidHeader, id, "缺少 fmt/data 块")
	}
	// 摘要目前只覆盖到已读到的位置；补齐文件剩余字节（截断文件本就到此为止）
	if _, err := io.Copy(h, f); err != nil {
		return BlockIndex{}, clock.WrapError(clock.CodeInvalidHeader, id, err, "摘要计算失败")
	}
	bi.SHA256 = hex.EncodeToString(h.Sum(nil))
	return bi, nil
}

func readFmt(r io.Reader, size int64, bi *BlockIndex) error {
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "fmt 块不完整")
	}
	if len(buf) < 16 {
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "fmt 块过短")
	}
	le := binary.LittleEndian
	if tag := le.Uint16(buf[0:2]); tag != 1 {
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "仅支持 PCM(format=1), 得 %d", tag)
	}
	bi.Channels = int(le.Uint16(buf[2:4]))
	bi.SampleRate = int64(le.Uint32(buf[4:8]))
	byteRate := int64(le.Uint32(buf[8:12]))
	bi.BlockAlign = int(le.Uint16(buf[12:14]))
	bi.BitsPerSample = int(le.Uint16(buf[14:16]))
	if bi.SampleRate <= 0 || bi.Channels <= 0 || bi.Channels > 64 {
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "非法采样率/声道数")
	}
	switch bi.BitsPerSample {
	case 8, 16, 24, 32:
	default:
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "不支持位深 %d", bi.BitsPerSample)
	}
	if want := bi.Channels * bi.BitsPerSample / 8; bi.BlockAlign != want {
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "blockAlign=%d 与声道/位深不符(应 %d)", bi.BlockAlign, want)
	}
	if byteRate != bi.SampleRate*int64(bi.BlockAlign) {
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "byteRate 与采样率不一致")
	}
	return nil
}

func readData(r io.Reader, f *os.File, declared, maxBlock int64, bi *BlockIndex) error {
	if declared > maxBlock {
		return clock.NewError(clock.CodeOversize, bi.ID, "data 段 %d 超上限 %d", declared, maxBlock)
	}
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return clock.WrapError(clock.CodeInvalidHeader, bi.ID, err, "定位数据段失败")
	}
	bi.DataOffset = offset
	// 实际可读字节：文件可能在声明长度之前结束（截断）
	avail, err := io.CopyN(io.Discard, r, declared)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return clock.WrapError(clock.CodeInvalidHeader, bi.ID, err, "数据段读取失败")
	}
	bi.DeclaredData = declared
	align := int64(bi.BlockAlign)
	if avail%align != 0 {
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "半个采样帧: 数据 %d 字节不能被 %d 整除", avail, align)
	}
	bi.DataBytes = avail
	bi.Frames = avail / align
	if avail < declared {
		bi.Truncated = true
		bi.MissingFrames = (declared - avail + align - 1) / align
	}
	if declared%align != 0 {
		return clock.NewError(clock.CodeInvalidHeader, bi.ID, "声明数据长度含半个采样帧")
	}
	return nil
}

// blockName 约定块文件名 block_%06d.wav。
func blockName(info os.DirEntry) (int64, bool) {
	name := info.Name()
	if !strings.HasPrefix(name, "block_") || !strings.HasSuffix(name, ".wav") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, "block_"), ".wav")
	if len(mid) != 6 {
		return 0, false
	}
	id, err := strconv.ParseInt(mid, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// ScanDir 扫描目录内全部 block_*.wav，按块号升序返回索引。
func ScanDir(base string, maxBlock int64) (*Index, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, clock.WrapError(clock.CodeInvalidParam, -1, err, "读取输入目录失败")
	}
	idx := &Index{Base: base, MaxBlock: maxBlock}
	seen := map[int64]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := blockName(e)
		if !ok {
			if strings.HasSuffix(e.Name(), ".wav") {
				return nil, clock.NewError(clock.CodeInvalidParam, -1, "无法解析块文件名: %s", e.Name())
			}
			continue
		}
		if seen[id] {
			return nil, clock.NewError(clock.CodeInvalidParam, id, "块号重复")
		}
		seen[id] = true
		full, err := SafeJoin(base, e.Name())
		if err != nil {
			return nil, err
		}
		bi, err := ScanBlock(id, full, maxBlock)
		if err != nil {
			return nil, err
		}
		idx.Blocks = append(idx.Blocks, bi)
	}
	sort.Slice(idx.Blocks, func(i, j int) bool { return idx.Blocks[i].ID < idx.Blocks[j].ID })
	return idx, nil
}

// FormatRate 仅用于终端日志的一致格式。
func FormatRate(r int64) string { return strconv.FormatInt(r, 10) + "Hz" }
