package transport

import (
	"bytes"
	"fmt"

	"pairflow/internal/model"
)

// FECBlock is an XOR-protected set of equal-size media shards.
type FECBlock struct {
	Group     int
	Sequences []uint16
	ShardSize int
	Parity    []byte
	ParitySeq uint16
}

// BuildXORParity returns byte-wise XOR of all shards. Shards must have equal
// length; generated proof traffic uses fixed-size payloads.
func BuildXORParity(shards [][]byte) ([]byte, error) {
	if len(shards) == 0 {
		return nil, fmt.Errorf("cannot build parity for empty block")
	}
	size := len(shards[0])
	parity := make([]byte, size)
	for i, shard := range shards {
		if len(shard) != size {
			return nil, fmt.Errorf("shard %d size %d != first size %d", i, len(shard), size)
		}
		for j := range parity {
			parity[j] ^= shard[j]
		}
	}
	return parity, nil
}

// NewFECBlock builds parity for sequences and their payloads.
func NewFECBlock(group int, sequences []uint16, shards [][]byte, paritySeq uint16) (*FECBlock, error) {
	if len(sequences) != len(shards) {
		return nil, fmt.Errorf("sequences/shards length mismatch")
	}
	parity, err := BuildXORParity(shards)
	if err != nil {
		return nil, err
	}
	return &FECBlock{Group: group, Sequences: append([]uint16(nil), sequences...), ShardSize: len(parity), Parity: parity, ParitySeq: paritySeq}, nil
}

// RecoverXOR reconstructs exactly one missing shard. known must contain all
// other media shards and parity. If zero or more than one media shard is
// missing, it returns a burst-loss error carrying the missing sequences.
func (b *FECBlock) RecoverXOR(known map[uint16][]byte) ([]byte, *model.MediaError) {
	var missing []uint16
	for _, seq := range b.Sequences {
		if _, ok := known[seq]; !ok {
			missing = append(missing, seq)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	if len(missing) > 1 {
		me := &model.MediaError{
			Code:        model.CodeBurstLoss,
			Message:     fmt.Sprintf("XOR FEC cannot recover %d missing shards", len(missing)),
			Recoverable: false,
		}
		if len(missing) > 0 {
			me.Impact.StartSeq = int64(missing[0])
			me.Impact.EndSeq = int64(missing[len(missing)-1])
		}
		return nil, me
	}

	parity, ok := known[b.ParitySeq]
	if !ok {
		return nil, &model.MediaError{Code: model.CodeBurstLoss, Message: "missing FEC parity shard", Recoverable: false}
	}
	if len(parity) != b.ShardSize {
		return nil, &model.MediaError{Code: model.CodeSeqConflict, Message: "FEC parity size mismatch", Recoverable: false}
	}
	recovered := append([]byte(nil), parity...)
	for _, seq := range b.Sequences {
		shard, ok := known[seq]
		if !ok {
			continue
		}
		if len(shard) != b.ShardSize {
			return nil, &model.MediaError{Code: model.CodeSeqConflict, Message: "FEC shard size mismatch", Recoverable: false}
		}
		for j := range recovered {
			recovered[j] ^= shard[j]
		}
	}
	return recovered, nil
}

// VerifyXOR confirms parity against complete shards.
func (b *FECBlock) VerifyXOR(shards [][]byte) bool {
	parity, err := BuildXORParity(shards)
	if err != nil {
		return false
	}
	return bytes.Equal(parity, b.Parity)
}
