package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"tsprobe/internal/fixture"
	"tsprobe/internal/report"
	"tsprobe/internal/slicer"
)

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	in := fs.String("in", "", "input .ts file")
	reportPath := fs.String("report", "", "scan report JSON")
	manifestPath := fs.String("manifest", "", "optional slicer manifest JSON")
	p := fixtureParams(fs)
	if err := fs.Parse(args); err != nil {
		return exitIO
	}
	if *in == "" || *reportPath == "" {
		fmt.Fprintln(os.Stderr, "verify: --in and --report are required")
		return exitIO
	}
	withPlan := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			withPlan = true
		}
	})

	rep, err := readReport(*reportPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return exitIO
	}
	fresh, err := rescan(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return exitIO
	}
	var diffs []string
	diff := func(format string, a ...any) {
		diffs = append(diffs, fmt.Sprintf(format, a...))
	}
	compareReports(rep, fresh, diff)
	if withPlan {
		comparePlan(rep, p.Plan(), diff)
	}
	if d := astitsCheck(*in, rep); d != "" {
		diff("astits: %s", d)
	}
	if len(diffs) > 0 {
		for _, d := range diffs {
			fmt.Fprintln(os.Stderr, "verify mismatch:", d)
		}
		return exitMismatch
	}
	if *manifestPath != "" {
		if err := checkManifest(*manifestPath, rep); err != nil {
			fmt.Fprintln(os.Stderr, "verify manifest:", err)
			return exitManifest
		}
	}
	fmt.Println("verify: all checks passed")
	return exitOK
}

func readReport(path string) (*report.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rep report.Report
	if err := json.NewDecoder(f).Decode(&rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

func rescan(path string) (*report.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return report.Scan(f, path)
}

func pidCounts(rep *report.Report) map[uint16]uint64 {
	m := map[uint16]uint64{}
	for _, pi := range rep.PIDs {
		m[pi.PID] = pi.Packets
	}
	return m
}

// compareReports checks a fresh rescan against the stored report.
func compareReports(stored, fresh *report.Report, diff func(string, ...any)) {
	if stored.Packets != fresh.Packets {
		diff("packets: report=%d rescan=%d", stored.Packets, fresh.Packets)
	}
	if stored.LostPacketsEst != fresh.LostPacketsEst {
		diff("lost_packets_est: report=%d rescan=%d", stored.LostPacketsEst, fresh.LostPacketsEst)
	}
	if stored.CCJumpEvents != fresh.CCJumpEvents {
		diff("cc_jump_events: report=%d rescan=%d", stored.CCJumpEvents, fresh.CCJumpEvents)
	}
	if stored.PATVersionChanges != fresh.PATVersionChanges {
		diff("pat_version_changes: report=%d rescan=%d", stored.PATVersionChanges, fresh.PATVersionChanges)
	}
	a, b := pidCounts(stored), pidCounts(fresh)
	for pid, n := range a {
		if b[pid] != n {
			diff("pid 0x%04X packets: report=%d rescan=%d", pid, n, b[pid])
		}
	}
	for pid, n := range b {
		if a[pid] != n {
			diff("pid 0x%04X packets: report=%d rescan=%d", pid, a[pid], n)
		}
	}
}

// comparePlan checks the report against fixture expectations rebuilt by seed.
func comparePlan(rep *report.Report, plan *fixture.Plan, diff func(string, ...any)) {
	if rep.Packets != plan.Packets {
		diff("packets: report=%d plan=%d", rep.Packets, plan.Packets)
	}
	if rep.LostPacketsEst != plan.LostEst {
		diff("lost_packets_est: report=%d plan=%d", rep.LostPacketsEst, plan.LostEst)
	}
	if rep.CCJumpEvents != plan.JumpEvents {
		diff("cc_jump_events: report=%d plan=%d", rep.CCJumpEvents, plan.JumpEvents)
	}
	if rep.PATVersionChanges != plan.PATChanges {
		diff("pat_version_changes: report=%d plan=%d", rep.PATVersionChanges, plan.PATChanges)
	}
	counts := pidCounts(rep)
	for pid, n := range plan.PerPID {
		if counts[pid] != n {
			diff("pid 0x%04X packets: report=%d plan=%d", pid, counts[pid], n)
		}
	}
	pmtChanges := map[uint16]uint64{}
	for _, pr := range rep.Programs {
		pmtChanges[pr.ProgramNumber] = pr.Versions.Changes
	}
	for prog, n := range plan.PMTChanges {
		if pmtChanges[prog] != n {
			diff("program %d pmt_version_changes: report=%d plan=%d", prog, pmtChanges[prog], n)
		}
	}
}

// checkManifest validates segment files against the manifest.
func checkManifest(path string, rep *report.Report) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var m slicer.Manifest
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		f.Close()
		return err
	}
	f.Close()
	dir := filepath.Dir(path)
	if len(m.Segments) == 0 {
		return fmt.Errorf("manifest has no segments")
	}
	var esSum uint64
	for _, seg := range m.Segments {
		fp := filepath.Join(dir, seg.File)
		st, err := os.Stat(fp)
		if err != nil {
			return err
		}
		if uint64(st.Size()) != seg.Bytes {
			return fmt.Errorf("%s: size %d != manifest %d", seg.File, st.Size(), seg.Bytes)
		}
		var counted uint64
		for pid, n := range seg.PIDCounts {
			counted += n
			if pid != 0 && pid != 0x1FFF && (pid < 0x0100 || pid > 0x01FF) {
				esSum += n
			}
		}
		if counted*188 != seg.Bytes {
			return fmt.Errorf("%s: pid counts %d packets != %d bytes", seg.File, counted, seg.Bytes)
		}
		head := make([]byte, 188)
		sf, err := os.Open(fp)
		if err != nil {
			return err
		}
		_, err = sf.Read(head)
		sf.Close()
		if err != nil {
			return fmt.Errorf("%s: %v", seg.File, err)
		}
		if head[0] != 0x47 || head[1]&0x40 == 0 || head[1]&0x1F != 0 || head[2] != 0 ||
			head[4] != 0 || head[5] != 0x00 {
			return fmt.Errorf("%s: first packet is not a PAT section", seg.File)
		}
	}
	if rep != nil && esSum != rep.ESPackets {
		return fmt.Errorf("segment es_packets sum %d != report %d", esSum, rep.ESPackets)
	}
	return nil
}
