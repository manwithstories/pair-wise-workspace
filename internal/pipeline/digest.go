package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"mediadeliver/internal/model"
)

// DigestBytes returns the hex SHA-256 of an in-memory byte slice.
func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DigestFile streams a file and returns its hex SHA-256 and size.
func DigestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, model.Wrap(model.KindIO, "pipeline", "digest file", -1,
			"cannot open "+path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, model.Wrap(model.KindIO, "pipeline", "digest file", -1,
			"cannot read "+path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// BuildInputDigest assembles the checkpoint input digest from manifest text
// and the per-material source file digests.
func BuildInputDigest(manifestText []byte, sourceFiles map[string]string) (model.InputDigest, error) {
	out := model.InputDigest{
		ManifestSHA256: DigestBytes(manifestText),
		SourceSHA256:   map[string]string{},
	}
	for id, path := range sourceFiles {
		sum, _, err := DigestFile(path)
		if err != nil {
			return model.InputDigest{}, err
		}
		out.SourceSHA256[id] = sum
	}
	return out, nil
}
