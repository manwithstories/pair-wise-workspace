package rtp

import (
	"bytes"
	"testing"
)

func TestXORGroupValidation(t *testing.T) {
	rec := NewRecovery()
	if _, err := rec.Recover(Gap{Start: 1, Count: 1}); err == nil {
		t.Fatal("expected failure without block")
	}
	rec.Observe(pkt(1, "abcdef", 0))
	parity, n := XORParity([][]byte{[]byte("abcdef"), []byte("ghijkl")})
	if n != 6 {
		t.Fatalf("parity len=%d", n)
	}
	if err := rec.AddBlock(FECBlock{Base: 1, Count: 3, Lengths: []int{6, 6, 5}, Parity: nil}); err == nil {
		t.Fatal("expected missing parity rejection")
	}
	if err := rec.AddBlock(FECBlock{Base: 1, Count: 3, Lengths: []int{6, 6, 7}, Parity: make([]byte, 6)}); err == nil {
		t.Fatal("expected parity too small")
	}
	// A group where both 2 and 3 are missing cannot repair either member.
	half := make([]byte, 6)
	copy(half, parity)
	if err := rec.AddBlock(FECBlock{Base: 1, Count: 3, Lengths: []int{6, 6, 0}, Parity: half}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Recover(Gap{Start: 3, Count: 1}); err == nil {
		t.Fatal("expected incomplete group failure")
	}
	if rec.Failed < 1 {
		t.Fatalf("failed=%d", rec.Failed)
	}
}

func TestParityDeterministic(t *testing.T) {
	a := []byte{1, 2, 3, 4}
	b := []byte{9, 8}
	p1, n1 := XORParity([][]byte{a, b})
	p2, n2 := XORParity([][]byte{a, b})
	if n1 != n2 || !bytes.Equal(p1, p2) {
		t.Fatal("parity not deterministic")
	}
	if !bytes.Equal(p1, []byte{8, 10, 3, 4}) {
		t.Fatalf("parity=%v", p1)
	}
}
