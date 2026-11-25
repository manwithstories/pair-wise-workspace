package pipeline

import "mediad/internal/silence"

func (rn *runner) result(m *Manifest, hash string) *Result {
	if m == nil {
		m = rn.buildManifest(silence.Result{})
	}
	return &Result{
		Manifest: m, ManifestPath: rn.manifestPath, ManifestSHA256: hash,
		JournalPath: rn.journalPath, Journal: rn.journal, Reports: rn.reports,
		RTPDuplicates: rn.flow.RTPDuplicates, PausedReads: rn.pausedReads,
		FirstErrorByte: rn.firstErrorByte,
	}
}
