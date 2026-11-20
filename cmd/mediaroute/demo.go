package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediaroute/internal/provenance"
	"mediaroute/internal/repair"
	"mediaroute/internal/testkit"
)

// cmdDemo generates a damaged sample stream, recovers it end to end
// and reports damage-location accuracy against ground truth.
func cmdDemo(args []string) (int, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "generator seed")
	outDir := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}
	if *outDir == "" {
		return exitUsage, errors.New("demo requires --out")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return exitUsage, err
	}
	dir, err := resolveDir(*outDir)
	if err != nil {
		return exitUsage, err
	}
	inputPath, err := inDir(dir, "input.rtp")
	if err != nil {
		return exitUsage, err
	}
	outPath, err := inDir(dir, "output.mp4")
	if err != nil {
		return exitUsage, err
	}

	f, err := os.OpenFile(inputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return exitUsage, err
	}
	res, err := testkit.Generate(f, testkit.DefaultOptions(*seed))
	if err != nil {
		f.Close()
		return exitUsage, err
	}
	f.Close()
	truth, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return exitUsage, err
	}
	damagePath, err := inDir(dir, "damage.json")
	if err != nil {
		return exitUsage, err
	}
	if err := os.WriteFile(damagePath, truth, 0o600); err != nil {
		return exitUsage, err
	}

	rr, code, err := execute(execParams{
		inputPath: inputPath, outPath: outPath,
		cfg: repair.Config{Window: 64, DriftPPM: 1000},
	})
	if err != nil {
		return code, err
	}
	locate := int64(rr.report.FirstDamageSeq) - int64(res.DamagedFirstSeq)
	if locate < 0 {
		locate = -locate
	}
	locateStr := "locate_error_packets<=1"
	if !rr.report.HasDamage || locate > 1 {
		locateStr = fmt.Sprintf("locate_error_packets=%d(FAIL)", locate)
	}
	fmt.Printf("packets=%d tracks=%d damaged_first_seq=%d %s index_entries=%d\n",
		res.Packets, res.Tracks, res.DamagedFirstSeq, locateStr, rr.report.IndexEntries)
	if !rr.report.HasDamage || locate > 1 {
		return exitMedia, fmt.Errorf("damage located at seq %d, ground truth %d",
			rr.report.FirstDamageSeq, res.DamagedFirstSeq)
	}
	return exitOK, nil
}

// cmdVerify checks a demo/run output directory: output checksum,
// resume cost, damage-location accuracy and random-access structure.
func cmdVerify(args []string) (int, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory with manifest.json and output.mp4")
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}
	if *dir == "" {
		return exitUsage, errors.New("verify requires --dir")
	}
	d, err := resolveDir(*dir)
	if err != nil {
		return exitUsage, err
	}
	manPath, err := inDir(d, "manifest.json")
	if err != nil {
		return exitUsage, err
	}
	man, err := provenance.Load(manPath)
	if err != nil {
		return exitUsage, err
	}
	mp4s, err := filepath.Glob(filepath.Join(d, "*.mp4"))
	if err != nil || len(mp4s) != 1 {
		return exitUsage, fmt.Errorf("expected exactly one .mp4 in %s, found %d", d, len(mp4s))
	}
	sha, err := sha256File(mp4s[0])
	if err != nil {
		return exitUsage, err
	}
	checksumOK := sha == man.OutputSHA256
	resumeOK := man.ResumeMs < 1000

	locateOK := true
	locateStr := "locate_error_packets<=1"
	truthPath, _ := inDir(d, "damage.json")
	if data, err := os.ReadFile(truthPath); err == nil {
		var truth testkit.Result
		if err := json.Unmarshal(data, &truth); err == nil && truth.DamagedFirstSeq > 0 {
			got := minDamageSeq(man)
			delta := int64(got) - int64(truth.DamagedFirstSeq)
			if delta < 0 {
				delta = -delta
			}
			locateOK = delta <= 1
			if !locateOK {
				locateStr = fmt.Sprintf("locate_error_packets=%d(FAIL)", delta)
			}
		}
	}

	checks, fails, err := randomAccessChecks(mp4s[0], man)
	if err != nil {
		return exitMedia, err
	}
	fmt.Printf("%s resume_ms<1000 checksum_match=%v random_access_checks=%d\n",
		locateStr, checksumOK, checks)
	if !resumeOK {
		fmt.Printf("resume_ms=%d(FAIL: >=1000)\n", man.ResumeMs)
	}
	if !locateOK || !resumeOK || !checksumOK || fails > 0 {
		return exitMedia, fmt.Errorf("verification failed: locate=%v resume=%v checksum=%v ra_failures=%d",
			locateOK, resumeOK, checksumOK, fails)
	}
	return exitOK, nil
}
