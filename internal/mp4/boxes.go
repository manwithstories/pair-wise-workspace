// Package mp4 流式解析分片式 MP4（fMP4）的 moof/mdat 盒与样本。
//
// 顶层盒由本包自带的有界遍历器扫描：盒长度越界时记录首个错误字节偏移，
// 并尽量恢复以继续处理后续分片；moof 内部的 tfdt/tfhd/trun 委托 go-mp4。
package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	gomp4 "github.com/abema/go-mp4"
)

const boxHeaderSize = 8

// ErrTruncated 表示文件在一个盒声明的长度之前结束。
var ErrTruncated = errors.New("box truncated")

// limitedReader 在硬边界内读取，越界即 ErrTruncated，杜绝越界读。
type limitedReader struct {
	r     io.ReaderAt
	base  int64 // 本段起始绝对偏移
	end   int64 // 文件总长度（硬边界）
	pos   int64 // 当前绝对偏移
}

func (l *limitedReader) readAt(p []byte, off int64) error {
	if off < l.base || off > l.end || off+int64(len(p)) > l.end {
		return fmt.Errorf("%w at offset %d", ErrTruncated, off)
	}
	_, err := l.r.ReadAt(p, off)
	return err
}

// readHeader 读取并校验一个顶层盒头，返回类型、声明长度与头长。
func (l *limitedReader) readHeader(off int64) (boxType string, size, header uint64, err error) {
	var head [boxHeaderSize]byte
	if err = l.readAt(head[:], off); err != nil {
		return "", 0, 0, err
	}
	size32 := binary.BigEndian.Uint32(head[0:4])
	boxType = string(head[4:8])
	switch size32 {
	case 0: // 盒延伸到文件末尾
		return boxType, uint64(l.end - off), boxHeaderSize, nil
	case 1: // 64 位 largesize
		var ext [8]byte
		if err = l.readAt(ext[:], off+boxHeaderSize); err != nil {
			return "", 0, 0, err
		}
		return boxType, binary.BigEndian.Uint64(ext[:]), boxHeaderSize + 8, nil
	default:
		return boxType, uint64(size32), boxHeaderSize, nil
	}
}

// boxInfo 是一个顶层盒的定位信息。
type boxInfo struct {
	typ    string
	off    int64
	size   uint64
	header uint64
}

// nextBox 从 off 处取下一个顶层盒；越界时返回错误与当前偏移用于故障定位。
func (l *limitedReader) nextBox(off int64) (boxInfo, error) {
	if off >= l.end {
		return boxInfo{}, io.EOF
	}
	typ, size, header, err := l.readHeader(off)
	if err != nil {
		return boxInfo{off: off}, err
	}
	if size < header {
		return boxInfo{typ: typ, off: off, size: size, header: header},
			fmt.Errorf("invalid box size %d < header %d at offset %d", size, header, off)
	}
	if uint64(l.end-off) < size {
		return boxInfo{typ: typ, off: off, size: size, header: header},
			fmt.Errorf("%w: box %s at offset %d declares size %d, %d bytes remain",
				ErrTruncated, typ, off, size, l.end-off)
	}
	return boxInfo{typ: typ, off: off, size: size, header: header}, nil
}

// readBoxBytes 读取一个完整盒（含盒头）到新缓冲；调用前已确认边界合法。
func (l *limitedReader) readBoxBytes(b boxInfo) ([]byte, error) {
	buf := make([]byte, b.size)
	if err := l.readAt(buf, b.off); err != nil {
		return nil, err
	}
	return buf, nil
}

// parseMoof 用 go-mp4 解析 moof 负载，抽取 tfdt/tfhd/trun。
func parseMoof(raw []byte) (*moofData, error) {
	r := bytes.NewReader(raw)
	var md moofData
	handler := func(h *gomp4.ReadHandle) (interface{}, error) {
		payload, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		switch b := payload.(type) {
		case *gomp4.Tfdt:
			md.baseDecodeTime = b.GetBaseMediaDecodeTime()
			md.haveTfdt = true
		case *gomp4.Tfhd:
			md.trackID = b.TrackID
			if b.CheckFlag(gomp4.TfhdDefaultSampleDurationPresent) {
				md.defaultDuration = b.DefaultSampleDuration
			}
			if b.CheckFlag(gomp4.TfhdDefaultSampleSizePresent) {
				md.haveDefaultSize = true
				md.defaultSize = b.DefaultSampleSize
			}
			if b.CheckFlag(gomp4.TfhdDefaultSampleFlagsPresent) {
				md.defaultFlags = b.DefaultSampleFlags
				md.haveDefaultFlags = true
			}
		case *gomp4.Trun:
			md.truns = append(md.truns, *b)
		}
		return nil, nil
	}
	if _, err := gomp4.ReadBoxStructure(r, handler); err != nil {
		return nil, err
	}
	return &md, nil
}
