// Package index 扫描长度前缀包流，生成含字节偏移与扩展时间戳的摘要索引，
// 并把索引与参数摘要持久化。输入或参数变化即作废重建，支撑断点续跑。
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"

	"relay/internal/clock"
	"relay/internal/media"
	"relay/internal/rtp"
)

// Entry 是单个音频 RTP 包在索引中的摘要记录。
type Entry struct {
	Offset   int64 // 数据报在输入文件中的字节偏移（长度前缀起点）
	Seq      uint16
	ExtTS    int64 // 经回绕扩展的 RTP 时间戳
	Nanos    int64 // 统一纳秒时间轴上的呈现时刻
	PayloadN int   // 负载字节数
	Datagram int64  // 数据报次序
}

// Params 是决定处理产物的流参数；任一变化都会使旧索引失效。
type Params struct {
	AudioRate   int   // 输入 L16 采样率：8000/16000/48000/96000
	Channels    int   // 输入声道：1/2/6
	PayloadType uint8 // 音频 RTP 负载类型
	ClockRate   int   // RTP 时钟基率
	TargetRate  int   // 输出采样率
	TargetChans int   // 输出声道
}

// Digest 返回参数的稳定十六进制摘要。
func (p Params) Digest() string {
	h := sha256.New()
	var buf [8]byte
	put := func(v uint64) {
		binary.BigEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	put(uint64(p.AudioRate))
	put(uint64(p.Channels))
	put(uint64(p.PayloadType))
	put(uint64(p.ClockRate))
	put(uint64(p.TargetRate))
	put(uint64(p.TargetChans))
	return hex.EncodeToString(h.Sum(nil))
}

// Index 是扫描产物。
type Index struct {
	InputDigest string `json:"input_digest"`
	ParamDigest string `json:"param_digest"`
	Entries     []Entry
	PacketCount int64  `json:"packet_count"`
	SRCount     int64  `json:"sr_count"`
	PPM         int    `json:"ppm"`
	WrapCount   uint64 `json:"wrap_count"`
}

// Scan 顺序读取包流并构建索引。读取经过的字节同时喂入 sha256 得到输入摘要，
// 时钟恢复与扫描共用一遍数据。可恢复的解析故障经 onFault 上抛后继续扫描。
func Scan(r io.Reader, params Params, onFault func(*media.Fault)) (*Index, error) {
	hasher := sha256.New()
	tee := io.TeeReader(r, hasher)
	rr := rtp.NewReader(tee)
	rec := clock.NewRecover(params.ClockRate)
	idx := &Index{ParamDigest: params.Digest()}

	for {
		dg, err := rr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			f, ok := media.FindFault(err)
			if !ok {
				return nil, err
			}
			if onFault != nil {
				onFault(f)
			}
			// 长度前缀级故障无法定位下一条边界，必须终止（退出码 2）；
			// 数据报内部的头部/负载故障已按前缀跳过，扫描继续。
			if f.Kind == media.FaultFraming {
				return nil, err
			}
			continue
		}
		if dg.IsRTCP {
			if dg.PayloadType == 200 {
				rec.ObserveSR(clock.SRPoint{
					NTPSeconds:  dg.SRNTPSeconds,
					NTPFraction: dg.SRNTPFraction,
					RTP:         dg.SRRTP,
				})
				idx.SRCount++
			}
			continue
		}
		ext := rec.ObserveRTP(dg.Timestamp)
		idx.Entries = append(idx.Entries, Entry{
			Offset:   dg.Offset,
			Seq:      dg.Seq,
			ExtTS:    ext,
			Nanos:    rec.ToNanos(ext).Ticks,
			PayloadN: len(dg.Payload),
			Datagram: dg.Index,
		})
		idx.PacketCount++
	}
	idx.PPM = rec.PPM()
	idx.WrapCount = rec.WrapCount()
	idx.InputDigest = hex.EncodeToString(hasher.Sum(nil))
	return idx, nil
}

// hashReader 流式计算 r 的 sha256，避免整文件载入内存（128MiB 上限）。
func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", &media.IOError{Op: "hash", Reason: err.Error()}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
