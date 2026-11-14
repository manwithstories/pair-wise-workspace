// Package keydist 是进程内密钥分发器：凭校验通过的许可证换发带 TTL 的
// 一次性取密钥凭据，按段赎回对应 epoch 的 AES 密钥。全程不接触网络，
// 每次签发与赎回都写入内存台账，供审计回溯。
package keydist

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"kmsctl/internal/keyschedule"
	"kmsctl/internal/license"
)

// 分发器相关失败原因。
var (
	ErrKeyExpired  = errors.New("KEY_EXPIRED")
	ErrOutOfScope  = errors.New("OUT_OF_SCOPE")
	ErrAlreadyUsed = errors.New("CREDENTIAL_ALREADY_USED")
)

// Credential 是一次性取密钥凭据。
type Credential struct {
	ID        string    `json:"id"`
	Device    string    `json:"device"`
	Sequence  uint64    `json:"sequence"`
	Nonce     string    `json:"nonce"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LedgerEntry 记录一次凭据的完整生命周期。
type LedgerEntry struct {
	Credential Credential   `json:"credential"`
	FirstSeg   int          `json:"first_segment"`
	LastSeg    int          `json:"last_segment"`
	Redeemed   map[int]bool `json:"redeemed_segments"`
}

// Distributor 依赖密钥调度，按许可证分发派生密钥。
type Distributor struct {
	schedule *keyschedule.Schedule
	ttl      time.Duration
	now      func() time.Time
	entries  map[string]*LedgerEntry
	counter  uint64
}

// New 构造分发器。ttl<=0 时使用默认 5 分钟。
func New(schedule *keyschedule.Schedule, ttl time.Duration, now func() time.Time) *Distributor {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if now == nil {
		now = time.Now
	}
	return &Distributor{
		schedule: schedule,
		ttl:      ttl,
		now:      now,
		entries:  map[string]*LedgerEntry{},
	}
}

// credentialID 由许可证签名确定性派生，使同一许可证得到稳定可追溯的凭据号。
func credentialID(l *license.License) (uint64, string) {
	sum := sha256.Sum256([]byte(l.Sig))
	num := binary.BigEndian.Uint64(sum[:8])
	return num, hex.EncodeToString(sum[8:24])
}

// Issue 凭许可证签发一次性凭据。序号在进程内单调递增。
func (d *Distributor) Issue(l *license.License) *Credential {
	d.counter++
	num, nonce := credentialID(l)
	issued := d.now()
	cred := Credential{
		ID:        fmt.Sprintf("cred-%016x", num),
		Device:    l.Claim.Device,
		Sequence:  d.counter,
		Nonce:     nonce,
		IssuedAt:  issued,
		ExpiresAt: issued.Add(d.ttl),
	}
	d.entries[cred.ID] = &LedgerEntry{
		Credential: cred,
		FirstSeg:   l.Claim.FirstSeg,
		LastSeg:    l.Claim.LastSeg,
		Redeemed:   map[int]bool{},
	}
	return &cred
}

// Redeem 用凭据赎回指定段的密钥与 epoch。超时返回 KEY_EXPIRED，越界返回
// OUT_OF_SCOPE，同段重复赎回返回 CREDENTIAL_ALREADY_USED。
func (d *Distributor) Redeem(cred *Credential, segmentIndex int) ([]byte, int64, error) {
	entry, ok := d.entries[cred.ID]
	if !ok {
		return nil, 0, fmt.Errorf("%w: unknown credential", ErrAlreadyUsed)
	}
	now := d.now()
	if now.After(cred.ExpiresAt) {
		return nil, 0, fmt.Errorf("%w: expired at %s", ErrKeyExpired, cred.ExpiresAt.Format(time.RFC3339))
	}
	if segmentIndex < entry.FirstSeg || segmentIndex > entry.LastSeg {
		return nil, 0, fmt.Errorf("%w: segment %d outside %d-%d",
			ErrOutOfScope, segmentIndex, entry.FirstSeg, entry.LastSeg)
	}
	if entry.Redeemed[segmentIndex] {
		return nil, 0, fmt.Errorf("%w: segment %d", ErrAlreadyUsed, segmentIndex)
	}
	entry.Redeemed[segmentIndex] = true
	epoch := d.schedule.EpochFor(int64(segmentIndex))
	return d.schedule.KeyForEpoch(epoch), epoch, nil
}

// Records 返回发放台账的只读副本，供审计使用。
func (d *Distributor) Records() map[string]*LedgerEntry {
	out := make(map[string]*LedgerEntry, len(d.entries))
	for id, entry := range d.entries {
		copy := *entry
		out[id] = &copy
	}
	return out
}
