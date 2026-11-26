// Package pipeline orchestrates demux, mapping, remux, recovery and
// provenance for one relay run.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"relaycheck/internal/clock"
	"relaycheck/internal/demux"
)

// segNameRe accepts only canonical segment names; anything else (including
// path separators and traversal) is rejected.
var segNameRe = regexp.MustCompile(`^seg_([0-9]{6})(?:_dup([0-9]+))?\.m4s$`)

// inputFile is one segment file in the input directory.
type inputFile struct {
	name string
	pos  uint32
	dup  int
	size int64
}

// listInputs enumerates init.mp4 and the segment files of an input
// directory. Non-regular files and non-matching names are ignored.
func listInputs(dir string) (string, []inputFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	var initPath string
	var files []inputFile
	for _, e := range entries {
		name := e.Name()
		if name != filepath.Base(name) {
			return "", nil, fmt.Errorf("path traversal rejected: %q", name)
		}
		info, err := e.Info()
		if err != nil {
			return "", nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if name == "init.mp4" {
			initPath = filepath.Join(dir, name)
			continue
		}
		m := segNameRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		p, _ := strconv.ParseUint(m[1], 10, 32)
		if p == 0 {
			return "", nil, fmt.Errorf("invalid segment position 0: %q", name)
		}
		dup := 0
		if m[2] != "" {
			dup, _ = strconv.Atoi(m[2])
		}
		files = append(files, inputFile{name: name, pos: uint32(p), dup: dup, size: info.Size()})
	}
	if initPath == "" {
		return "", nil, fmt.Errorf("init.mp4 not found in %s", dir)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].pos != files[j].pos {
			return files[i].pos < files[j].pos
		}
		return files[i].dup < files[j].dup
	})
	return initPath, files, nil
}

// inputDigest hashes init.mp4 and every segment file (name, size, content)
// so a resumed run can verify it operates on the same input.
func inputDigest(dir string, files []inputFile) (string, error) {
	h := sha256.New()
	names := append([]string{"init.mp4"}, fileNames(files)...)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s:%d:", name, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileNames(files []inputFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.name
	}
	return out
}

// memBudget bounds the bytes held by in-flight parsed segments.
type memBudget struct {
	mu   sync.Mutex
	cond *sync.Cond
	max  int64
	used int64
}

func newMemBudget(max int64) *memBudget {
	b := &memBudget{max: max}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *memBudget) acquire(n int64) {
	b.mu.Lock()
	for b.used > 0 && b.used+n > b.max {
		b.cond.Wait()
	}
	b.used += n
	b.mu.Unlock()
}

func (b *memBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	b.cond.Broadcast()
	b.mu.Unlock()
}

// parsedFile is the parse result of one segment file.
type parsedFile struct {
	file    inputFile
	seg     *demux.Segment
	samples [][]demux.Sample
	fault   *clock.Fault
	sha     string
	start   time.Time
}

// parseOne reads and parses one segment file.
func parseOne(dir string, f inputFile) *parsedFile {
	pr := &parsedFile{file: f, start: time.Now()}
	data, err := os.ReadFile(filepath.Join(dir, f.name))
	if err != nil {
		fault := clock.NewFault(clock.FaultTruncate, f.pos, 0, 0, err.Error())
		pr.fault = &fault
		return pr
	}
	sum := sha256.Sum256(data)
	pr.sha = hex.EncodeToString(sum[:])
	seg, fault := demux.ParseSegment(data, f.pos)
	if fault != nil {
		pr.fault = fault
		return pr
	}
	samples, fault := seg.ExtractAll(f.pos)
	if fault != nil {
		pr.fault = fault
		return pr
	}
	pr.seg = seg
	pr.samples = samples
	return pr
}

// parser fans parseOne out over a bounded worker pool under a memory
// budget.
type parser struct {
	dir    string
	budget *memBudget
	jobs   chan inputFile
	res    chan *parsedFile
	wg     sync.WaitGroup
}

func newParser(dir string, workers int, budget *memBudget) *parser {
	p := &parser{
		dir:    dir,
		budget: budget,
		jobs:   make(chan inputFile),
		res:    make(chan *parsedFile, workers),
	}
	for w := 0; w < workers; w++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for f := range p.jobs {
				p.budget.acquire(f.size)
				p.res <- parseOne(p.dir, f)
			}
		}()
	}
	return p
}

func (p *parser) submit(files []inputFile) {
	go func() {
		for _, f := range files {
			p.jobs <- f
		}
		close(p.jobs)
		p.wg.Wait()
		close(p.res)
	}()
}

// release returns a parsed file's memory to the budget.
func (p *parser) release(pr *parsedFile) {
	p.budget.release(pr.file.size)
}
