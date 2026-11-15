package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"kmsctl/internal/cryptox"
	"kmsctl/internal/keys"
	"kmsctl/internal/license"
	"kmsctl/internal/replay"
	"kmsctl/internal/shard"
)

// aggregator collects failure reasons from concurrent workers.
type aggregator struct {
	mu      sync.Mutex
	reasons map[string]int
	failed  int
}

func (a *aggregator) add(r license.Reason) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasons[string(r)]++
	a.failed++
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pack := fs.String("pack", "", "pack directory from encrypt")
	licPath := fs.String("license", "", "license file from issue")
	jsonOut := fs.Bool("json", false, "emit JSON summary")
	maxSkew := fs.Duration("max-skew", 60*time.Second, "allowed clock skew")
	replayWindow := fs.Duration("replay-window", 24*time.Hour, "replay cache window")
	replayCapacity := fs.Int("replay-capacity", 4096, "replay cache capacity")
	machine := fs.String("machine", "", "machine fingerprint (default local)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pack == "" || *licPath == "" {
		return fail(exitUsage, errRequired("--pack and --license"))
	}
	manifest, err := shard.Load(filepath.Join(*pack, "manifest.json"))
	if err != nil {
		return fail(exitUsage, err)
	}
	reg, err := keys.Load(filepath.Join(*pack, "keys.json"))
	if err != nil {
		return fail(exitUsage, err)
	}
	raw, err := os.ReadFile(filepath.Join(*pack, "meta.json"))
	if err != nil {
		return fail(exitUsage, err)
	}
	var meta packMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return fail(exitUsage, err)
	}
	master, err := loadMaster(*pack)
	if err != nil {
		return fail(exitUsage, err)
	}
	lic, err := license.Load(*licPath)
	if err != nil {
		return fail(exitUsage, err)
	}
	fp := *machine
	if fp == "" {
		fp = license.MachineFingerprint()
	}
	cache, err := replay.Open(filepath.Join(*pack, "replay.json"), *replayWindow, *replayCapacity)
	if err != nil {
		return fail(exitUsage, err)
	}
	now := time.Now()
	agg := &aggregator{reasons: map[string]int{}}

	signingKey := cryptox.SigningKey(master)
	licReasons := license.CheckLicense(lic, signingKey, reg, cache.LastEpoch(), fp, now, *maxSkew)
	sigOK := true
	for _, r := range licReasons {
		if r == license.ReasonBadSignature {
			sigOK = false
		}
		agg.add(r)
	}
	if sigOK {
		fresh, err := cache.CheckAndAdd(lic.Nonce, now)
		if err != nil {
			return fail(exitInternal, err)
		}
		if !fresh {
			agg.add(license.ReasonReplay)
		}
		if v := reg.Find(lic.Version); v != nil {
			cache.UpdateEpoch(v.Epoch)
		}
	}

	aad := manifest.Digest()
	if aad == nil {
		return fail(exitInternal, errString("manifest digest unavailable"))
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				verifyShard(*pack, manifest, reg, &meta, lic, aad, i, now, agg)
			}
		}()
	}
	for i := range manifest.Shards {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	if err := cache.Close(); err != nil {
		return fail(exitUsage, err)
	}
	boundaries := map[int]string{}
	for k, v := range meta.Boundaries {
		i, err := strconv.Atoi(k)
		if err != nil {
			return fail(exitInternal, fmt.Errorf("meta boundary key %q: %v", k, err))
		}
		boundaries[i] = v
	}
	sum := summary{
		Shards:      len(manifest.Shards),
		Rotations:   reg.Rotations,
		KeyIDs:      orderedKeyIDs(boundaries),
		Failed:      agg.failed,
		FailReasons: agg.reasons,
	}
	if *jsonOut {
		printJSON(sum)
	} else {
		fmt.Printf("shards=%d rotations=%d failed=%d reasons=%v\n",
			sum.Shards, sum.Rotations, sum.Failed, sum.FailReasons)
	}
	if agg.failed > 0 {
		return exitVerifyFailed
	}
	return exitOK
}

// verifyShard checks one shard against the license range, its key version
// state and the AEAD tag. Failures are recorded, never fatal.
func verifyShard(pack string, manifest *shard.Manifest, reg *keys.Registry, meta *packMeta, lic *license.License, aad []byte, i int, now time.Time, agg *aggregator) {
	if i < lic.ShardFrom || i > lic.ShardTo {
		agg.add(license.ReasonOutOfRange)
		return
	}
	kvID, ok := meta.ShardKeys[strconv.Itoa(i)]
	if !ok {
		agg.add(license.ReasonVersionRetired)
		return
	}
	kv := reg.Find(kvID)
	if r := license.ShardVersionReason(kv, now); r != "" {
		agg.add(r)
		return
	}
	entry, err := manifest.Entry(i)
	if err != nil {
		agg.add(license.ReasonTampered)
		return
	}
	ctPath := shard.ShardPath(pack, i)
	st, err := os.Stat(ctPath)
	if err != nil || st.Size() != int64(entry.Size+cryptox.TagSize) {
		agg.add(license.ReasonTampered)
		return
	}
	key, err := kv.Key()
	if err != nil {
		agg.add(license.ReasonTampered)
		return
	}
	ct, err := os.ReadFile(ctPath)
	if err != nil {
		agg.add(license.ReasonTampered)
		return
	}
	if _, err := cryptox.Open(key, cryptox.Nonce(kvID, i, kv.Epoch), ct, aad); err != nil {
		agg.add(license.ReasonTampered)
	}
}
