package license

import (
	"time"

	"kmsctl/internal/keys"
)

// Reason is a machine-readable failure cause.
type Reason string

const (
	ReasonBadSignature    Reason = "bad_signature"
	ReasonClockSkew       Reason = "clock_skew"
	ReasonExpired         Reason = "expired"
	ReasonOutOfRange      Reason = "out_of_range"
	ReasonVersionRetired  Reason = "version_retired"
	ReasonKeyExpired      Reason = "key_expired"
	ReasonVersionRollback Reason = "version_rollback"
	ReasonReplay          Reason = "replay"
	ReasonTampered        Reason = "tampered"
	ReasonMachineMismatch Reason = "machine_mismatch"
)

// CheckLicense runs the license-level check chain in fixed order
// (signature, time window, range, version state, rollback, machine) and
// returns every reason found. License expiry and key expiry are reported
// as distinct reasons.
func CheckLicense(l *License, signingKey []byte, reg *keys.Registry, lastEpoch int, machine string, now time.Time, maxSkew time.Duration) []Reason {
	var reasons []Reason
	if !VerifySignature(signingKey, l) {
		reasons = append(reasons, ReasonBadSignature)
	}
	if now.Add(maxSkew).Before(l.NotBefore) {
		reasons = append(reasons, ReasonClockSkew)
	}
	if now.After(l.NotAfter.Add(maxSkew)) {
		reasons = append(reasons, ReasonExpired)
	}
	if l.ShardFrom < 0 || l.ShardTo < l.ShardFrom {
		reasons = append(reasons, ReasonOutOfRange)
	}
	v := reg.Find(l.Version)
	switch {
	case v == nil:
		reasons = append(reasons, ReasonVersionRetired)
	case v.State == keys.StateRetired:
		reasons = append(reasons, ReasonVersionRetired)
	case v.State == keys.StateGrace && !v.GraceUntil.IsZero() && now.After(v.GraceUntil):
		reasons = append(reasons, ReasonKeyExpired)
	}
	if v != nil && v.Epoch < lastEpoch {
		reasons = append(reasons, ReasonVersionRollback)
	}
	if machine != "" && l.Machine != machine {
		reasons = append(reasons, ReasonMachineMismatch)
	}
	return reasons
}

// ShardVersionReason maps a shard's key version state to a reason, or ""
// when the version may still decrypt.
func ShardVersionReason(v *keys.Version, now time.Time) Reason {
	switch {
	case v == nil:
		return ReasonVersionRetired
	case v.State == keys.StateRetired:
		return ReasonVersionRetired
	case v.State == keys.StateGrace && !v.GraceUntil.IsZero() && now.After(v.GraceUntil):
		return ReasonKeyExpired
	}
	return ""
}
