package index

import (
	"encoding/json"
	"os"
	"path/filepath"

	"archive/internal/model"
)

// LoadManifest reads the job manifest. A missing file means a fresh run and
// returns (nil, nil); malformed JSON is a hard index error.
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, model.New(model.KindIO, path, "cannot read manifest", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, model.New(model.KindIndex, path, "corrupt manifest json", err)
	}
	return &m, nil
}

// Resume describes where a restarted run continues.
type Resume struct {
	Available       bool
	CompleteChunks  int
	ResumeOrdinal   int64 // next source packet ordinal to process
	RecoveredChunks int   // complete chunks reused without reprocessing
}

// VerifyAndResume validates the manifest against the current job digests and
// re-checks every chunk checksum. It returns the resume point at the last
// complete valid chunk. Mismatches and corruption produce explicit Kind
// errors that the command layer maps to exit code 2.
func VerifyAndResume(dir string, inputDigest, paramDigest string) (Resume, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return Resume{}, err
	}
	if m == nil {
		return Resume{}, nil // nothing committed: start from scratch
	}
	if m.Version != Version {
		return Resume{}, model.Newf(model.KindInput, "manifest",
			"index version mismatch: stored=%d supported=%d", m.Version, Version)
	}
	if m.InputDigest != inputDigest {
		return Resume{}, model.New(model.KindInput, "manifest",
			"input digest changed; refusing to resume across different source data", nil)
	}
	if m.ParamDigest != paramDigest {
		return Resume{}, model.New(model.KindInput, "manifest",
			"parameter digest changed; refusing to resume with different processing parameters", nil)
	}

	valid := 0
	var resumeOrdinal int64
	for i, c := range m.Chunks {
		if err := verifyChunkFile(dir, c); err != nil {
			if i == 0 {
				return Resume{}, err
			}
			// Tail corruption: rebuild from the last good chunk.
			break
		}
		if !c.Complete {
			break // incomplete tail is rebuilt, never trusted
		}
		valid++
		resumeOrdinal = c.PacketEnd + 1
	}
	if valid == 0 {
		return Resume{}, model.New(model.KindIndex, "chunks",
			"no valid complete chunk found; index must be rebuilt", nil)
	}
	return Resume{
		Available:       true,
		CompleteChunks:  valid,
		ResumeOrdinal:   resumeOrdinal,
		RecoveredChunks: valid,
	}, nil
}

// verifyChunkFile re-reads a chunk from disk and recomputes its checksum.
func verifyChunkFile(dir string, want Chunk) error {
	path := filepath.Join(dir, chunkName(want.Index))
	data, err := os.ReadFile(path)
	if err != nil {
		return model.New(model.KindIndex, path, "chunk missing or unreadable", err)
	}
	var got Chunk
	if err := json.Unmarshal(data, &got); err != nil {
		return model.New(model.KindIndex, path, "corrupt chunk json", err)
	}
	if got.Version != Version {
		return model.Newf(model.KindIndex, path, "chunk version mismatch: %d", got.Version)
	}
	if checksum(got) != got.Checksum {
		return model.New(model.KindIndex, path, "chunk checksum mismatch", nil)
	}
	if got.Checksum != want.Checksum {
		return model.New(model.KindIndex, path, "manifest/chunk checksum disagreement", nil)
	}
	return nil
}

// CountCompleteChunks returns the number of complete chunks in a manifest,
// used by the verify command and metrics.
func CountCompleteChunks(m *Manifest) int {
	n := 0
	for _, c := range m.Chunks {
		if c.Complete {
			n++
		}
	}
	return n
}
