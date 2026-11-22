package pipeline

import (
	"bytes"
	"os"
	"path/filepath"

	"mediapack/internal/manifest"
)

// errInterrupted marks a deliberately simulated crash; it is not a
// corruption or runtime failure and carries no error category.
type interruptSentinel struct{}

func (interruptSentinel) Error() string { return "simulated interruption" }

var errInterrupted = interruptSentinel{}

// IsInterrupted reports whether err is the simulated-interruption marker.
func IsInterrupted(err error) bool {
	_, ok := err.(interruptSentinel)
	return ok
}

// bytesReader wraps bytes.NewReader so parse.go stays free of the bytes
// import in its main signature set.
func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }

// Outputs owns the on-disk working directory for manifest artifacts.
type Outputs struct {
	dir string
	// reps is the representation layout shared by every run.
	reps []manifest.Representation
}

// NewOutputs prepares an output directory and the default two-layer
// representation layout (high=video track 1, low=video track 2).
func NewOutputs(dir string) (*Outputs, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Outputs{
		dir: dir,
		reps: []manifest.Representation{
			{ID: "rep-high", Tracks: []uint64{1}, Bitratebps: 2_000_000},
			{ID: "rep-low", Tracks: []uint64{2}, Bitratebps: 500_000},
		},
	}, nil
}

// Representations returns the configured representation layers.
func (o *Outputs) Representations() []manifest.Representation {
	return o.reps
}

// Dir returns the output directory path.
func (o *Outputs) Dir() string { return o.dir }

// ManifestPath is the canonical manifest file location.
func (o *Outputs) ManifestPath() string {
	return filepath.Join(o.dir, "manifest.json")
}

// WriteManifest persists the canonical manifest bytes and its checksum
// sidecar so verify runs can recompute and compare.
func (o *Outputs) WriteManifest(m *manifest.Manifest, sha string) error {
	raw, err := manifest.CanonicalBytes(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.ManifestPath(), raw, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.dir, "manifest.sha256"),
		[]byte(sha+"\n"), 0o644); err != nil {
		return err
	}
	return nil
}

// ReadChecksum returns the stored manifest checksum.
func (o *Outputs) ReadChecksum() (string, error) {
	raw, err := os.ReadFile(filepath.Join(o.dir, "manifest.sha256"))
	if err != nil {
		return "", err
	}
	return trimSpace(string(raw)), nil
}

// RecomputeChecksum re-reads the manifest file and hashes its exact
// bytes, used by verify to prove the output was not altered.
func (o *Outputs) RecomputeChecksum() (string, error) {
	raw, err := os.ReadFile(o.ManifestPath())
	if err != nil {
		return "", err
	}
	return manifest.FingerprintBytes(raw), nil
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
