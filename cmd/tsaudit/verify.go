package main

import (
	"fmt"

	"tsaudit/internal/fixtures"
	"tsaudit/internal/report"
)

type difference struct {
	field    string
	expected any
	actual   any
}

func compareTruth(t *fixtures.Truth, r *report.Report) *difference {
	if t.PacketsTotal != 0 && t.PacketsTotal != r.PacketsTotal {
		return &difference{"packets_total", t.PacketsTotal, r.PacketsTotal}
	}
	if r.PAT == nil {
		return &difference{"pat", t.PAT.VersionChanges, "missing"}
	}
	if t.PAT.VersionChanges != r.PAT.VersionChanges {
		return &difference{"pat.version_changes", t.PAT.VersionChanges, r.PAT.VersionChanges}
	}
	for _, want := range t.PMT {
		var actual *report.PMTReport
		for i := range r.PMTs {
			if int(r.PMTs[i].Program) == want.Program {
				actual = &r.PMTs[i]
				break
			}
		}
		if actual == nil {
			return &difference{fmt.Sprintf("pmt.program[%d]", want.Program), "present", "missing"}
		}
		if want.VersionChanges != actual.VersionChanges {
			return &difference{fmt.Sprintf("pmt.program[%d].version_changes", want.Program),
				want.VersionChanges, actual.VersionChanges}
		}
	}
	if t.Continuity.LossPackets != int(r.Continuity.LossPackets) {
		return &difference{"continuity.loss_packets", t.Continuity.LossPackets, r.Continuity.LossPackets}
	}
	if t.Continuity.CCJumps != int(r.Continuity.CCJumps) {
		return &difference{"continuity.cc_jumps", t.Continuity.CCJumps, r.Continuity.CCJumps}
	}
	if t.Continuity.Duplicates != int(r.Continuity.Duplicates) {
		return &difference{"continuity.duplicates", t.Continuity.Duplicates, r.Continuity.Duplicates}
	}
	if t.CRCErrors != int(r.CRCErrors) {
		return &difference{"crc_errors", t.CRCErrors, r.CRCErrors}
	}
	if t.OrphanPMTs != nil && !sameOrphans(t.OrphanPMTs, r.OrphanPMTs) {
		return &difference{"orphan_pmts", t.OrphanPMTs, r.OrphanPMTs}
	}
	return nil
}

func sameOrphans(a []fixtures.OrphanPMTTruth, b []report.OrphanPMT) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Program != b[i].Program || a[i].PID != b[i].PID {
			return false
		}
	}
	return true
}
