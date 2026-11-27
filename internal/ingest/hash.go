package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// HashPacket 计算一个 PCM 包内容的确定性摘要。
func HashPacket(pcm []byte) string {
	sum := sha256.Sum256(pcm)
	return hex.EncodeToString(sum[:])
}

// SilenceSHA 返回给定帧几何的零值静音摘要（纯函数，保证补偿可复现）。
func SilenceSHA(frames int64, bytesPerFrame int) string {
	sum := sha256.Sum256(make([]byte, int(frames)*bytesPerFrame))
	return hex.EncodeToString(sum[:])
}

// PacketSHA 与块号/坐标一起摘要，便于检查点逐项核对。
func PacketSHA(p *Packet) string {
	h := sha256.New()
	fmt.Fprintf(h, "seq=%d|block=%d|start=%d|frames=%d|src=%s|", p.Seq, p.BlockID, p.StartFrame, p.Frames, p.Source)
	h.Write(p.PCM)
	return hex.EncodeToString(h.Sum(nil))
}
