// Package fixture generates deterministic sample shards, a manifest and
// master key material from a fixed seed. No external files are needed.
package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"kmsctl/internal/shard"
)

// Params controls fixture generation.
type Params struct {
	Shards    int
	ShardSize int
	Seed      int64
	Out       string
}

// MasterKey derives the master key material deterministically from the seed.
func MasterKey(seed int64) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("kmsctl/master/%d", seed)))
	return sum[:]
}

// shardBytes expands a deterministic SHA-256 counter stream to size bytes.
func shardBytes(seed int64, index, size int) []byte {
	buf := make([]byte, 0, size)
	for counter := uint64(0); len(buf) < size; counter++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("kmsctl/shard/%d/%d/%d", seed, index, counter)))
		buf = append(buf, sum[:]...)
	}
	return buf[:size]
}

// Generate writes shards, manifest.json and master.key under p.Out.
func Generate(p Params) (*shard.Manifest, error) {
	if p.Shards <= 0 {
		return nil, fmt.Errorf("shards must be positive")
	}
	if p.ShardSize <= 0 {
		return nil, fmt.Errorf("shard-size must be positive")
	}
	if err := os.MkdirAll(filepath.Join(p.Out, "shards"), 0o755); err != nil {
		return nil, err
	}
	m := &shard.Manifest{Seed: p.Seed, ShardSize: p.ShardSize}
	for i := 0; i < p.Shards; i++ {
		data := shardBytes(p.Seed, i, p.ShardSize)
		sum := sha256.Sum256(data)
		if err := os.WriteFile(shard.ShardPath(p.Out, i), data, 0o644); err != nil {
			return nil, err
		}
		m.Shards = append(m.Shards, shard.Entry{
			Index:  i,
			Size:   p.ShardSize,
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	if err := m.Save(filepath.Join(p.Out, "manifest.json")); err != nil {
		return nil, err
	}
	master := hex.EncodeToString(MasterKey(p.Seed))
	if err := os.WriteFile(filepath.Join(p.Out, "master.key"), []byte(master), 0o600); err != nil {
		return nil, err
	}
	return m, nil
}
