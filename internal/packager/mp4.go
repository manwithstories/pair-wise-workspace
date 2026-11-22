// Package packager 使用 go-mp4 把处理后的音频帧再封装为 MP4，
// 按目标码率做 CBR 填充（free box），并按统一时钟计算实际码率。
// 单段内存占用有界：帧数据流式写入，仅保留每帧 4 字节的尺寸表。
package packager

import (
	"encoding/binary"
	"io"

	"github.com/abema/go-mp4"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

// Result 封装结果统计。
type Result struct {
	Bytes      int64       `json:"bytes"`
	Samples    int64       `json:"samples"` // 每声道采样数
	Duration   clock.Ticks `json:"duration_ns"`
	ActualKbps float64     `json:"actual_kbps"`
}

// Packager 流式 MP4 封装器（单轨音频）。
type Packager struct {
	w            *mp4.Writer
	rate         int
	channels     int
	frameSamples int
	frameBytes   int
	targetKbps   int
	sizes        []uint32
	mdatPayload  uint64
	dataBytes    int64
	totalBytes   int64
	resumed      bool
}

var identityMatrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

// New 创建封装器并写入 ftyp 与 mdat 头。
// bytesPerFrame 为每帧打包字节数；targetKbps>0 时 Close 用 free box 填充至目标码率。
func New(ws io.WriteSeeker, rate, channels, frameSamples, bytesPerFrame, targetKbps int) (*Packager, error) {
	if rate <= 0 || channels <= 0 || frameSamples <= 0 || bytesPerFrame <= 0 {
		return nil, fault.New(fault.KindPackage, fault.Fail, "mp4", 0, -1,
			"invalid params rate=%d channels=%d frameSamples=%d bpf=%d",
			rate, channels, frameSamples, bytesPerFrame)
	}
	w := mp4.NewWriter(ws)
	ftyp := &mp4.Ftyp{MajorBrand: mp4.BrandISOM(), MinorVersion: 1}
	ftyp.AddCompatibleBrand(mp4.BrandISOM())
	ftyp.AddCompatibleBrand(mp4.BrandISO6())
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return nil, err
	}
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // ftyp
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return nil, err
	}
	off, err := ws.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	return &Packager{
		w: w, rate: rate, channels: channels, frameSamples: frameSamples,
		frameBytes: bytesPerFrame, targetKbps: targetKbps,
		mdatPayload: uint64(off),
	}, nil
}

// Resume 在中断的输出上继续封装：此前 mdat 未关闭，
// 帧尺寸表按均匀帧长重建，文件指针定位到已写入数据末尾。
func Resume(ws io.ReadWriteSeeker, rate, channels, frameSamples, bytesPerFrame, targetKbps int, frames int64) (*Packager, error) {
	if _, err := ws.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var hdr [8]byte
	if _, err := io.ReadFull(ws, hdr[:]); err != nil {
		return nil, fault.New(fault.KindResume, fault.Rescan, "mp4", 0, -1,
			"cannot read ftyp header: %v", err)
	}
	ftypSize := int64(binary.BigEndian.Uint32(hdr[:]))
	if ftypSize < 8 || string(hdr[4:]) != "ftyp" {
		return nil, fault.New(fault.KindResume, fault.Rescan, "mp4", 0, -1,
			"output head is not a valid ftyp box")
	}
	p := &Packager{
		w: mp4.NewWriter(ws), rate: rate, channels: channels,
		frameSamples: frameSamples, frameBytes: bytesPerFrame, targetKbps: targetKbps,
		mdatPayload: uint64(ftypSize) + 8,
		resumed:     true,
	}
	p.sizes = make([]uint32, frames)
	for i := range p.sizes {
		p.sizes[i] = uint32(p.frameBytes)
	}
	p.dataBytes = frames * int64(p.frameBytes)
	if _, err := ws.Seek(p.dataBytes+int64(p.mdatPayload), io.SeekStart); err != nil {
		return nil, err
	}
	return p, nil
}

// WriteFrame 写入一帧打包音频；长度必须为整帧，否则返回可定位错误。
func (p *Packager) WriteFrame(frame []byte) error {
	if len(frame) != p.frameBytes {
		return fault.New(fault.KindPackage, fault.Fail, "mp4", 0, len(p.sizes),
			"non-integral frame: %d bytes, want %d", len(frame), p.frameBytes)
	}
	if _, err := p.w.Write(frame); err != nil {
		return err
	}
	p.sizes = append(p.sizes, uint32(len(frame)))
	p.dataBytes += int64(len(frame))
	return nil
}

// Frames 返回已写入帧数。
func (p *Packager) Frames() int64 { return int64(len(p.sizes)) }

// Close 结束 mdat、按目标码率填充 free box 并写出 moov，返回统计。
func (p *Packager) Close() (*Result, error) {
	if p.resumed {
		if _, err := p.w.Seek(int64(p.mdatPayload)-8, io.SeekStart); err != nil {
			return nil, err
		}
		var sz [4]byte
		binary.BigEndian.PutUint32(sz[:], uint32(8+p.dataBytes))
		if _, err := p.w.Write(sz[:]); err != nil {
			return nil, err
		}
		if _, err := p.w.Seek(int64(p.mdatPayload)+p.dataBytes, io.SeekStart); err != nil {
			return nil, err
		}
	} else if _, err := p.w.EndBox(); err != nil { // mdat
		return nil, err
	}
	frames := int64(len(p.sizes))
	samples := frames * int64(p.frameSamples)
	dur := clock.SamplesToTicks(samples, p.rate)
	// moov 先写入内存以测量尺寸
	mem := newMemWS()
	mw := mp4.NewWriter(mem)
	if err := p.writeMoov(mw, samples, dur); err != nil {
		return nil, err
	}
	cur, err := p.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	// CBR 填充：总文件尺寸对齐目标码率
	if p.targetKbps > 0 && dur > 0 {
		want := int64(p.targetKbps) * 1000 * int64(dur) / 8 / int64(clock.Second)
		if pad := want - cur - int64(mem.Len()); pad >= 8 {
			if _, err := p.w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFree()}); err != nil {
				return nil, err
			}
			if _, err := p.w.Write(make([]byte, pad-8)); err != nil {
				return nil, err
			}
			if _, err := p.w.EndBox(); err != nil { // free
				return nil, err
			}
		}
	}
	if _, err := p.w.Write(mem.Bytes()); err != nil {
		return nil, err
	}
	end, err := p.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	p.totalBytes = end
	res := &Result{Bytes: p.totalBytes, Samples: samples, Duration: dur}
	if dur > 0 {
		res.ActualKbps = float64(p.totalBytes*8) / (float64(dur) / float64(clock.Second)) / 1000
	}
	return res, nil
}

func marshalTo(w *mp4.Writer, b mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: b.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func (p *Packager) writeMoov(w *mp4.Writer, samples int64, dur clock.Ticks) error {
	durMs := uint32(int64(dur) * 1000 / int64(clock.Second))
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	defer w.EndBox() // moov
	if err := marshalTo(w, &mp4.Mvhd{Timescale: 1000, DurationV0: durMs,
		Rate: 0x10000, Volume: 0x0100, Matrix: identityMatrix, NextTrackID: 2}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	defer w.EndBox() // trak
	if err := marshalTo(w, &mp4.Tkhd{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 7}},
		TrackID: 1, DurationV0: durMs, Volume: 0x0100, Matrix: identityMatrix}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	defer w.EndBox() // mdia
	if err := marshalTo(w, &mp4.Mdhd{Timescale: uint32(p.rate), DurationV0: uint32(samples),
		Language: [3]byte{'u', 'n', 'd'}}); err != nil {
		return err
	}
	if err := marshalTo(w, &mp4.Hdlr{HandlerType: [4]byte{'s', 'o', 'u', 'n'},
		Name: "SoundHandler"}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	defer w.EndBox() // minf
	if err := marshalTo(w, &mp4.Smhd{}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return err
	}
	if err := marshalTo(w, &mp4.Dref{EntryCount: 1}); err != nil {
		return err
	}
	if err := marshalTo(w, &mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // dinf
		return err
	}
	return p.writeStbl(w, samples)
}

// memWS 内存 WriteSeeker（用于测量 moov 尺寸）。
type memWS struct {
	buf []byte
	pos int64
}

func newMemWS() *memWS { return &memWS{} }

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.buf)) + off
	}
	return m.pos, nil
}

func (m *memWS) Len() int { return len(m.buf) }

func (m *memWS) Bytes() []byte { return m.buf }
