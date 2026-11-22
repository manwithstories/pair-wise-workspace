// Package fanout 把统一时间轴扇出为多个码率的 fMP4 代理片段。
package fanout

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"mediatool/internal/clock"
	"mediatool/internal/repair"
)

// Representation 描述一个输出层。
type Representation struct {
	ID          int
	Bitrate     int         // bps
	KeyInterval clock.Ticks // 关键帧间隔
}

// SegmentFile 是一个已写出的片段。
type SegmentFile struct {
	RepID, SegID int
	Path         string
	Start, End   clock.Ticks
	FirstPTS     clock.Ticks // 层内实际首帧（丢包后可能晚于 Start）
	LastPTS      clock.Ticks
	SHA256       string
	Complete     bool
}

// KeyEntry 是关键帧随机访问索引的一条记录。
type KeyEntry struct {
	RepID, SegID int
	Path         string
	Time         clock.Ticks
	Offset       int64 // 文件内绝对偏移
	Size         uint32
}

// Encoder 驱动多码率扇出。LossRate 模拟各层独立丢包（0 表示不丢）。
type Encoder struct {
	Dir      string
	Seed     int64
	LossRate float64
}

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

func fullbox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(head, payload...))
}

// EncodeSegment 把一个片段的帧写成 fMP4 文件，返回片段记录与关键帧索引项。
func (e *Encoder) EncodeSegment(rep Representation, tl repair.Timeline, seg repair.Segment) (SegmentFile, []KeyEntry, error) {
	sf := SegmentFile{RepID: rep.ID, SegID: seg.ID, Start: seg.Start, End: seg.End, Complete: seg.Complete}
	rng := rand.New(rand.NewSource(e.Seed*7919 + int64(rep.ID)*104729 + int64(seg.ID)))
	var frames []repair.Frame
	for _, f := range tl.Frames {
		if f.PTS >= seg.Start && f.PTS < seg.End {
			frames = append(frames, f)
		}
	}
	if len(frames) == 0 {
		sf.Complete = false
		return sf, nil, nil
	}
	// 采样数据：确定性伪随机，大小随码率层缩放。
	type sample struct {
		data []byte
		key  bool
		pts  clock.Ticks
	}
	var samples []sample
	frameDelta := clock.Ticks(3600)
	for i, f := range frames {
		key := i == 0 || (f.PTS-tl.Origin)/rep.KeyInterval != (f.PTS-frameDelta-tl.Origin)/rep.KeyInterval
		if !key && e.LossRate > 0 && rng.Float64() < e.LossRate {
			continue // 层内独立丢包：仅丢非关键帧
		}
		h := sha256.Sum256([]byte(fmt.Sprintf("sample/%d/%d/%d/%d", e.Seed, rep.ID, seg.ID, f.PTS)))
		data := make([]byte, 32*(rep.ID+1))
		for off := range data {
			data[off] = h[off%32]
		}
		samples = append(samples, sample{data: data, key: key, pts: f.PTS})
	}
	if len(samples) == 0 {
		sf.Complete = false
		return sf, nil, nil
	}
	sf.FirstPTS, sf.LastPTS = samples[0].pts, samples[len(samples)-1].pts

	// moof：mfhd + traf(tfhd, tfdt, trun)
	mfhd := fullbox("mfhd", 0, 0, binary.BigEndian.AppendUint32(nil, uint32(seg.ID+1)))
	tfhd := fullbox("tfhd", 0, 0x020000, binary.BigEndian.AppendUint32(nil, uint32(rep.ID+1)))
	tfdt := fullbox("tfdt", 1, 0, binary.BigEndian.AppendUint64(nil, uint64(seg.Start)))
	trunHead := make([]byte, 8)
	binary.BigEndian.PutUint32(trunHead, uint32(len(samples)))
	var trunEntries []byte
	for _, s := range samples {
		var flags uint32 = 0x01010000 // 非关键帧
		if s.key {
			flags = 0x02000000
		}
		trunEntries = binary.BigEndian.AppendUint32(trunEntries, uint32(frameDelta))
		trunEntries = binary.BigEndian.AppendUint32(trunEntries, uint32(len(s.data)))
		trunEntries = binary.BigEndian.AppendUint32(trunEntries, flags)
	}
	trun := fullbox("trun", 0, 0x000701, append(trunHead, trunEntries...))
	traf := box("traf", append(append(tfhd, tfdt...), trun...))
	moof := box("moof", append(mfhd, traf...))

	var mdatPayload []byte
	var keys []KeyEntry
	ftyp := box("ftyp", []byte("isom\x00\x00\x00\x00isomiso6"))
	mdatDataStart := int64(len(ftyp) + len(moof) + 8)
	for i, s := range samples {
		if s.key {
			keys = append(keys, KeyEntry{
				RepID: rep.ID, SegID: seg.ID, Time: s.pts,
				Offset: mdatDataStart + int64(len(mdatPayload)), Size: uint32(len(s.data)),
			})
		}
		mdatPayload = append(mdatPayload, s.data...)
		_ = i
	}
	// trun data_offset 相对 moof 起点，需要回填。
	dataOffset := int32(len(moof) + 8)
	file := append(append(ftyp, moof...), box("mdat", mdatPayload)...)
	// data_offset 字段位置：moof 内 trun 起点 + trun 头(8) + version/flags(4) + sample_count(4)
	trunPosInMoof := 8 + len(mfhd) + 8 + len(tfhd) + len(tfdt)
	trunDataOffsetPos := len(ftyp) + trunPosInMoof + 16
	binary.BigEndian.PutUint32(file[trunDataOffsetPos:], uint32(dataOffset))

	name := fmt.Sprintf("rep%d_seg%04d.m4s", rep.ID, seg.ID)
	if err := os.MkdirAll(e.Dir, 0o755); err != nil {
		return sf, nil, err
	}
	path := filepath.Join(e.Dir, name)
	if err := os.WriteFile(path, file, 0o644); err != nil {
		return sf, nil, err
	}
	sum := sha256.Sum256(file)
	sf.Path = path
	sf.SHA256 = fmt.Sprintf("%x", sum)
	for i := range keys {
		keys[i].Path = path
	}
	return sf, keys, nil
}

// Encode 对全部层与片段执行扇出。
func (e *Encoder) Encode(reps []Representation, tl repair.Timeline) ([]SegmentFile, []KeyEntry, error) {
	var files []SegmentFile
	var keys []KeyEntry
	for _, rep := range reps {
		for _, seg := range tl.Segments {
			sf, ks, err := e.EncodeSegment(rep, tl, seg)
			if err != nil {
				return nil, nil, err
			}
			files = append(files, sf)
			keys = append(keys, ks...)
		}
	}
	return files, keys, nil
}
