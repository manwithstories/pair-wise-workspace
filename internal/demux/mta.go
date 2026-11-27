package demux

import (
	"archive/tar"
	"io"

	"relay/internal/media"
)

// MTAReader 读取 MTA/1：TAR 归档，每个常规成员承载一条线格式记录。
// 成员内容与 MFS/1 记录体完全相同（不含外层 4 字节长度前缀）。
type MTAReader struct {
	tr     *tar.Reader
	name   string
	offset int64 // 当前成员头在归档中的字节偏移
}

// NewMTAReader 包装一个 TAR 输入流。
func NewMTAReader(r io.Reader, name string) *MTAReader {
	return &MTAReader{tr: tar.NewReader(r), name: name}
}

// Read 返回下一条通过全部校验的帧；归档正常结束时返回 (nil, nil)。
func (a *MTAReader) Read() (*media.Frame, error) {
	for {
		hdr, err := a.tr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			// tar 库在截断/结构损坏时返回 ParseError 或 UnexpectedEOF。
			return nil, media.NewFault(media.StageTar, a.offset, 0, 0,
				"tar structure error: %v", err).MarkEstimated()
		}
		memberOff := a.offset
		// 推进到下一成员头：头块(512) + 数据块(向上取整 512)。
		a.offset += 512 + (hdr.Size+511)/512*512
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue // 目录、PAX 扩展头等非数据成员跳过
		}
		if hdr.Size < int64(media.HeaderLen+media.TailLen) ||
			hdr.Size > int64(media.MaxPayload+media.HeaderLen+media.TailLen) {
			return nil, media.NewFault(media.StageLength, memberOff, 0, 0,
				"member %q out-of-range size %d", hdr.Name, hdr.Size)
		}
		rec := make([]byte, hdr.Size)
		if _, err := io.ReadFull(a.tr, rec); err != nil {
			return nil, media.NewFault(media.StagePayload, memberOff, 0, 0,
				"member %q truncated: %v", hdr.Name, err).MarkEstimated()
		}
		frame, fault := validateRecord(rec, memberOff, hdr.Name)
		if fault != nil {
			return nil, fault
		}
		frame.SourceName = a.name + "/" + hdr.Name
		return frame, nil
	}
}

// ReadAllTAR 消费整个 MTA/1 归档。
func ReadAllTAR(r io.Reader, name string) ([]*media.Frame, error) {
	ar := NewMTAReader(r, name)
	var frames []*media.Frame
	for {
		f, err := ar.Read()
		if err != nil {
			return frames, err
		}
		if f == nil {
			return frames, nil
		}
		frames = append(frames, f)
	}
}

// OpenContainer 按扩展名选择解复用器读取全部帧（.mta 走 TAR，其余走 MFS）。
func OpenContainer(r io.Reader, name string) ([]*media.Frame, error) {
	if isTarName(name) {
		return ReadAllTAR(r, name)
	}
	return ReadAll(r, name)
}

func isTarName(name string) bool {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			ext := name[i:]
			return ext == ".mta" || ext == ".tar"
		}
	}
	return false
}
