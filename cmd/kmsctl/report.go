package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"kmsctl/internal/keys"
	"kmsctl/internal/shard"
)

type versionReport struct {
	ID         string `json:"id"`
	Epoch      int    `json:"epoch"`
	State      string `json:"state"`
	GraceUntil string `json:"grace_until,omitempty"`
}

type report struct {
	Shards    int             `json:"shards"`
	Rotations int             `json:"rotations"`
	KeyIDs    json.RawMessage `json:"key_ids"`
	Versions  []versionReport `json:"versions"`
}

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	pack := fs.String("pack", "", "pack directory from encrypt")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pack == "" {
		return fail(exitUsage, errRequired("--pack"))
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
	boundaries := map[int]string{}
	for k, v := range meta.Boundaries {
		i, err := strconv.Atoi(k)
		if err != nil {
			return fail(exitInternal, fmt.Errorf("meta boundary key %q: %v", k, err))
		}
		boundaries[i] = v
	}
	rep := report{
		Shards:    len(manifest.Shards),
		Rotations: reg.Rotations,
		KeyIDs:    orderedKeyIDs(boundaries),
	}
	for _, v := range reg.Versions {
		vr := versionReport{ID: v.ID, Epoch: v.Epoch, State: string(v.State)}
		if !v.GraceUntil.IsZero() {
			vr.GraceUntil = v.GraceUntil.Format("2006-01-02T15:04:05Z07:00")
		}
		rep.Versions = append(rep.Versions, vr)
	}
	printJSON(rep)
	return exitOK
}
