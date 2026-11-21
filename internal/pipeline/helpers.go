package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"

	"mediad/internal/model"
)

type hasher struct{ h hash.Hash }

func newHash() *hasher { return &hasher{h: sha256.New()} }

func (h *hasher) Write(p []byte) (int, error) { return h.h.Write(p) }

func (h *hasher) Hex() string { return hex.EncodeToString(h.h.Sum(nil)) }

func asModel(err error, out **model.Error) bool {
	return errors.As(err, out)
}
