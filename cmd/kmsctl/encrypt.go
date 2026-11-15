package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"kmsctl/internal/cryptox"
	"kmsctl/internal/keys"
	"kmsctl/internal/shard"
)

// packMeta records which key version encrypted each shard.
type packMeta struct {
	ShardKeys  map[string]string `json:"shard_keys"`
	Boundaries map[string]string `json:"key_boundaries"`
}

func cmdEncrypt(args []string) int {
	fs := flag.NewFlagSet("encrypt", flag.ContinueOnError)
	fixtureDir := fs.String("fixture", "", "fixture directory from gen-fixture")
	rotateEvery := fs.Int("rotate-every", 0, "rotate key every N shards")
	rotateWindow := fs.Duration("rotate-window", 0, "rotate key after this elapsed time")
	grace := fs.Duration("grace", 24*time.Hour, "grace period for superseded keys")
	quota := fs.Int("quota", 64, "distributor lease quota")
	leaseTTL := fs.Duration("lease-ttl", 5*time.Minute, "distributor lease ttl")
	out := fs.String("out", "", "output pack directory")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *fixtureDir == "" || *out == "" {
		return fail(exitUsage, errRequired("--fixture and --out"))
	}
	manifest, err := shard.Load(filepath.Join(*fixtureDir, "manifest.json"))
	if err != nil {
		return fail(exitUsage, err)
	}
	master, err := loadMaster(*fixtureDir)
	if err != nil {
		return fail(exitUsage, err)
	}
	reg := keys.NewRegistry(*grace)
	dist := keys.NewDistributor(reg, *quota, *leaseTTL)
	aad := manifest.Digest()
	if aad == nil {
		return fail(exitInternal, errors.New("manifest digest unavailable"))
	}
	if err := os.MkdirAll(filepath.Join(*out, "shards"), 0o755); err != nil {
		return fail(exitUsage, err)
	}
	meta := packMeta{ShardKeys: map[string]string{}, Boundaries: map[string]string{}}
	boundaries := map[int]string{}
	var active *keys.Version
	lastRotate := time.Now()
	for i := range manifest.Shards {
		needRotate := active == nil ||
			(*rotateEvery > 0 && i > 0 && i%*rotateEvery == 0) ||
			(*rotateWindow > 0 && time.Since(lastRotate) >= *rotateWindow)
		if needRotate {
			id := fmt.Sprintf("kv%d", len(reg.Versions)+1)
			keyHex := hex.EncodeToString(cryptox.DeriveKey(master, id))
			v, err := reg.Rotate(id, keyHex, time.Now())
			if err != nil {
				return fail(exitInternal, err)
			}
			if _, err := dist.Acquire(id); err != nil {
				return fail(exitInternal, err)
			}
			active = v
			lastRotate = time.Now()
			boundaries[i] = id
			meta.Boundaries[strconv.Itoa(i)] = id
		}
		if _, err := dist.Acquire(active.ID); err != nil {
			return fail(exitInternal, err)
		}
		plain, err := os.ReadFile(shard.ShardPath(*fixtureDir, i))
		if err != nil {
			return fail(exitUsage, err)
		}
		key, err := active.Key()
		if err != nil {
			return fail(exitInternal, err)
		}
		ct, err := cryptox.Seal(key, cryptox.Nonce(active.ID, i, active.Epoch), plain, aad)
		if err != nil {
			return fail(exitInternal, err)
		}
		if err := os.WriteFile(shard.ShardPath(*out, i), ct, 0o644); err != nil {
			return fail(exitUsage, err)
		}
		meta.ShardKeys[strconv.Itoa(i)] = active.ID
	}
	if err := manifest.Save(filepath.Join(*out, "manifest.json")); err != nil {
		return fail(exitUsage, err)
	}
	if err := reg.Save(filepath.Join(*out, "keys.json")); err != nil {
		return fail(exitUsage, err)
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fail(exitInternal, err)
	}
	if err := os.WriteFile(filepath.Join(*out, "meta.json"), raw, 0o644); err != nil {
		return fail(exitUsage, err)
	}
	src, err := os.ReadFile(filepath.Join(*fixtureDir, "master.key"))
	if err != nil {
		return fail(exitUsage, err)
	}
	if err := os.WriteFile(filepath.Join(*out, "master.key"), src, 0o600); err != nil {
		return fail(exitUsage, err)
	}
	printJSON(summary{
		Shards:      len(manifest.Shards),
		Rotations:   reg.Rotations,
		KeyIDs:      orderedKeyIDs(boundaries),
		Failed:      0,
		FailReasons: map[string]int{},
	})
	return exitOK
}
