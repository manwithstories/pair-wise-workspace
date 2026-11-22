// Package packager 使用 go-mp4 把处理后的 PCM 帧再封装为 MP4，
// 并按统一时钟计算实际码率。单段内存占用有界：帧数据流式写入，
// 仅保留每帧 4 字节的尺寸表。
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

// Packager 流式 MP4 封装器（单轨 PCM, "sowt" 采样入口）。
type Packager struct {
	w            *mp4.Writer
	rate         int
	channels     int
	frameSamples int
	frameBytes   int
	sizes        []uint32
	mdatPayload  uint64
	dataBytes    int64
	totalBytes   int64
	resumed      bool
}

var identityMatrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

// New 创建封装器并写入 ftyp 与 mdat 头。
func New(ws io.WriteSeeker, rate, channels, frameSamples int) (*Packager, error) {
	if rate <= 0 || channels <= 0 || frameSamples <= 0 {
		return nil, fault.New(fault.KindPackage, fault.Fail, "mp4", 0, -1,
			"invalid params rate=%d channels=%d frameSamples=%d", rate, channels, frameSamples)
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
		frameBytes: frameSamples * channels * 2,
		mdatPayload: uint64(off),
	}, nil
}

// Resume 在中断的输出上继续封装：此前 mdat 未关闭，
// 帧尺寸表按均匀帧长重建，文件指针定位到已写入数据末尾。
func Resume(ws io.ReadWriteSeeker, rate, channels, frameSamples int, frames int64) (*Packager, error) {
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
		frameSamples: frameSamples, frameBytes: frameSamples * channels * 2,
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

// WriteFrame 写入一帧交错 s16le PCM；长度必须为整帧，否则返回可定位错误。
func (p *Packager) WriteFrame(pcm []byte) error {
	if len(pcm) != p.frameBytes {
		return fault.New(fault.KindPackage, fault.Fail, "mp4", 0, len(p.sizes),
			"non-integral frame: %d bytes, want %d", len(pcm), p.frameBytes)
	}
	if _, err := p.w.Write(pcm); err != nil {
		return err
	}
	p.sizes = append(p.sizes, uint32(len(pcm)))
	p.dataBytes += int64(len(pcm))
	return nil
}

// Close 结束 mdat 并写出 moov，返回统计。
func (p *Packager) Close() (*Result, error) {
	if p.resumed {
		// mdat 头由前次运行写出：手动回填尺寸
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
	if err := p.writeMoov(samples, dur); err != nil {
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

func (p *Packager) marshal(b mp4.IImmutableBox) error {
	if _, err := p.w.StartBox(&mp4.BoxInfo{Type: b.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(p.w, b, mp4.Context{}); err != nil {
		return err
	}
	_, err := p.w.EndBox()
	return err
}

func (p *Packager) writeMoov(samples int64, dur clock.Ticks) error {
	w := p.w
	durMs := uint32(int64(dur) * 1000 / int64(clock.Second))
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	defer w.EndBox() // moov
	if err := p.marshal(&mp4.Mvhd{Timescale: 1000, DurationV0: durMs,
		Rate: 0x10000, Volume: 0x0100, Matrix: identityMatrix, NextTrackID: 2}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	defer w.EndBox() // trak
	if err := p.marshal(&mp4.Tkhd{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 7}},
		TrackID: 1, DurationV0: durMs, Volume: 0x0100, Matrix: identityMatrix}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	defer w.EndBox() // mdia
	if err := p.marshal(&mp4.Mdhd{Timescale: uint32(p.rate), DurationV0: uint32(samples),
		Language: [3]byte{'u', 'n', 'd'}}); err != nil {
		return err
	}
	if err := p.marshal(&mp4.Hdlr{HandlerType: [4]byte{'s', 'o', 'u', 'n'},
		Name: "SoundHandler"}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	defer w.EndBox() // minf
	if err := p.marshal(&mp4.Smhd{}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return err
	}
	if err := p.marshal(&mp4.Dref{EntryCount: 1}); err != nil {
		return err
	}
	if err := p.marshal(&mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // dinf
		return err
	}
	return p.writeStbl(samples)
}
