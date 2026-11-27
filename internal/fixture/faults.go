package fixture

import (
	"encoding/binary"
	"os"

	"relay/internal/media"
)

// buildLiveSet 构造主输入：基帧 + 窗内迟到帧 + 幂等重复帧。
// 去重后唯一载荷集合 = 每机位 perCam 个基帧 + 1 个迟到帧（每机位）。
func buildLiveSet(perCam int) []*media.Frame {
	base := baseFrames(perCam)
	out := make([]*media.Frame, 0, len(base)+2*CameraCount)
	out = append(out, base...)

	for cam := uint16(1); cam <= CameraCount; cam++ {
		// 迟到帧：序号 perCam（在基帧之后到达），但事件时间落在最近窗口内。
		late := makeFrame(cam, uint64(perCam))
		late.EventNS = media.TimeNS(int64(perCam-1)*FrameStepNS) - 10*media.Millisecond
		late = recheck(late)
		out = append(out, late)

		// 幂等重复：与该机位首帧完全相同（同序号、同载荷、同哈希）。
		dup := makeFrame(cam, 0).Clone()
		out = append(out, dup)
	}
	return out
}

// recheck 在修改事件时间后重新计算 CRC（SHA 只覆盖载荷，不受时间影响）。
func recheck(f *media.Frame) *media.Frame {
	rec := media.EncodeRecord(f)
	f.CRC32C = media.CRC32C(rec[:media.HeaderLen+len(f.Payload)])
	return f
}

// writeBadCRC 生成 CRC32C 被破坏的记录，scan 必须定位到该帧。
func writeBadCRC(path string, perCam int) error {
	frames := baseFrames(perCam)
	var buf []byte
	for i, f := range frames {
		rec := media.EncodeRecord(f)
		if i == 2 {
			// 翻转 CRC 末字节（记录最后 4 字节之一）。
			rec[len(rec)-1] ^= 0xA5
		}
		var p [media.PrefixLen]byte
		binary.LittleEndian.PutUint32(p[:], uint32(len(rec)))
		buf = append(buf, p[:]...)
		buf = append(buf, rec...)
	}
	return writeBytes(path, buf)
}

// writeTruncated 在完整帧后追加一条被截断的尾记录（前缀声明长度 > 实际字节）。
func writeTruncated(path string, perCam int) error {
	frames := baseFrames(perCam)
	var buf []byte
	for _, f := range frames[:len(frames)-1] {
		rec := media.EncodeRecord(f)
		var p [media.PrefixLen]byte
		binary.LittleEndian.PutUint32(p[:], uint32(len(rec)))
		buf = append(buf, p[:]...)
		buf = append(buf, rec...)
	}
	last := media.EncodeRecord(frames[len(frames)-1])
	var p [media.PrefixLen]byte
	binary.LittleEndian.PutUint32(p[:], uint32(len(last)))
	buf = append(buf, p[:]...)
	buf = append(buf, last[:len(last)-12]...) // 砍掉尾部 12 字节
	return writeBytes(path, buf)
}

// writeConflict 生成同机位同序号但载荷不同的两条记录（哈希冲突）。
func writeConflict(path string) error {
	a := makeFrame(1, 5)
	b := makeFrame(1, 5)
	b.Payload = append(append([]byte(nil), b.Payload...), 0x01) // 改变载荷
	b = recheck(b)
	return writeMFSFile(path, []*media.Frame{a, b})
}

// writeBeyondWindow 生成超出 250ms 回溯窗的迟到帧（应被隔离）。
func writeBeyondWindow(path string) error {
	first := makeFrame(1, 0)
	adv := makeFrame(1, 10) // 大幅推进事件时间
	late := makeFrame(1, 11)
	late.EventNS = adv.EventNS - media.GraceWindow - media.Millisecond // 低于水位 1ms	late = recheck(late)
	return writeMFSFile(path, []*media.Frame{first, adv, late})
}

// writeSlow 生成含大载荷帧的样例，用于演示慢消费者背压（不丢帧）。
func writeSlow(path string) error {
	big := makeFrame(1, 0)
	// 放大到约 1MiB 的确定性 PNG 不可行（PNG 压缩后很小），
	// 故直接构造合法记录体并配正确校验：载荷为可识别 PNG 头 + 填充。
	payload := make([]byte, 1<<20)
	copy(payload, big.Payload) // 保留真实 PNG 头与结构前缀
	big.Payload = payload
	big = recheck(big)
	return writeMFSFile(path, []*media.Frame{big})
}

// writeBytes 是文件写入的统一入口。
func writeBytes(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
