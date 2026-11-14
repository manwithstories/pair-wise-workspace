// Package license 实现离线许可证的签发与校验。许可证绑定设备号、允许解密
// 的段范围以及 not-before/not-after 有效期，全部字段参与 HMAC-SHA256 签名。
// 校验时容忍有限时钟偏移（防篡改的设备时钟抖动），超出窗口即判失效。
package license

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"kmsctl/internal/keyschedule"
)

// 许可证相关失败原因。
var (
	ErrDeviceMismatch = errors.New("DEVICE_MISMATCH")
	ErrBadSignature   = errors.New("LICENSE_SIGNATURE_INVALID")
	ErrExpired        = errors.New("EXPIRED")
	ErrNotYetValid    = errors.New("NOT_YET_VALID")
)

// DefaultClockSkew 是默认容忍的时钟偏移（秒）。
const DefaultClockSkew = 60

// Claim 是许可证的完整声明，所有字段都被签名覆盖。
type Claim struct {
	Device    string    `json:"device"`
	FirstSeg  int       `json:"first_segment"`
	LastSeg   int       `json:"last_segment"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	IssuedAt  time.Time `json:"issued_at"`
}

// License 是落盘的许可证结构。
type License struct {
	Claim Claim  `json:"claim"`
	Sig   string `json:"sig"`
}

func signingKey(masterKey []byte) []byte {
	return keyschedule.DeriveSecret(masterKey, "license-hmac", 0)
}

func canonical(l *License) []byte {
	copy := License{Claim: l.Claim}
	raw, _ := json.Marshal(copy)
	return raw
}

// Issue 用主密钥对声明签名，生成完整许可证。
func Issue(masterKey []byte, claim Claim) (*License, error) {
	if claim.Device == "" {
		return nil, errors.New("license device must not be empty")
	}
	if claim.FirstSeg < 0 || claim.LastSeg < claim.FirstSeg {
		return nil, fmt.Errorf("invalid segment range %d-%d", claim.FirstSeg, claim.LastSeg)
	}
	if !claim.NotAfter.After(claim.NotBefore) {
		return nil, errors.New("not-after must be later than not-before")
	}
	if claim.IssuedAt.IsZero() {
		claim.IssuedAt = time.Unix(keyschedule.EpochOrigin, 0).UTC()
	}
	l := &License{Claim: claim}
	mac := hmac.New(sha256.New, signingKey(masterKey))
	mac.Write(canonical(l))
	l.Sig = hex.EncodeToString(mac.Sum(nil))
	return l, nil
}

// VerifyAt 在给定时刻校验设备绑定、签名与有效期。skewSeconds 为容忍的
// 时钟偏移窗口；设备不符与签名失效优先于时间判定。
func (l *License) VerifyAt(masterKey []byte, device string, now time.Time, skewSeconds int64) error {
	if l.Claim.Device != device {
		return fmt.Errorf("%w: license for %q, presented by %q",
			ErrDeviceMismatch, l.Claim.Device, device)
	}
	want, err := hex.DecodeString(l.Sig)
	if err != nil {
		return fmt.Errorf("%w: bad signature encoding", ErrBadSignature)
	}
	mac := hmac.New(sha256.New, signingKey(masterKey))
	mac.Write(canonical(l))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return ErrBadSignature
	}
	skew := time.Duration(skewSeconds) * time.Second
	if now.Add(skew).Before(l.Claim.NotBefore) {
		return ErrNotYetValid
	}
	if now.Add(-skew).After(l.Claim.NotAfter) {
		return ErrExpired
	}
	return nil
}

// Covers 判断许可证段范围是否包含指定段。
func (l *License) Covers(segmentIndex int) bool {
	return segmentIndex >= l.Claim.FirstSeg && segmentIndex <= l.Claim.LastSeg
}

// Save 以 0600 权限写入许可证。
func (l *License) Save(path string) error {
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// Load 读取许可证文件（此时不校验，校验由 VerifyAt 完成）。
func Load(path string) (*License, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l License
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// ParseRange 解析 "0-15" 形式的闭区间段范围。
func ParseRange(text string) (int, int, error) {
	parts := strings.Split(text, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("range must look like 0-15, got %q", text)
	}
	first, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("bad range start: %w", err)
	}
	last, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("bad range end: %w", err)
	}
	if first < 0 || last < first {
		return 0, 0, fmt.Errorf("invalid segment range %d-%d", first, last)
	}
	return first, last, nil
}
