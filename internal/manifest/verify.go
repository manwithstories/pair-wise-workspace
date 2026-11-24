package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"mediad/internal/mp4"
)

// VerifyReport is the result of re-hashing artifacts and re-running the
// parse/encode roundtrip on every delivered fragment.
type VerifyReport struct {
	SHAOK           int      `json:"sha_ok"`
	SHATotal        int      `json:"sha_total"`
	RoundTripOK     int      `json:"roundtrip_ok"`
	RoundTripTotal  int      `json:"roundtrip_total"`
	Missing         []string `json:"missing,omitempty"`
	SHAFailed       []string `json:"sha_failed,omitempty"`
	RoundTripFailed []string `json:"roundtrip_failed,omitempty"`
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// VerifyManifest re-reads every artifact referenced by the manifest under
// baseDir. Quarantined items participate in SHA verification (their bytes are
// evidence) but not in roundtrip verification.
func VerifyManifest(baseDir string, m *Manifest) VerifyReport {
	var rep VerifyReport

	for _, fr := range m.Fragments {
		rep.SHATotal++
		rep.RoundTripTotal++
		full := filepath.Join(baseDir, filepath.FromSlash(fr.Path))
		b, err := os.ReadFile(full)
		if err != nil {
			rep.Missing = append(rep.Missing, fr.Path)
			continue
		}
		if sha256Hex(b) == fr.SHA256 {
			rep.SHAOK++
		} else {
			rep.SHAFailed = append(rep.SHAFailed, fr.Path)
		}
		if _, out, err := mp4.RoundTrip(b); err == nil && bytes.Equal(b, out) {
			rep.RoundTripOK++
		} else {
			rep.RoundTripFailed = append(rep.RoundTripFailed, fr.Path)
		}
	}

	for _, q := range m.Quarantined {
		rep.SHATotal++
		full := filepath.Join(baseDir, filepath.FromSlash(q.Path))
		b, err := os.ReadFile(full)
		if err != nil {
			rep.Missing = append(rep.Missing, q.Path)
			continue
		}
		if sha256Hex(b) == q.SHA256 {
			rep.SHAOK++
		} else {
			rep.SHAFailed = append(rep.SHAFailed, q.Path)
		}
	}
	return rep
}

// Complete reports whether every hash and every expected roundtrip passed.
func (r VerifyReport) Complete() bool {
	return r.SHAOK == r.SHATotal &&
		r.RoundTripOK == r.RoundTripTotal &&
		len(r.Missing) == 0
}
