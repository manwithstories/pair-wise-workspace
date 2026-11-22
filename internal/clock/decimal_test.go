package clock

import "testing"

func TestDecimalExact(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		rend string
	}{
		{"2", 180000, "2.000"},
		{"1.96", 176400, "1.960"},
		{"0.5", 45000, "0.500"},
		{"0.00001", 1, "0.000"},
		{"60.000", 5400000, "60.000"},
	}
	for _, tc := range cases {
		got, err := ParseDecimalTicks(tc.in, 90000)
		if err != nil || got != tc.want {
			t.Fatalf("parse %q = %d, %v; want %d", tc.in, got, err, tc.want)
		}
		if s := FormatDecimalTicks(got, 90000, 3); s != tc.rend {
			t.Fatalf("format %d = %q; want %q", got, s, tc.rend)
		}
	}
	for _, bad := range []string{"", "x", "1.2.3", "-", "1e3"} {
		if _, err := ParseDecimalTicks(bad, 90000); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestISODuration(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"PT20S", 1800000},
		{"PT1M2.5S", 5625000},
		{"P1DT0.5S", 7776045000},
		{"PT0S", 0},
	}
	for _, tc := range cases {
		got, err := ParseISODurationTicks(tc.in, 90000)
		if err != nil || got != tc.want {
			t.Fatalf("parse %q = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	if s := FormatISODurationTicks(1800000, 90000); s != "PT20S" {
		t.Fatalf("format = %q", s)
	}
	if s := FormatISODurationTicks(5625000, 90000); s != "PT62.5S" {
		t.Fatalf("format = %q", s)
	}
}
