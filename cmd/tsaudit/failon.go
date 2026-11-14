package main

import (
	"fmt"
	"strconv"
	"strings"

	"tsaudit/internal/report"
)

type stringsFlag []string

func (s *stringsFlag) String() string { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type condition struct {
	metric string
	op     string
	value  int64
}

func parseFailOn(values []string) ([]condition, error) {
	var out []condition
	for _, group := range values {
		for _, item := range strings.Split(group, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			metric, op, raw := splitCondition(item)
			if !validMetric(metric) {
				return nil, fmt.Errorf("unknown --fail-on metric %q", metric)
			}
			value := int64(0)
			if raw != "" {
				parsed, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("invalid --fail-on threshold %q", raw)
				}
				value = parsed
			}
			out = append(out, condition{metric: metric, op: op, value: value})
		}
	}
	return out, nil
}

func splitCondition(s string) (string, string, string) {
	for _, op := range []string{">=", "<=", "==", ">", "<", "="} {
		if i := strings.Index(s, op); i >= 0 {
			return strings.TrimSpace(s[:i]), op, strings.TrimSpace(s[i+len(op):])
		}
	}
	return s, ">", "0"
}

func validMetric(name string) bool {
	switch name {
	case "all", "cc_jumps", "cc_duplicates", "lost_packets", "pat_version_changes",
		"pmt_version_changes", "conflicts", "warnings", "transport_errors",
		"crc_failures", "sync_errors", "duplicate_sections":
		return true
	}
	return false
}

func triggered(rep *report.Report, conditions []condition) bool {
	for _, c := range conditions {
		if compare(metricValue(rep, c.metric), c.op, c.value) {
			return true
		}
	}
	return false
}

func metricValue(rep *report.Report, name string) int64 {
	switch name {
	case "all":
		if len(rep.Conflicts) > 0 || len(rep.Warnings) > 0 || rep.TransportErrors > 0 ||
			rep.CRCFailures > 0 || rep.SyncErrors > 0 {
			return 1
		}
		return 0
	case "cc_jumps":
		return int64(rep.CCJumps)
	case "cc_duplicates":
		return int64(rep.CCDuplicates)
	case "lost_packets":
		return int64(rep.LostPackets)
	case "pat_version_changes":
		return int64(rep.PATVersionChanges)
	case "pmt_version_changes":
		total := int64(0)
		for _, n := range rep.PMTVersionChanges {
			total += int64(n)
		}
		return total
	case "conflicts":
		return int64(len(rep.Conflicts))
	case "warnings":
		return int64(len(rep.Warnings))
	case "transport_errors":
		return rep.TransportErrors
	case "crc_failures":
		return int64(rep.CRCFailures)
	case "sync_errors":
		return rep.SyncErrors
	case "duplicate_sections":
		return int64(rep.DuplicateSections)
	}
	return 0
}

func compare(left int64, op string, right int64) bool {
	switch op {
	case ">":
		return left > right
	case ">=":
		return left >= right
	case "<":
		return left < right
	case "<=":
		return left <= right
	case "=", "==":
		return left == right
	}
	return false
}
