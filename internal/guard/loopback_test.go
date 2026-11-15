package guard

import (
	"errors"
	"testing"
)

func TestValidateListenAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:47000", "127.0.0.1:0"} {
		if err := ValidateListenAddress(address); err != nil {
			t.Fatalf("expected %s to pass: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:47000", "[::1]:47000", "localhost:47000", "127.8.9.10:1"} {
		if err := ValidateListenAddress(address); !errors.Is(err, ErrNonLoopback) {
			t.Fatalf("expected %s to be rejected, got %v", address, err)
		}
	}
	if err := ValidateListenAddress("127.0.0.1:0"); err != nil {
		t.Fatalf("ephemeral loopback port should be allowed: %v", err)
	}
}

func TestValidateCandidate(t *testing.T) {
	if err := ValidateCandidate("candidate:1 1 udp 2130706431 127.0.0.1 50000 typ host"); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{
		"candidate:1 1 udp 2130706431 192.168.1.2 50000 typ host",
		"candidate:1 1 udp 2130706431 abcd.local 50000 typ host",
		"candidate:1 1 udp 2130706431 2001:db8::1 50000 typ host",
	} {
		if err := ValidateCandidate(candidate); !errors.Is(err, ErrNonLoopback) {
			t.Fatalf("expected candidate to be rejected: %s", candidate)
		}
	}
}

func TestValidateSDP(t *testing.T) {
	valid := "v=0\r\na=candidate:1 1 udp 1 127.0.0.1 50000 typ host\r\n"
	if err := ValidateSDP(valid); err != nil {
		t.Fatal(err)
	}
	invalid := "v=0\r\na=candidate:1 1 udp 1 10.0.0.2 50000 typ host\r\n"
	if err := ValidateSDP(invalid); !errors.Is(err, ErrNonLoopback) {
		t.Fatalf("expected SDP rejection, got %v", err)
	}
}
