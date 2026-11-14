// Package keyschedule 按段数与墙钟双触发轮换，派生稳定的 epoch、key_id
// 与 AES-256 密钥材料。调度是纯函数：相同主密钥与参数在任意机器上结果一致。
//
// 离线工具不能依赖真实墙钟，因此采用确定性虚拟时钟：时间原点固定为 Unix
// 纪元（1970-01-01T00:00:00Z），每处理一段推进 1 秒。epoch-seconds>0 时
// 墙钟轮换生效；epoch-seconds<=0 时仅按 rotate-every 段轮换。
package keyschedule

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// EpochOrigin 是确定性虚拟时钟的固定时间原点（Unix 秒）。
const EpochOrigin int64 = 0

// DefaultRotateEvery 是 rotate-every 的默认值（每 4 段轮换一次）。
const DefaultRotateEvery = 4

// Schedule 描述一次加密作业的密钥轮换参数。
type Schedule struct {
	masterKey    []byte
	rotateEvery  int
	epochSeconds int64
}

// New 构造调度器。masterKey 必须为 32 字节（AES-256），rotateEvery 必须 >0。
func New(masterKey []byte, rotateEvery int, epochSeconds int64) (*Schedule, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(masterKey))
	}
	if rotateEvery <= 0 {
		return nil, fmt.Errorf("rotate-every must be positive, got %d", rotateEvery)
	}
	return &Schedule{
		masterKey:    append([]byte(nil), masterKey...),
		rotateEvery:  rotateEvery,
		epochSeconds: epochSeconds,
	}, nil
}

// SegmentTime 返回某一段的确定性虚拟墙钟（Unix 秒）。
func (s *Schedule) SegmentTime(segmentIndex int64) int64 {
	return EpochOrigin + segmentIndex
}

// EpochFor 返回某一段所属 epoch（从 0 起）。
// 段数触发：每 rotateEvery 段；墙钟触发：每 epochSeconds 秒。两者取较大值。
func (s *Schedule) EpochFor(segmentIndex int64) int64 {
	segmentEpoch := segmentIndex / int64(s.rotateEvery)
	if s.epochSeconds <= 0 {
		return segmentEpoch
	}
	timeEpoch := s.SegmentTime(segmentIndex) / s.epochSeconds
	if timeEpoch > segmentEpoch {
		return timeEpoch
	}
	return segmentEpoch
}

// KeyID 返回某一段的稳定密钥标识，形如 k-0001。
func (s *Schedule) KeyID(segmentIndex int64) string {
	return KeyIDForEpoch(s.EpochFor(segmentIndex))
}

// KeyIDForEpoch 把 epoch 编号格式化为稳定的 key_id 字符串。
func KeyIDForEpoch(epoch int64) string {
	return fmt.Sprintf("k-%04x", epoch+1)
}

// KeyFor 派生某一段使用的 32 字节 AES-256 密钥。相同输入必然得到相同密钥，
// 不同 epoch 的密钥在密码学上相互独立。
func (s *Schedule) KeyFor(segmentIndex int64) []byte {
	return DeriveSecret(s.masterKey, "aes-gcm-segment", s.EpochFor(segmentIndex))
}

// KeyForEpoch 按 epoch 直接派生 32 字节密钥，供分发器签发密钥时使用。
func (s *Schedule) KeyForEpoch(epoch int64) []byte {
	return DeriveSecret(s.masterKey, "aes-gcm-segment", epoch)
}

// RotationCount 返回 segmentCount 段实际使用的密钥批次数。段数不能整除时
// 末段不足一整块也要计入，且不得多算空批次。
func (s *Schedule) RotationCount(segmentCount int) int {
	if segmentCount <= 0 {
		return 0
	}
	lastEpoch := s.EpochFor(int64(segmentCount) - 1)
	return int(lastEpoch) + 1
}

// DeriveSecret 是全局可用的 HMAC-SHA256 派生原语，供其它包以不同 label
// 派生子密钥（清单 HMAC、许可证签名等），避免密钥用途混用。
func DeriveSecret(masterKey []byte, label string, epoch int64) []byte {
	mac := hmac.New(sha256.New, masterKey)
	var lengthBuf [8]byte
	binary.BigEndian.PutUint64(lengthBuf[:], uint64(epoch))
	mac.Write([]byte(label))
	mac.Write(lengthBuf[:])
	return mac.Sum(nil)
}

// KeyFingerprint 返回主密钥的非可逆指纹（32 字节），用于清单中标识主密钥，
// 明文密钥材料本身绝不落盘。
func KeyFingerprint(masterKey []byte) [32]byte {
	return sha256.Sum256(masterKey)
}
