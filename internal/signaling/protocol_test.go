package signaling

import "testing"

func TestDecodeRejectsBadEnvelopes(t *testing.T) {
	cases := []struct {
		raw  string
		code string
	}{
		{``, ErrCodeInvalidJSON},
		{`{"type":"nope","seq":1}`, ErrCodeUnknownType},
		{`{"type":"offer","seq":1}`, ErrCodeEmptyPayload},
		{`{"type":"answer","seq":1,"sdp":"x"}`, ErrCodeMissingSession},
		{`{"type":"candidate","seq":1,"session":"s"}`, ErrCodeEmptyPayload},
		{`{"type":"candidate","seq":1,"candidate":"c"}`, ErrCodeMissingSession},
	}
	for _, tc := range cases {
		_, err := Decode([]byte(tc.raw))
		if ErrCode(err) != tc.code {
			t.Fatalf("Decode(%q): want %s, got %v", tc.raw, tc.code, err)
		}
	}
}

func TestIsLoopbackCandidate(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"candidate:1 1 udp 100 127.0.0.1 5000 typ host", true},
		{"1 1 udp 100 127.0.0.2 5000 typ host", true},
		{"candidate:1 1 udp 100 ::1 5000 typ host", true},
		{"candidate:1 1 udp 100 [::1] 5000 typ host", true},
		{"candidate:1 1 udp 100 10.0.0.1 5000 typ host", false},
		{"candidate:1 1 udp 100 192.168.1.9 5000 typ host", false},
		{"candidate:1 1 udp 100 2001:db8::1 5000 typ host", false},
		{"candidate:1 udp 100 127.0.0.1 typ host", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		got, detail := IsLoopbackCandidate(tc.in)
		if got != tc.want {
			t.Fatalf("IsLoopbackCandidate(%q)=%v want %v (detail %q)", tc.in, got, tc.want, detail)
		}
		if !got && detail == "" {
			t.Fatalf("a rejected candidate must carry a reason: %q", tc.in)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	orig := Message{Type: KindCandidate, Seq: 9, Session: "abc", Candidate: "candidate:1 1 udp 1 127.0.0.1 1 typ host"}
	raw, err := Encode(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != orig {
		t.Fatalf("round trip changed the message: %+v vs %+v", got, orig)
	}
}
