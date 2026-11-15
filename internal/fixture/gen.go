package fixture

import (
	"fmt"
	"os"
	"path/filepath"
)

// Config controls sample data generation.
type Config struct {
	OutDir     string
	Renditions int
	Segments   int
	Drop       int
	Duplicates int
	Bad        int
	Shuffle    bool
	Seed       uint64
	SlotMS     int64
}

// rng is a tiny xorshift64* PRNG so the fixture needs no external randomness
// source and stays fully reproducible from the seed.
type rng struct {
	state uint64
}

func newRNG(seed uint64) *rng {
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	return &rng{state: seed}
}

func (r *rng) next() uint64 {
	r.state ^= r.state >> 12
	r.state ^= r.state << 25
	r.state ^= r.state >> 27
	return r.state * 0x2545F4914F6CDD1D
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// sample returns k unique integers from [0, n).
func sample(r *rng, n, k int) []int {
	if k > n {
		k = n
	}
	chosen := make(map[int]struct{}, k)
	for len(chosen) < k {
		chosen[r.intn(n)] = struct{}{}
	}
	out := make([]int, 0, k)
	for value := range chosen {
		out = append(out, value)
	}
	for i := len(out) - 1; i > 0; i-- {
		j := r.intn(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

type fileTask struct {
	name string
	size int64
}

// Generate writes the sample directory described by config and returns the
// total number of files written (including retransmissions).
func Generate(config Config) (int, error) {
	if config.Segments <= 0 || config.Renditions <= 0 || config.SlotMS <= 0 {
		return 0, fmt.Errorf("segments, renditions and slot duration must be positive")
	}
	if config.Renditions > 4 {
		return 0, fmt.Errorf("at most 4 renditions supported")
	}
	if err := os.MkdirAll(config.OutDir, 0o755); err != nil {
		return 0, err
	}
	r := newRNG(config.Seed)
	dropSet := make(map[int]struct{}, config.Drop)
	// Keep seq 0 present on every rendition so the timeline base timestamp is 0.
	for len(dropSet) < config.Drop {
		candidate := r.intn(config.Segments-1) + 1
		dropSet[candidate] = struct{}{}
	}
	dupSeqs := sample(r, config.Segments, config.Duplicates)
	badSeqs := sample(r, config.Segments, config.Bad)
	badSet := make(map[int]struct{}, len(badSeqs))
	for _, seq := range badSeqs {
		badSet[seq] = struct{}{}
	}

	profiles := fixtureProfiles(config.Renditions)
	var tasks []fileTask
	for rendition, profile := range profiles {
		for seq := 0; seq < config.Segments; seq++ {
			if _, ok := dropSet[seq]; ok {
				continue
			}
			sourceTS := int64(seq) * config.SlotMS
			jitter := int64(r.intn(77)) - 38 // [-38, 38] milliseconds
			if seq == 0 {
				jitter = 38 // guarantee a deterministic maximum drift
			}
			actualMS := config.SlotMS + jitter
			size := actualMS * int64(profile.bitrateBps) / 8000
			if _, bad := badSet[seq]; bad {
				if r.intn(2) == 0 {
					size = 0
				} else {
					size = size / 3
				}
			}
			name := fmt.Sprintf("r%d-s%06d-t%013d-d%d.ts",
				rendition, seq, sourceTS, config.SlotMS)
			tasks = append(tasks, fileTask{name: name, size: size})
			for _, dupSeq := range dupSeqs {
				if dupSeq != seq {
					continue
				}
				retrySize := (config.SlotMS + int64(r.intn(31)-15)) *
					int64(profile.bitrateBps) / 8000
				retryName := fmt.Sprintf("r%d-s%06d-t%013d-d%d.rt%d.ts",
					rendition, seq, sourceTS, config.SlotMS, r.intn(3)+2)
				tasks = append(tasks, fileTask{name: retryName, size: retrySize})
			}
		}
	}

	if config.Shuffle {
		shuffle(r, tasks)
	}
	for _, task := range tasks {
		if err := writeSegment(filepath.Join(config.OutDir, task.name), task.size); err != nil {
			return 0, err
		}
	}
	return len(tasks), nil
}

type fixtureProfile struct {
	bitrateBps int
}

func fixtureProfiles(renditions int) []fixtureProfile {
	all := []fixtureProfile{
		{400000}, {800000}, {1600000}, {3200000},
	}
	return all[:renditions]
}

func shuffle(r *rng, tasks []fileTask) {
	for i := len(tasks) - 1; i > 0; i-- {
		j := r.intn(i + 1)
		tasks[i], tasks[j] = tasks[j], tasks[i]
	}
}

// writeSegment fills the file with a deterministic, non-repeating byte pattern
// so zero-byte and truncated segments remain visible to the verifier.
func writeSegment(path string, size int64) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, 4096)
	var word uint64 = 0xD1B54A32D192ED03
	remaining := size
	for remaining > 0 {
		for offset := 0; offset < len(buffer); offset += 8 {
			word = rotateLeft64(word, 7) ^ uint64(offset)
			chunk := buffer[offset:]
			if len(chunk) > 8 {
				chunk = chunk[:8]
			}
			for bit := 0; bit < len(chunk); bit++ {
				chunk[bit] = byte(word >> (bit * 8))
			}
		}
		writeSize := int64(len(buffer))
		if writeSize > remaining {
			writeSize = remaining
		}
		if _, err := file.Write(buffer[:writeSize]); err != nil {
			return err
		}
		remaining -= writeSize
	}
	return file.Sync()
}

// rotateLeft64 is a dependency-free 64-bit left rotation (replaces math/bits
// so the module stays within the agreed standard-library allowlist).
func rotateLeft64(value uint64, shift uint) uint64 {
	return (value << shift) | (value >> (64 - shift))
}
