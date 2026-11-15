package segment

import (
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Segment is one parsed segment file together with its on-disk measurements.
type Segment struct {
	Rendition  int    `json:"rendition"`
	Seq        int64  `json:"seq"`
	SourceTS   int64  `json:"source_ts_ms"`
	DeclaredMS int64  `json:"declared_ms"`
	Retransmit int    `json:"retransmit"`
	FileName   string `json:"file_name"`
	Size       int64  `json:"size_bytes"`
	CRC32      uint32 `json:"crc32_ieee"`
	MeasuredMS int64  `json:"measured_ms"`
	ParseOrder int    `json:"-"`
}

// Failure is one non-fatal problem observed while scanning or checking.
type Failure struct {
	Kind      string `json:"kind"`
	File      string `json:"file,omitempty"`
	Detail    string `json:"detail"`
	Rendition int    `json:"rendition,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
}

// ScanResult aggregates everything the directory scan produced.
type ScanResult struct {
	Segments           []Segment
	Failures           []Failure
	DuplicateConflicts int
}

// segmentFileNames lists regular files ending in .ts inside dir.
func segmentFileNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ts") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// ParseName parses a segment file name of the form
//
//	r<rendition>-s<seq>-t<source-ms>-d<duration-ms>[.rt<retry>].ts
//
// Illegal names are reported through the error return so callers can record a
// failure without aborting the whole scan.
func ParseName(name string) (Segment, error) {
	base := strings.TrimSuffix(name, ".ts")
	retry := 0
	if dot := strings.LastIndex(base, ".rt"); dot >= 0 {
		value, err := strconv.Atoi(base[dot+3:])
		if err != nil || value <= 0 {
			return Segment{}, fmt.Errorf("bad retransmit marker: %q", name)
		}
		retry = value
		base = base[:dot]
	}

	parts := strings.Split(base, "-")
	if len(parts) != 4 {
		return Segment{}, fmt.Errorf("want 4 dash fields, got %d: %q", len(parts), name)
	}
	var values [4]int64
	prefixes := [4]byte{'r', 's', 't', 'd'}
	for index, part := range parts {
		if len(part) < 2 || part[0] != prefixes[index] {
			return Segment{}, fmt.Errorf("bad field %d: %q", index+1, name)
		}
		value, err := strconv.ParseInt(part[1:], 10, 64)
		if err != nil {
			return Segment{}, fmt.Errorf("bad number in field %d: %q", index+1, name)
		}
		values[index] = value
	}
	if values[1] < 0 || values[2] < 0 || values[3] <= 0 {
		return Segment{}, fmt.Errorf("negative or zero value in: %q", name)
	}
	if _, ok := ProfileByIndex(int(values[0])); !ok {
		return Segment{}, fmt.Errorf("unknown rendition %d: %q", values[0], name)
	}
	return Segment{
		Rendition:  int(values[0]),
		Seq:        values[1],
		SourceTS:   values[2],
		DeclaredMS: values[3],
		Retransmit: retry,
		FileName:   name,
	}, nil
}

// ScanDir reads dir, parses every .ts file name, hashes the payload and
// deduplicates same-sequence arrivals (last arrival wins). Illegal names and
// unreadable files become failures instead of aborting the run.
func ScanDir(dir string) (ScanResult, error) {
	names, err := segmentFileNames(dir)
	if err != nil {
		return ScanResult{}, err
	}

	type loaded struct {
		seg     Segment
		failure *Failure
		order   int
	}
	results := make([]loaded, len(names))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index] = loadOne(dir, names[index], index)
			}
		}()
	}
	for index := range names {
		jobs <- index
	}
	close(jobs)
	workers.Wait()

	result := ScanResult{Segments: make([]Segment, 0, len(names))}
	type key struct {
		rendition int
		seq       int64
	}
	latest := make(map[key]Segment)
	for _, item := range results {
		if item.failure != nil {
			result.Failures = append(result.Failures, *item.failure)
			continue
		}
		k := key{item.seg.Rendition, item.seg.Seq}
		if _, exists := latest[k]; exists {
			result.DuplicateConflicts++
		}
		latest[k] = item.seg
	}

	kept := make([]Segment, 0, len(latest))
	for _, seg := range latest {
		kept = append(kept, seg)
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].Rendition != kept[j].Rendition {
			return kept[i].Rendition < kept[j].Rendition
		}
		if kept[i].Seq != kept[j].Seq {
			return kept[i].Seq < kept[j].Seq
		}
		return kept[i].ParseOrder < kept[j].ParseOrder
	})
	result.Segments = kept
	return result, nil
}

// loadOne parses one file and reads its metadata. Any panic is converted to a
// failure so the parser can never take the process down.
func loadOne(dir, name string, order int) (out struct {
	seg     Segment
	failure *Failure
	order   int
}) {
	out.order = order
	defer func() {
		if recovered := recover(); recovered != nil {
			out.seg = Segment{}
			out.failure = &Failure{Kind: "panic", File: name, Detail: fmt.Sprint(recovered)}
		}
	}()

	seg, err := ParseName(name)
	if err != nil {
		out.failure = &Failure{Kind: "bad_name", File: name, Detail: err.Error()}
		return out
	}
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil {
		out.failure = &Failure{Kind: "unreadable", File: name, Detail: err.Error(),
			Rendition: seg.Rendition, Seq: seg.Seq}
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		out.failure = &Failure{Kind: "unreadable", File: name, Detail: err.Error(),
			Rendition: seg.Rendition, Seq: seg.Seq}
		return out
	}
	profile, _ := ProfileByIndex(seg.Rendition)
	seg.Size = info.Size()
	seg.CRC32 = crc32.ChecksumIEEE(data)
	seg.MeasuredMS = MeasuredDurationMS(seg.Size, profile.BitrateBps)
	seg.ParseOrder = order
	out.seg = seg
	return out
}

// ErrEmpty is returned when a directory contains no usable segments.
var ErrEmpty = errors.New("no parseable segment files found")
