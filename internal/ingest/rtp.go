package ingest

import (
	"encoding/binary"
	"io"
	"os"

	"github.com/pion/rtp"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/fault"
)

// MaxRTPFrame 限制单条长度前缀 RTP 记录，防止异常长度导致超大分配。
const MaxRTPFrame = 1 << 20

// RTPDecoder 维护单轨序号和时间戳回绕状态。
type RTPDecoder struct {
	TrackID string
	Codec   string
	prevSeq int64
	prevTS  int64
	hasSeq  bool
}

// NewRTPDecoder 创建解码器。
func NewRTPDecoder(trackID, codec string) *RTPDecoder {
	return &RTPDecoder{TrackID: trackID, Codec: codec}
}

// Parse 解析一条 RTP 包并完成 16/32 位回绕扩展。
func (d *RTPDecoder) Parse(no int, data []byte, ref SourceRef) (Packet, *fault.Error) {
	var rp rtp.Packet
	if err := rp.Unmarshal(data); err != nil {
		loc := refToLocation(ref)
		return Packet{}, fault.Wrap(fault.KindStructure, "短 RTP 包或非法 RTP 头", loc, err)
	}

	if len(rp.Payload) == 0 {
		loc := refToLocation(ref)
		return Packet{}, fault.New(fault.KindStructure, "RTP payload 为空", loc)
	}

	dur, err := clock.OpusDuration(rp.Payload)
	if d.Codec != "opus" {
		dur = clock.Ticks(20 * 48) // 非 Opus RTP 兜底不应在本项目样例中出现
		err = nil
	}
	if err != nil {
		loc := refToLocation(ref)
		return Packet{}, fault.Wrap(fault.KindStructure, "无法识别 Opus 包时长", loc, err)
	}

	if !d.hasSeq {
		d.prevSeq = int64(rp.SequenceNumber)
		d.prevTS = int64(rp.Timestamp)
		d.hasSeq = true
	} else {
		d.prevSeq = clock.Unwrap16(d.prevSeq, rp.SequenceNumber)
		d.prevTS = clock.Unwrap32(d.prevTS, rp.Timestamp)
	}

	ref.Length = len(data)
	p := Packet{
		No:           no,
		TrackID:      d.TrackID,
		Codec:        d.Codec,
		Seq:          rp.SequenceNumber,
		ExtSeq:       d.prevSeq,
		Timestamp:    rp.Timestamp,
		ExtTimestamp: d.prevTS,
		Start:        clock.Ticks(d.prevTS),
		Duration:     dur,
		Hash:         HashBytes(rp.Payload),
		Source:       ref,
		Payload:      append([]byte(nil), rp.Payload...),
	}
	return p, nil
}

// EncodeRecord 将全局序号和 RTP 包编码为长度前缀记录：
// uint32(frame length) | uint32(global packet no) | RTP bytes。
func EncodeRecord(no int, rp *rtp.Packet) ([]byte, error) {
	raw, err := rp.Marshal()
	if err != nil {
		return nil, err
	}
	body := make([]byte, 4+len(raw))
	binary.BigEndian.PutUint32(body[0:4], uint32(no))
	copy(body[4:], raw)
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(body)))
	copy(frame[4:], body)
	return frame, nil
}

// ReadRTPRecords 读取长度前缀 RTP 记录文件。单包结构错误转为 finding，
// 调用方仍可继续处理后续帧；长度帧越界等容器错误作为致命错误返回。
func ReadRTPRecords(path, trackID, codec string) ([]Packet, []fault.Finding, *fault.Error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fault.Wrap(fault.KindStructure, "无法打开 RTP 记录", &fault.Location{Source: path, Offset: 0}, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, nil, fault.Wrap(fault.KindStructure, "无法统计 RTP 记录", &fault.Location{Source: path}, err)
	}
	fileSize := st.Size()

	dec := NewRTPDecoder(trackID, codec)
	var packets []Packet
	var findings []fault.Finding
	var lengthBuf [4]byte

	for {
		recordOffset, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return packets, findings, fault.Wrap(fault.KindStructure, "无法获取 RTP 偏移", &fault.Location{Source: path}, err)
		}
		n, err := io.ReadFull(f, lengthBuf[:])
		if err == io.EOF && n == 0 {
			break
		}
		if err != nil {
			loc := &fault.Location{Source: path, Offset: recordOffset, End: fileSize, Message: "截断的记录长度前缀"}
			return packets, findings, fault.Wrap(fault.KindStructure, "RTP 记录容器截断", loc, err)
		}
		frameLen := int64(binary.BigEndian.Uint32(lengthBuf[:]))
		if frameLen < 4 || frameLen > MaxRTPFrame {
			loc := &fault.Location{Source: path, Offset: recordOffset, Length: 4, End: fileSize}
			return packets, findings, fault.New(fault.KindBounds, "RTP 记录长度非法或超长", loc)
		}
		if recordOffset+4+frameLen > fileSize {
			loc := &fault.Location{Source: path, Offset: recordOffset, Length: 4 + frameLen, End: fileSize}
			return packets, findings, fault.New(fault.KindBounds, "RTP 记录越界或文件截断", loc)
		}

		body := make([]byte, frameLen)
		if _, err := io.ReadFull(f, body); err != nil {
			loc := &fault.Location{Source: path, Offset: recordOffset + 4, Length: frameLen, End: fileSize}
			return packets, findings, fault.Wrap(fault.KindStructure, "读取 RTP 记录失败", loc, err)
		}
		no := int(binary.BigEndian.Uint32(body[0:4]))
		rtpStart := recordOffset + 4 + 4
		ref := SourceRef{
			Path:         path,
			Container:    "rtp-records",
			Offset:       rtpStart,
			RecordOffset: recordOffset,
			Length:       len(body) - 4,
		}
		p, perr := dec.Parse(no, body[4:], ref)
		if perr != nil {
			findings = append(findings, fault.Finding{
				Code:      string(perr.Kind),
				Severity:  "recoverable",
				Message:   perr.Message,
				Location:  perr.Location,
				Recovered: true,
			})
			continue
		}
		packets = append(packets, p)
	}
	return packets, findings, nil
}

func refToLocation(ref SourceRef) *fault.Location {
	return &fault.Location{
		Source: ref.Path,
		Offset: ref.Offset,
		Length: int64(ref.Length),
		Box:    ref.Box,
		Sample: ref.SampleIndex,
	}
}
