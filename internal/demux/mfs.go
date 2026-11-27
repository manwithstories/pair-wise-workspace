// Package demux 将 MFS/1 长度前缀流与 MTA/1 TAR 归档解复用为统一帧模型。
package demux

import (
	"bufio"
	"encoding/binary"
	"io"

	"relay/internal/media"
)

// validateRecord 校验一条完整线格式记录（头+载荷+尾），回填摘要字段与来源坐标。
// baseOffset 为该记录（长度前缀/成员起点）在容器中的字节偏移。
func validateRecord(rec []byte, baseOffset int64, source string) (*media.Frame, *media.Fault) {
	if len(rec) < media.HeaderLen+media.TailLen {
		return nil, media.NewFault(media.StageLength, baseOffset, 0, 0,
			"record shorter than minimum: %d", len(rec)).MarkEstimated()
	}
	frame, err := media.ParseHeader(rec[:media.HeaderLen])
	if err != nil {
		f := media.AsFault(err)
		f.ByteOffset = baseOffset
		return nil, f
	}
	declared := media.PayloadLen(rec[:media.HeaderLen])
	if declared > media.MaxPayload {
		return nil, media.NewFault(media.StageLength, baseOffset, frame.CameraID, frame.Sequence,
			"declared payload %d exceeds limit %d", declared, media.MaxPayload)
	}
	wantTotal := media.HeaderLen + int(declared) + media.TailLen
	if len(rec) != wantTotal {
		return nil, media.NewFault(media.StageLength, baseOffset, frame.CameraID, frame.Sequence,
			"record length %d does not match declared %d", len(rec), wantTotal).MarkEstimated()
	}

	payload := rec[media.HeaderLen : media.HeaderLen+declared]
	tail := media.HeaderLen + declared

	gotSHA := media.SumSHA256(payload)
	var wantSHA [32]byte
	copy(wantSHA[:], rec[tail:tail+32])
	if gotSHA != wantSHA {
		return nil, media.NewFault(media.StageSHA, baseOffset, frame.CameraID, frame.Sequence,
			"payload sha256 mismatch")
	}
	wantCRC := binary.LittleEndian.Uint32(rec[tail+32 : tail+36])
	if gotCRC := media.CRC32C(rec[:tail]); gotCRC != wantCRC {
		return nil, media.NewFault(media.StageCRC, baseOffset, frame.CameraID, frame.Sequence,
			"crc32c mismatch: got %#x want %#x", gotCRC, wantCRC)
	}

	frame.Payload = append([]byte(nil), payload...)
	frame.SHA256 = gotSHA
	frame.CRC32C = wantCRC
	frame.SourceOffset = baseOffset
	frame.SourceName = source
	return frame, nil
}

// MFSReader 流式读取 MFS/1：uint32 小端长度前缀 + 线格式记录。
type MFSReader struct {
	r      *bufio.Reader
	offset int64
	name   string
}

// NewMFSReader 包装一个输入流；name 仅用于来源标注。
func NewMFSReader(r io.Reader, name string) *MFSReader {
	return &MFSReader{r: bufio.NewReaderSize(r, 1<<20), name: name}
}

// Offset 返回当前读取位置（下一条记录的前缀起点）。
func (m *MFSReader) Offset() int64 { return m.offset }

// Read 返回下一条通过全部校验的帧；流正常结束时返回 (nil, io.EOF)。
func (m *MFSReader) Read() (*media.Frame, error) {
	for {
		recOff := m.offset
		var prefix [media.PrefixLen]byte
		if _, err := io.ReadFull(m.r, prefix[:]); err != nil {
			if err == io.EOF {
				return nil, nil // 干净的帧边界结束
			}
			if err == io.ErrUnexpectedEOF {
				return nil, media.NewFault(media.StagePrefix, recOff, 0, 0,
					"truncated length prefix").MarkEstimated()
			}
			return nil, err
		}
		length := binary.LittleEndian.Uint32(prefix[:])
		m.offset += media.PrefixLen
		if length < media.HeaderLen+media.TailLen || length > media.MaxPayload+media.HeaderLen+media.TailLen {
			return nil, media.NewFault(media.StageLength, recOff, 0, 0,
				"out-of-range record length %d", length)
		}
		rec := make([]byte, length)
		if _, err := io.ReadFull(m.r, rec); err != nil {
			return nil, media.NewFault(media.StageLength, recOff, 0, 0,
				"incomplete record: %v", err).MarkEstimated()
		}
		m.offset += int64(length)
		frame, fault := validateRecord(rec, recOff, m.name)
		if fault != nil {
			return nil, fault
		}
		return frame, nil
	}
}

// ReadAll 消费整个 MFS 流，返回全部帧与首个错误（错误时帧列表可能为空）。
func ReadAll(r io.Reader, name string) ([]*media.Frame, error) {
	mr := NewMFSReader(r, name)
	var frames []*media.Frame
	for {
		f, err := mr.Read()
		if err != nil {
			return frames, err
		}
		if f == nil {
			return frames, nil
		}
		frames = append(frames, f)
	}
}
