package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"rtprelay/internal/relay"
	"rtprelay/internal/stats"
)

type SelectedPair struct {
	Local         string `json:"local"`
	Remote        string `json:"remote"`
	LocalAddress  string `json:"local_address"`
	RemoteAddress string `json:"remote_address"`
	LocalPort     int    `json:"local_port"`
	RemotePort    int    `json:"remote_port"`
}

type CandidatePairReport struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

type Report struct {
	Schema             string                `json:"schema"`
	Mode               string                `json:"mode"`
	OK                 bool                  `json:"ok"`
	Error              string                `json:"error,omitempty"`
	DurationMS         int64                 `json:"duration_ms"`
	PPS                int                   `json:"pps"`
	Seed               int64                 `json:"seed"`
	JitterUS           int64                 `json:"jitter_us"`
	Faults             relay.Faults          `json:"faults"`
	SignalVersion      int                   `json:"signal_version"`
	SignalTrickles     uint64                `json:"signal_trickles,omitempty"`
	Gathered           int                   `json:"gathered"`
	Accepted           int                   `json:"accepted"`
	Rejected           int                   `json:"rejected"`
	RejectedCandidates int                   `json:"rejected_candidates"`
	RejectedAddresses  []string              `json:"rejected_addresses,omitempty"`
	PairsEvaluated     int                   `json:"pairs_evaluated"`
	PairsSucceeded     int                   `json:"pairs_succeeded"`
	ProbeCandidates    int                   `json:"probe_candidates,omitempty"`
	CandidatePairs     []CandidatePairReport `json:"candidate_pairs,omitempty"`
	SelectedPair       *SelectedPair         `json:"selected_pair,omitempty"`
	GeneratedPackets   uint64                `json:"generated_packets"`
	ReceivedPackets    uint64                `json:"received_packets"`
	ForwardedPackets   uint64                `json:"forwarded_packets"`
	UDPReceivedPackets uint64                `json:"udp_received_packets"`
	Dropped            uint64                `json:"dropped"`
	Duplicated         uint64                `json:"duplicated"`
	Reordered          uint64                `json:"reordered"`
	Lost               uint64                `json:"lost"`
	OutOfOrder         uint64                `json:"out_of_order"`
	DuplicatePackets   uint64                `json:"duplicate_packets"`
	JitterMeanMs       float64               `json:"jitter_mean_ms"`
	JitterP50Ms        float64               `json:"jitter_p50_ms"`
	JitterP95Ms        float64               `json:"jitter_p95_ms"`
	JitterMaxMs        float64               `json:"jitter_max_ms"`
	MaxJitterMS        float64               `json:"max_jitter_limit_ms,omitempty"`
	DTLSFingerprint    string                `json:"dtls_fingerprint,omitempty"`
	Stats              stats.Summary         `json:"stats"`
}

type Recheck struct {
	Schema       string        `json:"schema"`
	Source       string        `json:"source"`
	Records      uint64        `json:"records"`
	JitterMeanMs float64       `json:"jitter_mean_ms"`
	JitterP50Ms  float64       `json:"jitter_p50_ms"`
	JitterP95Ms  float64       `json:"jitter_p95_ms"`
	JitterMaxMs  float64       `json:"jitter_max_ms"`
	Stats        stats.Summary `json:"stats"`
}

func NewRecheck(source string, records uint64, summary stats.Summary) Recheck {
	return Recheck{
		Schema: "rtprelay.stats.v1", Source: source, Records: records,
		JitterMeanMs: summary.Jitter.MeanMs, JitterP50Ms: summary.Jitter.P50Ms,
		JitterP95Ms: summary.Jitter.P95Ms, JitterMaxMs: summary.Jitter.MaxMs,
		Stats: summary,
	}
}

func (r *Report) SyncStats() {
	r.JitterMeanMs = r.Stats.Jitter.MeanMs
	r.JitterP50Ms = r.Stats.Jitter.P50Ms
	r.JitterP95Ms = r.Stats.Jitter.P95Ms
	r.JitterMaxMs = r.Stats.Jitter.MaxMs
	r.Lost = r.Stats.Lost
	r.OutOfOrder = r.Stats.OutOfOrder
	r.DuplicatePackets = r.Stats.Duplicated
}

func (r Report) Write(path string) error {
	if path == "" {
		return fmt.Errorf("report path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".report-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (r Recheck) Write(path string) error {
	if path == "" {
		return fmt.Errorf("report path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func (r Report) Print(w io.Writer) {
	fmt.Fprintf(w, "%s status=%s duration_ms=%d pps=%d seed=%d\n", r.Mode, status(r.OK), r.DurationMS, r.PPS, r.Seed)
	fmt.Fprintf(w, "gathered=%d accepted=%d rejected=%d pairs_evaluated=%d pairs_succeeded=%d\n",
		r.Gathered, r.Accepted, r.Rejected, r.PairsEvaluated, r.PairsSucceeded)
	if r.SelectedPair != nil {
		fmt.Fprintf(w, "selected_pair=%s <-> %s\n", r.SelectedPair.Local, r.SelectedPair.Remote)
	}
	fmt.Fprintf(w, "generated_packets=%d received_packets=%d forwarded_packets=%d udp_received_packets=%d\n",
		r.GeneratedPackets, r.ReceivedPackets, r.ForwardedPackets, r.UDPReceivedPackets)
	fmt.Fprintf(w, "dropped=%d duplicated=%d reordered=%d lost=%d out_of_order=%d duplicate_packets=%d\n",
		r.Dropped, r.Duplicated, r.Reordered, r.Lost, r.OutOfOrder, r.DuplicatePackets)
	fmt.Fprintf(w, "jitter_ms mean=%.3f p50=%.3f p95=%.3f max=%.3f\n",
		r.JitterMeanMs, r.JitterP50Ms, r.JitterP95Ms, r.JitterMaxMs)
	if !r.OK && r.Error != "" {
		fmt.Fprintf(w, "error=%s\n", r.Error)
	}
}

func (r Recheck) Print(w io.Writer) {
	fmt.Fprintf(w, "stats records=%d packets=%d lost=%d out_of_order=%d duplicated=%d\n",
		r.Records, r.Stats.Received, r.Stats.Lost, r.Stats.OutOfOrder, r.Stats.Duplicated)
	fmt.Fprintf(w, "jitter_ms mean=%.3f p50=%.3f p95=%.3f max=%.3f\n",
		r.JitterMeanMs, r.JitterP50Ms, r.JitterP95Ms, r.JitterMaxMs)
}

func status(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}
