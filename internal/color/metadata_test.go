package color

import (
	"strings"
	"testing"

	"mediapack/internal/fault"
)

func src(id, tag string, p, tr, m uint8, full bool) Source {
	return Source{ID: id, Tag: tag, Triplet: Triplet{Primaries: p, Transfer: tr, Matrix: m, FullRange: full}}
}

func TestNormalizeNamedType(t *testing.T) {
	got, _ := Normalize(Source{ID: "cam", Triplet: Triplet{OriginalType: "BT2020PQ"}})
	if got.Primaries != 9 || got.Transfer != 16 || got.Matrix != 9 {
		t.Fatalf("bt2020pq normalized to %+v", got)
	}
	if got.String() != "bt2020/pq/bt2020nc/limited" {
		t.Fatalf("String=%q", got.String())
	}
}

func TestNormalizeUnknownTypePreservesToken(t *testing.T) {
	got, note := Normalize(Source{ID: "cam", Triplet: Triplet{OriginalType: "dolby-vision-mk2"}})
	if !got.Unknown || got.OriginalType != "dolby-vision-mk2" {
		t.Fatalf("unknown type lost: %+v", got)
	}
	if !strings.Contains(note, "dolby-vision-mk2") {
		t.Fatalf("note did not preserve token: %q", note)
	}
	if got.String() != "unknown(dolby-vision-mk2)" {
		t.Fatalf("String=%q", got.String())
	}
}

func TestNormalizeUnspecifiedCICP(t *testing.T) {
	// 255 is not a known CICP enumeration; it collapses to unspecified with a
	// note, never a silent pass-through.
	got, note := Normalize(Source{ID: "cam", Triplet: Triplet{Primaries: 255, Transfer: 1, Matrix: 1}})
	if got.Primaries != 2 || !strings.Contains(note, "primaries 255") {
		t.Fatalf("got %+v note=%q", got, note)
	}
}

func TestConsistentMetadataResolves(t *testing.T) {
	res, err := Resolve([]Source{
		src("cam-a", "nclx", 1, 1, 1, false),
		src("cam-b", "colr", 1, 1, 1, false),
	}, Config{}, DefaultLimits())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", res.Conflicts)
	}
	if res.Triplet.String() != "bt709/bt709/bt709/limited" {
		t.Fatalf("triplet=%s", res.Triplet)
	}
}

func TestConflictBlocksByDefaultAndKeepsBothSources(t *testing.T) {
	_, err := Resolve([]Source{
		src("cam-a", "nclx", 1, 1, 1, false),
		src("cam-b", "nclx", 9, 16, 9, false),
	}, Config{}, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Class != fault.ClassDependency || fe.Code != "color-conflict" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(fe.Msg, "bt709") || !strings.Contains(fe.Msg, "bt2020") {
		t.Fatalf("conflict message lost a source: %s", fe.Msg)
	}
	if fe.BlockedBy != "color:cam-b" {
		t.Fatalf("blocked_by=%q", fe.BlockedBy)
	}
}

func TestPreferKeepsConflictRecord(t *testing.T) {
	res, err := Resolve([]Source{
		src("cam-a", "nclx", 1, 1, 1, false),
		src("cam-b", "nclx", 9, 16, 9, false),
	}, Config{Policy: PolicyPrefer, PreferSource: "cam-b"}, DefaultLimits())
	if err != nil {
		t.Fatalf("prefer: %v", err)
	}
	if res.Source != "cam-b" || res.Triplet.Primaries != 9 {
		t.Fatalf("wrong preference: %+v", res)
	}
	if len(res.Conflicts) != 3 {
		t.Fatalf("expected primaries/transfer/matrix conflicts, got %+v", res.Conflicts)
	}
	for _, c := range res.Conflicts {
		if !c.Chosen || c.Resolved.Source != "cam-b" {
			t.Fatalf("conflict not resolved to preferred source: %+v", c)
		}
		if len(c.Values) != 2 {
			t.Fatalf("conflict %s lost a value: %+v", c.Field, c.Values)
		}
	}
}

func TestPreferMissingSourceIsUsageError(t *testing.T) {
	_, err := Resolve([]Source{
		src("cam-a", "nclx", 1, 1, 1, false),
		src("cam-b", "nclx", 9, 16, 9, false),
	}, Config{Policy: PolicyPrefer, PreferSource: "ghost"}, DefaultLimits())
	if fault.ClassOf(err) != fault.ClassUsage || fault.ExitCode(err) != 1 {
		t.Fatalf("got %v exit=%d", err, fault.ExitCode(err))
	}
}

func TestFullRangeConflictDetected(t *testing.T) {
	_, err := Resolve([]Source{
		src("cam-a", "nclx", 1, 1, 1, false),
		src("cam-b", "nclx", 1, 1, 1, true),
	}, Config{}, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || !strings.Contains(fe.Msg, "full_range") {
		t.Fatalf("got %v", err)
	}
}

func TestResolveLimitsAndValidation(t *testing.T) {
	if _, err := Resolve(nil, Config{}, DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("empty: %v", err)
	}
	three := []Source{src("a", "nclx", 1, 1, 1, false), src("b", "nclx", 1, 1, 1, false)}
	if _, err := Resolve(three, Config{}, Limits{MaxSources: 1}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("limit: %v", err)
	}
	if _, err := Resolve([]Source{{ID: "", Triplet: Triplet{Primaries: 1}}}, Config{}, DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("empty id: %v", err)
	}
	if _, err := Resolve([]Source{src("a", "nclx", 1, 1, 1, false)}, Config{Policy: "nope"}, DefaultLimits()); fault.ClassOf(err) != fault.ClassUsage {
		t.Fatalf("bad policy: %v", err)
	}
}

func TestNormalizeUnknownCICPValue(t *testing.T) {
	trip := Triplet{Primaries: 200, Transfer: 1, Matrix: 1}
	got, note := Normalize(Source{ID: "cam", Triplet: trip})
	if got.Primaries != 2 {
		t.Fatalf("unknown primaries not normalized: %+v", got)
	}
	if !strings.Contains(note, "primaries 200") {
		t.Fatalf("note=%q", note)
	}
	if NameOf("primaries", 1) != "bt709" || NameOf("matrix", 0) != "rgb" {
		t.Fatalf("name tables broken")
	}
	if NameOf("matrix", 199) != "cicp-199" {
		t.Fatalf("fallback name=%q", NameOf("matrix", 199))
	}
}
