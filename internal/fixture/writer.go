package fixture

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediarepair/internal/fault"
)

// samplePlan 是一个待写入样本的时长、负载尺寸与关键帧标记。
type samplePlan struct {
	delta    uint32
	size     int
	keyframe bool
}

// payloadFor 生成确定性的样本负载：头部写入轨道与序号，其余按线性同余填充。
func payloadFor(trackID uint32, seq uint64, size int) []byte {
	b := make([]byte, size)
	if size >= 12 {
		binary.BigEndian.PutUint32(b, trackID)
		binary.BigEndian.PutUint64(b[4:], seq)
	}
	seed := crc32.NewIEEE()
	fmt.Fprintf(seed, "t%d-s%d", trackID, seq)
	v := seed.Sum32()
	for i := 12; i < size; i++ {
		b[i] = byte(v >> uint((i%4)*8))
		if i%4 == 3 {
			v = v*1664525 + 1013904223
		}
	}
	return b
}

// mp4Writer 封装 go-mp4 的盒写入，记录需要回填的 stco 位置。
type mp4Writer struct {
	w       *mp4.Writer
	f       *os.File
	stcoPos []int64 // 每条轨道 stco 首个 chunk offset 字段的文件偏移
}

func (mw *mp4Writer) start(t mp4.BoxType) error {
	_, err := mw.w.StartBox(&mp4.BoxInfo{Type: t})
	return err
}

func (mw *mp4Writer) end() error {
	_, err := mw.w.EndBox()
	return err
}

func marshalBox(w *mp4.Writer, box mp4.IImmutableBox) error {
	_, err := mp4.Marshal(w, box, mp4.Context{})
	return err
}

// visualEntry 手写 avc1 视觉样本描述（不带 avcC，供结构解析演示）。
func visualEntry(width, height uint16) []byte {
	buf := make([]byte, 0, 86)
	be := binary.BigEndian
	buf = append(buf, make([]byte, 6)...)
	buf = be.AppendUint16(buf, 1) // data_reference_index
	buf = be.AppendUint16(buf, 0) // pre_defined
	buf = be.AppendUint16(buf, 0) // reserved
	buf = be.AppendUint32(buf, 0) // pre_defined[0..2]
	buf = be.AppendUint32(buf, 0)
	buf = be.AppendUint32(buf, 0)
	buf = be.AppendUint16(buf, width)
	buf = be.AppendUint16(buf, height)
	buf = be.AppendUint32(buf, 0x00480000) // horizresolution
	buf = be.AppendUint32(buf, 0x00480000) // vertresolution
	buf = be.AppendUint32(buf, 0)
	buf = be.AppendUint16(buf, 1) // frame_count
	name := make([]byte, 32)
	name[0] = 4
	copy(name[1:], "test")
	buf = append(buf, name...)
	buf = be.AppendUint16(buf, 24)
	buf = be.AppendUint16(buf, 0xffff)
	return buf
}

// audioEntry 手写 mp4a 音频样本描述。
func audioEntry(channels uint16, rate uint32) []byte {
	buf := make([]byte, 0, 28)
	be := binary.BigEndian
	buf = append(buf, make([]byte, 6)...)
	buf = be.AppendUint16(buf, 1) // data_reference_index
	buf = be.AppendUint16(buf, 0) // entry_version
	buf = be.AppendUint16(buf, 0) // reserved[0..2]
	buf = be.AppendUint16(buf, 0)
	buf = be.AppendUint16(buf, 0)
	buf = be.AppendUint16(buf, channels) // channelcount
	buf = be.AppendUint16(buf, 16)       // samplesize
	buf = be.AppendUint16(buf, 0)        // pre_defined
	buf = be.AppendUint16(buf, 0)        // reserved
	buf = be.AppendUint32(buf, rate<<16) // samplerate (16.16)
	return buf
}

// writeMP4 写出双轨 MP4：ftyp + moov（trak/mdia/minf/stbl）+ mdat。
func writeMP4(path string, spec Spec, lay *layout) error {
	f, err := os.Create(path)
	if err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "创建 MP4 失败", err)
	}
	defer f.Close()
	mw := &mp4Writer{w: mp4.NewWriter(f), f: f}

	if err := mw.ftyp(); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "写 ftyp 失败", err)
	}
	if err := mw.moov(spec, lay); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "写 moov 失败", err)
	}

	if err := mw.start(mp4.BoxTypeMdat()); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "写 mdat 失败", err)
	}
	dataOff, err := f.Seek(0, 1)
	if err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "定位 mdat 负载失败", err)
	}
	audioOff := dataOff
	for i, sp := range lay.video {
		if _, err := mw.w.Write(payloadFor(1, uint64(i), sp.size)); err != nil {
			return fault.Wrap(fault.StageIO, 0, -1, false, "写视频样本失败", err)
		}
	}
	audioOff = dataOff + int64(len(lay.video))*int64(videoPayload)
	for i, sp := range lay.audio {
		if _, err := mw.w.Write(payloadFor(2, uint64(i), sp.size)); err != nil {
			return fault.Wrap(fault.StageIO, 0, -1, false, "写音频样本失败", err)
		}
	}
	if err := mw.end(); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "关闭 mdat 失败", err)
	}

	// 回填每条轨道的首个 chunk offset。
	chunks := []int64{dataOff, audioOff}
	if len(chunks) < len(mw.stcoPos) {
		return fault.New(fault.StageIO, 0, -1, false, "stco 回填位置数量与轨道不一致")
	}
	var buf [4]byte
	for i, pos := range mw.stcoPos {
		binary.BigEndian.PutUint32(buf[:], uint32(chunks[i]))
		if _, err := f.WriteAt(buf[:], pos); err != nil {
			return fault.Wrap(fault.StageIO, 0, -1, false, "回填 stco 失败", err)
		}
	}
	return nil
}
