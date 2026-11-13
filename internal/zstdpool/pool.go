package zstdpool

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	MaxDictionarySize = 64 * 1024
	MaxTrainSamples   = 16 * 1024 * 1024
)

// DictMode describes how Dict bytes must be installed in a codec.
type DictMode string

const (
	DictNone DictMode = ""
	DictFull DictMode = "zstd"
	DictRaw  DictMode = "raw"
)

// Dict is a version-scoped compression dictionary.
type Dict struct {
	Data []byte
	Mode DictMode
	ID   uint32
}

// TrainDict builds a zstd dictionary from deterministic sample blocks. If the
// sample set is too small for zstd's trainer, a bounded raw dictionary is used.
func TrainDict(samples [][]byte) (Dict, error) {
	clean := make([][]byte, 0, len(samples))
	var total int
	for _, sample := range samples {
		if len(sample) == 0 {
			continue
		}
		if total+len(sample) > MaxTrainSamples {
			remain := MaxTrainSamples - total
			if remain <= 0 {
				break
			}
			sample = sample[:remain]
		}
		clean = append(clean, sample)
		total += len(sample)
	}
	if total < 8*1024 || len(clean) < 8 {
		return rawDict(clean), nil
	}
	dict, err := zstd.BuildDict(zstd.BuildDictOptions{
		Contents: clean,
		Level:    zstd.SpeedDefault,
	})
	if err != nil || len(dict) == 0 {
		return rawDict(clean), nil
	}
	if len(dict) > MaxDictionarySize {
		return rawDict(clean), nil
	}
	return Dict{Data: dict, Mode: DictFull, ID: dictionaryID(dict)}, nil
}

func rawDict(samples [][]byte) Dict {
	var data []byte
	for _, sample := range samples {
		remain := MaxDictionarySize - len(data)
		if remain <= 0 {
			break
		}
		if len(sample) > remain {
			sample = sample[:remain]
		}
		data = append(data, sample...)
	}
	if len(data) == 0 {
		return Dict{Mode: DictNone}
	}
	return Dict{Data: data, Mode: DictRaw, ID: dictionaryID(data)}
}

func dictionaryID(data []byte) uint32 {
	sum := sha256.Sum256(data)
	id := binary.LittleEndian.Uint32(sum[:4])
	if id == 0 {
		id = 1
	}
	return id
}

// Pool reuses zstd encoders and decoders. A codec is checked out for each call,
// so workers never share an encoder or decoder instance.
type Pool struct {
	encPool sync.Pool
	decPool sync.Pool
}

// New validates the dictionary and constructs a reusable codec pool.
func New(dict Dict) (*Pool, error) {
	opts, err := encoderOptions(dict)
	if err != nil {
		return nil, err
	}
	dopts, err := decoderOptions(dict)
	if err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, dopts...)
	if err != nil {
		enc.Close()
		return nil, err
	}
	p := &Pool{}
	p.encPool.New = func() any {
		e, err := zstd.NewWriter(nil, opts...)
		if err != nil {
			panic(err)
		}
		return e
	}
	p.decPool.New = func() any {
		d, err := zstd.NewReader(nil, dopts...)
		if err != nil {
			panic(err)
		}
		return d
	}
	p.encPool.Put(enc)
	p.decPool.Put(dec)
	return p, nil
}

// Compress encodes src with the version dictionary.
func (p *Pool) Compress(src []byte) ([]byte, error) {
	enc := p.encPool.Get().(*zstd.Encoder)
	defer p.encPool.Put(enc)
	return enc.EncodeAll(src, nil), nil
}

// Decompress decodes src and enforces a bounded expected output size.
func (p *Pool) Decompress(src []byte, expected int) ([]byte, error) {
	dec := p.decPool.Get().(*zstd.Decoder)
	defer p.decPool.Put(dec)
	out, err := dec.DecodeAll(src, make([]byte, 0, expected))
	if err != nil {
		return nil, fmt.Errorf("zstd decode: %w", err)
	}
	if len(out) != expected {
		return nil, fmt.Errorf("zstd decoded size %d, want %d", len(out), expected)
	}
	return out, nil
}

func encoderOptions(dict Dict) ([]zstd.EOption, error) {
	opts := []zstd.EOption{
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderLevel(zstd.SpeedDefault),
	}
	switch dict.Mode {
	case DictNone:
	case DictFull:
		opts = append(opts, zstd.WithEncoderDict(dict.Data))
	case DictRaw:
		opts = append(opts, zstd.WithEncoderDictRaw(dict.ID, dict.Data))
	default:
		return nil, errors.New("unknown dictionary mode")
	}
	return opts, nil
}

func decoderOptions(dict Dict) ([]zstd.DOption, error) {
	opts := []zstd.DOption{
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(512 << 20),
	}
	switch dict.Mode {
	case DictNone:
	case DictFull:
		opts = append(opts, zstd.WithDecoderDicts(dict.Data))
	case DictRaw:
		opts = append(opts, zstd.WithDecoderDictRaw(dict.ID, dict.Data))
	default:
		return nil, errors.New("unknown dictionary mode")
	}
	return opts, nil
}
