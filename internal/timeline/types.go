package timeline

// CommonTimescale is the 90 kHz presentation basis used for alignment.
const CommonTimescale uint32 = 90000

// Issue codes recorded during repair.
const (
	IssueGap                  = "sequence_gap"
	IssueOutOfOrder           = "out_of_order"
	IssueDuplicateDigest      = "duplicate_digest"
	IssueDuplicateSequence    = "duplicate_sequence"
	IssueTFDTRegression       = "tfdt_regression"
	IssueRTPContinuityRepair  = "rtp_continuity_repair"
	IssueNewPeriodMissingRTP  = "new_period_rtp_missing"
	IssueNewPeriodRTPConflict = "new_period_rtp_conflict"
	IssueOverflow             = "timestamp_overflow"
	IssueBadTimescale         = "bad_timescale"
)

// RTPEvidence is the pion/rtp-derived corroborating timestamp.
type RTPEvidence struct {
	Present   bool
	Timestamp uint32
	ClockRate uint32
}

// SampleTiming is the timing-relevant slice of one trun entry.
type SampleTiming struct {
	Duration uint32
	CTOffset int32
	Sync     bool
}

// Observation is one unique fragment fed into repair.
type Observation struct {
	ID             string
	Digest         string
	Path           string
	Camera         string
	Representation string
	Sequence       uint32
	HasSequence    bool
	Timescale      uint32
	TFDT           uint64
	Samples        []SampleTiming
	RTP            RTPEvidence
}

// Issue is a structured timing/anomaly record.
type Issue struct {
	Code           string `json:"code"`
	Representation string `json:"representation,omitempty"`
	Sequence       int64  `json:"sequence,omitempty"`
	Missing        int    `json:"missing,omitempty"`
	Message        string `json:"message"`
}

// Duplicate records a digest duplicate that was collapsed.
type Duplicate struct {
	Digest  string
	Kept    string
	Dropped string
}

// Fragment is a repaired, canonically timed fragment.
type Fragment struct {
	Observation
	Period        int
	StartDTS      int64
	EndDTS        int64
	StartPTS90k   int64
	DurationTicks int64
	TimingValid   bool
	RTPRepaired   bool
}

// Result holds repaired fragments and all recorded anomalies.
type Result struct {
	Fragments  []*Fragment
	Issues     []Issue
	Duplicates []Duplicate
	Periods    int
}

func (r *Result) addIssue(code, rep string, seq int64, msg string) {
	r.Issues = append(r.Issues, Issue{Code: code, Representation: rep, Sequence: seq, Message: msg})
}

// Counters returns aggregate anomaly counts by category.
func (r *Result) Counters() (gaps, outOfOrder, dupSeq, regressions, newPeriods, rtpRepaired, overflows int) {
	for _, is := range r.Issues {
		switch is.Code {
		case IssueGap:
			gaps += is.Missing
		case IssueOutOfOrder:
			outOfOrder++
		case IssueDuplicateSequence:
			dupSeq++
		case IssueTFDTRegression:
			regressions++
		case IssueNewPeriodMissingRTP, IssueNewPeriodRTPConflict:
			newPeriods++
		case IssueRTPContinuityRepair:
			rtpRepaired++
		case IssueOverflow, IssueBadTimescale:
			overflows++
		}
	}
	return
}
