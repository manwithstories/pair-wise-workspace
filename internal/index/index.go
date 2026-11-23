// Package index persists container scan results as a verifiable,
// invalidatable on-disk index. An index is only reusable when the input
// digest, scanner version and scan parameters all match; a truncated or
// checksum-failed index forces a safe re-scan and is never half-used.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"mediaroute/internal/probe"
)

// IndexVersion is bumped whenever the on-disk layout changes.
const IndexVersion = 1

// Params captures the scan-affecting parameters. Any change invalidates
// a previously generated index.
type Params struct {
	MaxFileSize int64  `json:"max_file_size"`
	MaxSamples  int    `json:"max_samples"`
	Mode        string `json:"mode"` // e.g. "mp4","mxf","auto"
}

// DefaultParams returns the production parameter set.
func DefaultParams() Params {
	return Params{
		MaxFileSize: probe.MaxFileSize,
		MaxSamples:  probe.MaxSamples,
		Mode:        "auto",
	}
}

// Header is the identity block placed at the front of the index.
type Header struct {
	Magic        string `json:"magic"`
	IndexVersion int    `json:"index_version"`
	Scanner      string `json:"scanner"`
	InputSHA256  string `json:"input_sha256"`
	Params       Params `json:"params"`
	BodySHA256   string `json:"body_sha256"`
}

// Body is the full serialised scan report plus a timestamp-independent
// checksum so repeated runs are byte-for-byte comparable.
type Body struct {
	Report *probe.ContainerReport `json:"report"`
}

// Magic identifies a mediaroute index file.
const Magic = "MEDIAROUTE-IDX"

// Fingerprint is the triple that must agree before reuse: input digest,
// scanner version and parameters.
type Fingerprint struct {
	InputSHA256 string
	Scanner     string
	Params      Params
}

// Fingerprint builds the identity triple for a report/parameter pair.
func FingerprintOf(r *probe.ContainerReport, p Params) Fingerprint {
	return Fingerprint{
		InputSHA256: r.InputSHA256,
		Scanner:     probe.ScannerVersion,
		Params:      p,
	}
}

func (f Fingerprint) String() string {
	return fmt.Sprintf("input=%s scanner=%s params=%+v",
		f.InputSHA256, f.Scanner, f.Params)
}

// Matches reports whether two fingerprints describe the same reusable work.
func (f Fingerprint) Matches(other Fingerprint) bool {
	return f.InputSHA256 == other.InputSHA256 &&
		f.Scanner == other.Scanner &&
		f.Params == other.Params
}

// checksum returns the hex SHA-256 of a JSON body. JSON encoding is
// deterministic here because the report contains only ordered slices and
// basic maps with fixed key insertion via encoding/json.
func checksum(b *Body) (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
