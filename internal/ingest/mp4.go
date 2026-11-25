package ingest

import (
	"encoding/binary"
	"errors"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/fault"
)

// MaxMP4BoxSize 是单次 box 的安全上限，避免恶意 size 触发超大分配。
const MaxMP4BoxSize int64 = 64 << 20

// MP4SampleNoFunc 将 stbl 样本序号映射到全局包序号。
type MP4SampleNoFunc func(sampleIndexZeroBased int) int

// ReadMP4 用 go-mp4 读取样本表，并结合 stco/stsz 计算每样本源偏移。
func ReadMP4(path, trackID string, noForSample MP4SampleNoFunc) ([]Packet, []fault.Finding, *fault.Error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fault.Wrap(fault.KindStructure, "无法打开 MP4", &fault.Location{Source: path}, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, nil, fault.Wrap(fault.KindStructure, "无法统计 MP4", &fault.Location{Source: path}, err)
	}
	fileSize := st.Size()

	info, err := gomp4.Probe(f)
	if err != nil {
		fe, _ := LocateStructuralDamage(path)
		if fe != nil && fe.Location != nil {
			return nil, nil, fault.Wrap(fault.KindStructure, "MP4 样本表解析失败且已定位结构损坏", fe.Location, errors.Join(err, fe))
		}
		return nil, nil, fault.Wrap(fault.KindStructure, "MP4 样本表解析失败", &fault.Location{Source: path}, err)
	}
	if len(info.Tracks) == 0 {
		return nil, nil, fault.New(fault.KindUnsupported, "MP4 不含轨道", &fault.Location{Source: path})
	}

	var packets []Packet
	var findings []fault.Finding
	for ti, track := range info.Tracks {
		id := trackID
		if len(info.Tracks) > 1 {
			id = trackID + "-" + gomp4.Uint32ToBoxType(track.TrackID).String()
		}
		if track.Codec != gomp4.CodecMP4A {
			return nil, findings, fault.New(fault.KindUnsupported, "拒绝未知或非 AAC 轨道格式", &fault.Location{
				Source: path, Track: id, Message: "codec=" + codecName(track.Codec),
			})
		}
		if track.Timescale == 0 {
			return nil, findings, fault.New(fault.KindFormat, "MP4 timescale 为 0", &fault.Location{Source: path, Track: id})
		}
		if len(track.Samples) == 0 {
			findings = append(findings, fault.Finding{Code: "empty_track", Severity: "warning", Message: "轨道没有样本", Location: &fault.Location{Source: path, Track: id}})
			continue
		}

		sampleIndex := 0
		var start clock.Ticks
		for ci, chunk := range track.Chunks {
			offset := int64(chunk.DataOffset)
			if offset < 0 || offset > fileSize {
				return nil, findings, fault.New(fault.KindBounds, "chunk 偏移越界", &fault.Location{
					Source: path, Track: id, Offset: offset, End: fileSize, Sample: sampleIndex + 1,
				})
			}
			for j := uint32(0); j < chunk.SamplesPerChunk && sampleIndex < len(track.Samples); j++ {
				s := track.Samples[sampleIndex]
				sampleStart := offset
				sampleEnd := sampleStart + int64(s.Size)
				if s.Size == 0 || sampleEnd > fileSize {
					return nil, findings, fault.New(fault.KindBounds, "MP4 sample 偏移或长度越界", &fault.Location{
						Source: path, Track: id, Offset: sampleStart, Length: int64(s.Size), End: fileSize, Sample: sampleIndex + 1,
					})
				}
				buf := make([]byte, s.Size)
				if _, err := f.ReadAt(buf, sampleStart); err != nil {
					return nil, findings, fault.Wrap(fault.KindStructure, "读取 MP4 sample 数据失败", &fault.Location{
						Source: path, Track: id, Offset: sampleStart, Length: int64(s.Size), Sample: sampleIndex + 1,
					}, err)
				}
				dur := clock.AACDuration(s.TimeDelta, track.Timescale)
				no := sampleIndex + 1
				if noForSample != nil {
					no = noForSample(sampleIndex)
				}
				packets = append(packets, Packet{
					No:           no,
					TrackID:      id,
					Codec:        "aac",
					Seq:          uint16(sampleIndex + 1),
					ExtSeq:       int64(sampleIndex + 1),
					Timestamp:    uint32(start),
					ExtTimestamp: int64(start),
					Start:        start,
					Duration:     dur,
					Hash:         HashBytes(buf),
					Source: SourceRef{
						Path: path, Container: "mp4", Offset: sampleStart, Length: int(s.Size),
						Box: "mdat/stsz/stts/stco", SampleIndex: sampleIndex + 1,
					},
				})
				start += dur
				offset = sampleEnd
				sampleIndex++
			}
			_ = ci
		}
		if sampleIndex != len(track.Samples) {
			return nil, findings, fault.New(fault.KindStructure, "stsc 与 stsz/stts 样本数不一致", &fault.Location{Source: path, Track: id})
		}
		_ = ti
	}
	return packets, findings, nil
}

// BoxDamage 描述扫描到的 box 头和截断/超长问题。
type BoxDamage struct {
	Location fault.Location
	Kind     fault.Kind
}

// LocateStructuralDamage 扫描 MP4 box 树，定位第一个截断、非法或超长 box。
func LocateStructuralDamage(path string) (*fault.Error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return scanBoxes(f, 0, st.Size(), path, "")
}

var containerBoxes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "dinf": true, "edts": true, "mvex": true,
}

func scanBoxes(r io.ReadSeeker, start, end int64, path, parent string) (*fault.Error, error) {
	pos := start
	header := make([]byte, 16)
	for pos+8 <= end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, header[:8]); err != nil {
			loc := &fault.Location{Source: path, Offset: pos, End: end, Box: parent}
			return fault.New(fault.KindStructure, "box 头截断", loc), err
		}
		size := int64(binary.BigEndian.Uint32(header[0:4]))
		boxType := string(header[4:8])
		headerSize := int64(8)
		if size == 1 {
			if _, err := io.ReadFull(r, header[8:16]); err != nil {
				loc := &fault.Location{Source: path, Offset: pos, Box: boxType, End: end}
				return fault.New(fault.KindStructure, "64 位 box size 截断", loc), err
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
			headerSize = 16
		} else if size == 0 {
			size = end - pos
		}
		loc := &fault.Location{Source: path, Offset: pos, Length: size, Box: parent + "/" + boxType, End: end}
		if size < headerSize {
			return fault.New(fault.KindFormat, "box size 小于头部", loc), nil
		}
		if size > MaxMP4BoxSize {
			return fault.New(fault.KindBounds, "拒绝超长 MP4 box", loc), nil
		}
		boxEnd := pos + size
		if boxEnd > end {
			return fault.New(fault.KindStructure, "MP4 box 被截断", loc), nil
		}
		if containerBoxes[boxType] {
			if fe, err := scanBoxes(r, pos+headerSize, boxEnd, path, parent+"/"+boxType); fe != nil {
				return fe, err
			}
		}
		pos = boxEnd
	}
	return nil, nil
}

func codecName(c gomp4.Codec) string {
	switch c {
	case gomp4.CodecMP4A:
		return "mp4a/aac"
	default:
		return "unknown"
	}
}
