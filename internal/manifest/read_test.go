package manifest

import (
	"math"
	"strconv"
	"testing"

	"fieldqc/internal/clock"
	"fieldqc/internal/fault"
)

func parseErrors(t *testing.T, json string) Errors {
	t.Helper()
	_, err := Parse([]byte(json))
	if err == nil {
		t.Fatalf("Parse(%s) succeeded, want validation error", json)
	}
	errs, ok := err.(Errors)
	if !ok {
		t.Fatalf("Parse(%s) error type %T, want manifest.Errors: %v", json, err, err)
	}
	return errs
}

func TestParseValidNonZeroStartAndDuration(t *testing.T) {
	m, err := Parse([]byte(`{"assets":[{"id":"card0","tracks":[
		{"id":"cam-a-v","kind":"video","timescale":90000,"duration":180000,"start_dts":9000}
	],"edits":[{"media_time":0,"duration":180000,"rate":1}]}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tr := m.Assets[0].Tracks[0]
	start, err := tr.Start()
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := clock.New(1, 10); start.Cmp(want) != 0 { // 9000/90000 = 0.1 s
		t.Fatalf("start = %s, want 1/10", start)
	}
	dur, err := m.Assets[0].Duration()
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := clock.New(2, 1); dur.Cmp(want) != 0 { // 180000/90000 = 2 s
		t.Fatalf("duration = %s, want 2", dur)
	}
}

func TestParseRejectsAndLocatesEachInvalidEntry(t *testing.T) {
	const track = `{"id":"t","kind":"video","timescale":90000,"duration":90000}`
	cases := []struct {
		name  string
		json  string
		code  fault.Code
		field string
	}{
		{"timescale", `{"assets":[{"id":"a","tracks":[{"id":"t","kind":"video","timescale":0,"duration":1}]}]}`, fault.CodeOverflow, "tracks[0].timescale"},
		{"negative duration", `{"assets":[{"id":"a","tracks":[{"id":"t","kind":"video","timescale":90000,"duration":-3}]}]}`, fault.CodeNegativeDuration, "tracks[0].duration"},
		{"bad kind", `{"assets":[{"id":"a","tracks":[{"id":"t","kind":"subtitle","timescale":90000,"duration":1}]}]}`, fault.CodeInvalidEdit, "tracks[0].kind"},
		{"negative edit media_time", `{"assets":[{"id":"a","tracks":[` + track + `],"edits":[{"media_time":-1,"duration":1,"rate":1}]}]}`, fault.CodeInvalidEdit, "edits[0].media_time"},
		{"zero edit duration", `{"assets":[{"id":"a","tracks":[` + track + `],"edits":[{"media_time":0,"duration":0,"rate":1}]}]}`, fault.CodeNegativeDuration, "edits[0].duration"},
		{"zero edit rate", `{"assets":[{"id":"a","tracks":[` + track + `],"edits":[{"media_time":0,"duration":1,"rate":0}]}]}`, fault.CodeInvalidEdit, "edits[0].rate"},
		{"edit out of range", `{"assets":[{"id":"a","tracks":[` + track + `],"edits":[{"media_time":0,"duration":180000,"rate":1}]}]}`, fault.CodeEditRange, "edits[0]"},
		{"negative keyframe", `{"assets":[{"id":"a","keyframes":[-5]}]}`, fault.CodeInvalidEdit, "keyframes[0]"},
		{"unsorted keyframes", `{"assets":[{"id":"a","keyframes":[10,5]}]}`, fault.CodeInvalidEdit, "keyframes[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := parseErrors(t, tc.json)
			for _, e := range errs {
				if e.Code == tc.code && e.Asset == "a" && e.Field == tc.field {
					return // located exactly
				}
			}
			t.Fatalf("no error with code=%s field=%s asset=a in %+v", tc.code, tc.field, errs)
		})
	}
}

func TestParseEditOverflowRejected(t *testing.T) {
	json := `{"assets":[{"id":"a","edits":[{"media_time":` +
		strconv.FormatInt(math.MaxInt64, 10) + `,"duration":1,"rate":1}]}]}`
	errs := parseErrors(t, json)
	found := false
	for _, e := range errs {
		if e.Code == fault.CodeOverflow && e.Field == "edits[0]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("overflow edit not rejected/located: %+v", errs)
	}
}

func TestParseAggregatesMultipleErrors(t *testing.T) {
	errs := parseErrors(t, `{"assets":[{"id":"","tracks":[
		{"id":"t","kind":"video","timescale":1,"duration":-1},
		{"id":"u","kind":"huh","timescale":1,"duration":1}
	]}]}`)
	if len(errs) < 3 { // empty id + negative duration + bad kind
		t.Fatalf("len(errs)=%d, want >= 3 aggregated: %v", len(errs), errs)
	}
	if errs.Error() == "" {
		t.Fatal("Errors.Error() empty")
	}
}

func TestParseMalformedAndUnknownField(t *testing.T) {
	if _, err := Parse([]byte(`{not json`)); err == nil {
		t.Fatal("malformed JSON accepted")
	} else if _, ok := err.(Errors); ok {
		t.Fatalf("malformed JSON returned manifest.Errors, want wrapped decode error: %v", err)
	}
	if _, err := Parse([]byte(`{"assets":[],"mystery":1}`)); err == nil {
		t.Fatal("unknown field accepted by DisallowUnknownFields")
	}
}
