// Package wav 解析以 RTP 包负载承载的 PCM/WAV 多声道采样块，
// 全程保留原始字节偏移，并对可恢复错误采用“记录后继续”策略。
package wav

import (
	"encoding/binary"
	"encoding/json"

	"pairflow/internal/clock"
)

// Format 描述一个 WAV 块声明的 PCM 格式。
type Format struct {
	SampleRate   uint32 `json:"sample_rate"`
	Channels     uint16 `json:"channels"`
	BitsPerSample uint16 `json:"bits_per_sample"`
}

// FrameBytes 返回单帧（所有声道一个采样）的字节数。
func (f Format) FrameBytes() int { return int(f.Channels) * int(f.BitsPerSample) / 8 }

// Block 是一个解析后的采样块；即使出现可恢复错误，也尽量填充可用字段。
type Block struct {
	DeviceID    string          `json:"device_id"`
	SourceOffset int64          `json:"source_offset"` // 块在采集文件中的字节偏移
	RTPSeq      uint64          `json:"rtp_seq"`
	RTPTimestamp uint32         `json:"rtp_timestamp"`
	ArrivalNano int64           `json:"arrival_nano"`
	Format      *Format         `json:"format,omitempty"`
	DataOffset  int64           `json:"data_offset"` // data 负载相对源块起点的偏移
	Data        []byte          `json:"-"`           // 原始 PCM 字节
	FrameCount  int64           `json:"frame_count"`
	Recoverable bool            `json:"recoverable"` // 出错但仍可继续后续处理
	Errors      []*clock.StageError `json:"errors,omitempty"`
}

// SampleCount 返回该块包含的每声道采样数（帧数）。
func (b *Block) SampleCount() int64 { return b.FrameCount }

// Marshal 使 Block 可在检查点中以 JSON 持久化（Data 单独落盘）。
func (b *Block) Marshal() ([]byte, error)  { return json.Marshal(b) }

const (
	riffHeaderLen = 12 // "RIFF" + size(4) + "WAVE"
	fmtPCM        = 1
)

// ParseBlock 解析单个 WAV 字节块。任何可恢复错误都会写入 Block.Errors，
// 调用方仍可从返回的 Block 取得已识别的格式与数据；严重损坏返回 Recoverable=false。
func ParseBlock(deviceID string, sourceOffset int64, rtpSeq uint64, rtpTS uint32, arrival int64, raw []byte) *Block {
	blk := &Block{DeviceID: deviceID, SourceOffset: sourceOffset, RTPSeq: rtpSeq,
		RTPTimestamp: rtpTS, ArrivalNano: arrival, Recoverable: true}
	if len(raw) < riffHeaderLen {
		blk.Recoverable = false
		blk.Errors = append(blk.Errors, clock.New("wav.parse", clock.KindTruncatedHeader,
			deviceID, sourceOffset, rtpSeq,
			"wav header truncated before RIFF/WAVE", "请求重传该源块"))
		return blk
	}
	if string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		blk.Recoverable = false
		blk.Errors = append(blk.Errors, clock.New("wav.parse", clock.KindTruncatedHeader,
			deviceID, sourceOffset, rtpSeq, "missing RIFF/WAVE signature", "请求重传该源块"))
		return blk
	}
	pos := riffHeaderLen
	var dataLen, dataStart int
	dataFound := false
	for pos+8 <= len(raw) {
		chunkID := string(raw[pos : pos+4])
		chunkLen := int(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
		body := pos + 8
		if body+chunkLen > len(raw) {
			blk.Errors = append(blk.Errors, clock.New("wav.parse", clock.KindTruncatedHeader,
				deviceID, sourceOffset+int64(pos), rtpSeq,
				"chunk body truncated: "+chunkID, "请求重传该源块"))
			return blk
		}
		switch chunkID {
		case "fmt ":
			if chunkLen < 16 {
				blk.Errors = append(blk.Errors, clock.New("wav.parse", clock.KindTruncatedHeader,
					deviceID, sourceOffset+int64(pos), rtpSeq,
					"fmt chunk shorter than PCM minimum", "请求重传该源块"))
			} else {
				tag := binary.LittleEndian.Uint16(raw[body : body+2])
				ch := binary.LittleEndian.Uint16(raw[body+2 : body+4])
				sr := binary.LittleEndian.Uint32(raw[body+4 : body+8])
				bits := binary.LittleEndian.Uint16(raw[body+14 : body+16])
				if tag != fmtPCM {
					blk.Errors = append(blk.Errors, clock.New("wav.parse",
						clock.KindUnsupportedPayload, deviceID, sourceOffset+int64(pos),
						rtpSeq, "only uncompressed PCM (format tag 1) is supported", "跳过该块"))
				}
				blk.Format = &Format{SampleRate: sr, Channels: ch, BitsPerSample: bits}
			}
		case "data":
			dataFound = true
			dataStart = body
			dataLen = chunkLen
		}
		pos = body + chunkLen + (chunkLen & 1) // RIFF 块按字对齐填充
	}
	if !dataFound {
		blk.Errors = append(blk.Errors, clock.New("wav.parse", clock.KindTruncatedHeader,
			deviceID, sourceOffset, rtpSeq, "no data chunk present", "请求重传该源块"))
		return blk
	}
	blk.DataOffset = sourceOffset + int64(dataStart)
	blk.Data = raw[dataStart : dataStart+dataLen]
	if blk.Format == nil {
		blk.Errors = append(blk.Errors, clock.New("wav.parse", clock.KindTruncatedHeader,
			deviceID, sourceOffset, rtpSeq, "no fmt chunk present", "请求重传该源块"))
		return blk
	}
	frame := blk.Format.FrameBytes()
	if frame <= 0 {
		blk.Errors = append(blk.Errors, clock.New("wav.parse", clock.KindUnsupportedPayload,
			deviceID, sourceOffset, rtpSeq, "invalid frame geometry", "跳过该块"))
		blk.Recoverable = false
		return blk
	}
	blk.FrameCount = int64(dataLen / frame)
	if rem := dataLen % frame; rem != 0 {
		blk.Errors = append(blk.Errors, clock.New("wav.parse", clock.KindNonIntegralFrame,
			deviceID, blk.DataOffset+int64(dataLen-rem), rtpSeq,
			"data length is not an integral number of frames",
			"丢弃尾部不完整帧后继续"))
		blk.Data = blk.Data[:dataLen-rem]
	}
	return blk
}

// DetectFormatChange 与前一块格式比较，发现采样率/声道/位深中途变化时返回结构化错误。
// 规格仅强制要求采样率与声道数；位深变化同样按错误上报，避免静默混轨。
func DetectFormatChange(prev, cur *Block) *clock.StageError {
	if prev == nil || cur == nil || prev.Format == nil || cur.Format == nil {
		return nil
	}
	if prev.Format.SampleRate == cur.Format.SampleRate &&
		prev.Format.Channels == cur.Format.Channels &&
		prev.Format.BitsPerSample == cur.Format.BitsPerSample {
		return nil
	}
	return clock.New("wav.parse", clock.KindFormatChanged, cur.DeviceID,
		cur.SourceOffset, cur.RTPSeq,
		"pcm format changed mid-stream",
		"按新格式分段并保留偏移，交修复层判定时间线")
}
