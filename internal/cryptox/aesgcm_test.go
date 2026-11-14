package cryptox

import (
	"errors"
	"testing"
)

func TestSealOpenAndTamper(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	nonce := DeriveNonce(2, 7)
	if len(nonce) != 12 {
		t.Fatalf("nonce size %d", len(nonce))
	}
	aad := BuildAAD(7, "k-0003", "digest")
	sealed, err := Seal(key, nonce, []byte("segment payload"), aad)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Open(key, nonce, sealed, aad, 7, "k-0003", "digest")
	if err != nil || string(pt) != "segment payload" {
		t.Fatalf("open failed: %v %q", err, pt)
	}
	// 密文改一字节 -> TAG_INVALID
	bad := append([]byte(nil), sealed...)
	bad[0] ^= 0xff
	if _, err := Open(key, nonce, bad, aad, 7, "k-0003", "digest"); !errors.Is(err, ErrTagInvalid) {
		t.Fatalf("want TAG_INVALID got %v", err)
	}
	// key_id 不符 -> AAD_MISMATCH
	if _, err := Open(key, nonce, sealed, aad, 7, "k-0009", "digest"); !errors.Is(err, ErrAADMismatch) {
		t.Fatalf("want AAD_MISMATCH got %v", err)
	}
	// 段换位（nonce 段号与期望不一致）-> AAD_MISMATCH
	if _, err := Open(key, nonce, sealed, aad, 8, "k-0003", "digest"); !errors.Is(err, ErrAADMismatch) {
		t.Fatalf("want AAD_MISMATCH on swap got %v", err)
	}
}
