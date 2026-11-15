package ice

import "testing"

func TestLoopbackValidation(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "127.1.2.3", "::1", "[::1]"} {
		if !IsLoopback(address) {
			t.Fatalf("%s should be loopback", address)
		}
	}
	for _, address := range []string{"10.0.0.5", "192.168.1.1", "203.0.113.1", ""} {
		if IsLoopback(address) {
			t.Fatalf("%q should not be loopback", address)
		}
	}
	if _, err := Parse("10.0.0.5:40000"); err == nil {
		t.Fatal("non-loopback candidate should be rejected")
	}
}

func TestEnumerateLoopbackPairs(t *testing.T) {
	left := []Candidate{
		{Address: "127.0.0.1", Port: 1000, Protocol: "udp"},
		{Address: "127.0.0.1", Port: 1001, Protocol: "udp"},
	}
	right := []Candidate{
		{Address: "127.0.0.1", Port: 2000, Protocol: "udp"},
		{Address: "127.0.0.1", Port: 2001, Protocol: "udp"},
	}
	if pairs := EnumeratePairs(left, right); len(pairs) != 4 {
		t.Fatalf("pairs = %d, want 4", len(pairs))
	}
}
