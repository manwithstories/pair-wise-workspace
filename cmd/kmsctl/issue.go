package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kmsctl/internal/cryptox"
	"kmsctl/internal/keys"
	"kmsctl/internal/license"
)

func parseShardRange(s string) (int, int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid shard range %q, want A-B", s)
	}
	from, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid shard range %q: %v", s, err)
	}
	to, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid shard range %q: %v", s, err)
	}
	if from < 0 || to < from {
		return 0, 0, fmt.Errorf("invalid shard range %q", s)
	}
	return from, to, nil
}

func cmdIssue(args []string) int {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	pack := fs.String("pack", "", "pack directory from encrypt")
	ttl := fs.Duration("ttl", time.Hour, "license validity duration")
	shardRange := fs.String("shards", "", "shard range A-B")
	nonce := fs.String("nonce", "", "nonce hex (default random)")
	machine := fs.String("machine", "", "machine fingerprint (default local)")
	out := fs.String("out", "", "output license file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pack == "" || *out == "" || *shardRange == "" {
		return fail(exitUsage, errRequired("--pack, --shards and --out"))
	}
	from, to, err := parseShardRange(*shardRange)
	if err != nil {
		return fail(exitUsage, err)
	}
	master, err := loadMaster(*pack)
	if err != nil {
		return fail(exitUsage, err)
	}
	reg, err := keys.Load(filepath.Join(*pack, "keys.json"))
	if err != nil {
		return fail(exitUsage, err)
	}
	active := reg.Active()
	if active == nil {
		return fail(exitInternal, errString("registry has no active key version"))
	}
	nonceHex := *nonce
	if nonceHex == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return fail(exitInternal, err)
		}
		nonceHex = hex.EncodeToString(buf)
	} else if _, err := hex.DecodeString(nonceHex); err != nil {
		return fail(exitUsage, fmt.Errorf("nonce must be hex: %v", err))
	}
	fp := *machine
	if fp == "" {
		fp = license.MachineFingerprint()
	}
	now := time.Now()
	lic := &license.License{
		Version:   active.ID,
		ShardFrom: from,
		ShardTo:   to,
		NotBefore: now,
		NotAfter:  now.Add(*ttl),
		Nonce:     nonceHex,
		Machine:   fp,
	}
	lic.Signature = license.Sign(cryptox.SigningKey(master), lic)
	if err := lic.Save(*out); err != nil {
		return fail(exitUsage, err)
	}
	printJSON(map[string]any{
		"version": lic.Version,
		"shards":  fmt.Sprintf("%d-%d", from, to),
		"nonce":   lic.Nonce,
		"out":     *out,
	})
	return exitOK
}
