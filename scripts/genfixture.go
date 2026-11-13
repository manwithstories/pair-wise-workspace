// Command genfixture creates deterministic synthetic resources for demos and
// tests. It never reads external fixture data.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultFiles = 3072
	defaultSize  = 32 * 1024
)

type resource struct {
	path string
	size int
}

var noisePercent = 2

func main() {
	fout := flag.String("out", "", "output directory")
	fversion := flag.String("version", "v1", "fixture version: v1 or v2")
	ffiles := flag.Int("files", defaultFiles, "number of common files")
	fsize := flag.Int("size", defaultSize, "common file size in bytes")
	fnoise := flag.Int("noise", 2, "percent of each resource replaced by deterministic high-entropy bytes")
	flag.Parse()
	noisePercent = *fnoise
	if noisePercent < 0 || noisePercent > 100 {
		fmt.Fprintln(os.Stderr, `{"level":"error","code":"usage","message":"--noise must be between 0 and 100"}`)
		os.Exit(2)
	}
	if *fout == "" {
		fmt.Fprintln(os.Stderr, `{"level":"error","code":"usage","message":"--out is required"}`)
		os.Exit(2)
	}
	if *fversion != "v1" && *fversion != "v2" {
		fmt.Fprintln(os.Stderr, `{"level":"error","code":"usage","message":"--version must be v1 or v2"}`)
		os.Exit(2)
	}
	if err := generate(*fout, *fversion, *ffiles, *fsize); err != nil {
		fmt.Fprintf(os.Stderr, "{\"level\":\"error\",\"code\":\"fixture_failed\",\"message\":%q}\n", err.Error())
		os.Exit(1)
	}
}

func generate(out, version string, files, size int) error {
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	resources := fixtureResources(files, size)
	skip := map[string]bool{}
	if version == "v2" {
		// Three deletions make rollback demonstrate file restoration.
		for _, p := range []string{
			"assets/data/level_0999.dat",
			"assets/tex/hero_499.ktx",
			"assets/audio/bgm_119.ogg",
		} {
			skip[p] = true
		}
		// One nightly asset makes rollback demonstrate removal.
		resources = append(resources, resource{path: "assets/nightly/patch_bundle.bin", size: 1_597_000})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].path < resources[j].path })

	modified := map[string]bool{}
	if version == "v2" {
		picked := 0
		for _, r := range resources {
			if skip[r.path] || strings.Contains(r.path, "nightly") {
				continue
			}
			modified[r.path] = true
			picked++
			if picked == 12 {
				break
			}
		}
	}

	for _, r := range resources {
		if skip[r.path] {
			continue
		}
		if err := writeResource(out, r, version, modified[r.path]); err != nil {
			return err
		}
	}
	return nil
}

func fixtureResources(files, size int) []resource {
	if files < 0 {
		files = 0
	}
	var out []resource
	// Names are intentionally spread across four asset classes.
	for i := 0; i < files; i++ {
		var path string
		switch {
		case i < files/6:
			path = fmt.Sprintf("assets/tex/hero_%02d.ktx", i)
		case i < files/3:
			path = fmt.Sprintf("assets/audio/bgm_%02d.ogg", i-files/6)
		case i < files*2/3:
			path = fmt.Sprintf("assets/data/level_%04d.dat", i-files/3)
		default:
			path = fmt.Sprintf("assets/ui/atlas_%04d.bin", i-files*2/3)
		}
		// Stable size variation still totals roughly the requested size.
		sz := size
		if i%97 == 0 {
			sz += 173
		}
		out = append(out, resource{path: path, size: sz})
	}
	return out
}

func writeResource(root string, r resource, version string, modified bool) error {
	full := filepath.Join(root, filepath.FromSlash(r.path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	seed := fnv64(r.path)
	if err := emit(bw, r.path, r.size, seed, version); err != nil {
		_ = f.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	// A stable high-entropy tail prevents synthetic assets from compressing
	// unrealistically well while keeping the fixture reproducible.
	if err := writeNoise(f, r.size, seed^0x9e3779b97f4a7c15); err != nil {
		_ = f.Close()
		return err
	}
	if modified {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			_ = f.Close()
			return err
		}
		extra := make([]byte, 4096)
		x := seed ^ 0xd1b54a32d192ed03
		for i := range extra {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			extra[i] = byte(x) ^ byte(i)
		}
		if _, err := f.Write(extra); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

func emit(w io.Writer, path string, size int, seed uint64, version string) error {
	switch {
	case strings.HasSuffix(path, ".ktx"):
		return emitTexture(w, size, seed)
	case strings.HasSuffix(path, ".ogg"):
		return emitAudio(w, size, seed)
	case strings.HasSuffix(path, ".dat"):
		return emitText(w, size, seed)
	case strings.Contains(path, "nightly"):
		return emitNightly(w, size, seed)
	default:
		return emitBinary(w, size, seed)
	}
}

var words = []string{
	"engine", "sprite", "terrain", "weather", "combat", "audio", "stream", "chunk",
	"archive", "manifest", "texture", "shader", "level", "player", "enemy", "light",
	"shadow", "water", "forest", "village", "quest", "inventory", "dialog", "camera",
	"physics", "motion", "portal", "trigger", "region", "effect", "entity", "system",
}

func emitText(w io.Writer, size int, seed uint64) error {
	bw := bufio.NewWriterSize(w, 32*1024)
	written := 0
	line := 0
	for written < size {
		a := words[int((seed+uint64(line)*7)%uint64(len(words)))]
		b := words[int((seed/3+uint64(line)*13)%uint64(len(words)))]
		c := words[int((seed/7+uint64(line)*29)%uint64(len(words)))]
		text := fmt.Sprintf("%06d %s.%s id=%04d value=%d state=%s\n",
			line, a, b, line%997, (line*37)%100003, c)
		if written+len(text) > size {
			text = text[:size-written]
		}
		if _, err := bw.WriteString(text); err != nil {
			return err
		}
		written += len(text)
		line++
	}
	return bw.Flush()
}

func emitTexture(w io.Writer, size int, seed uint64) error {
	bw := bufio.NewWriterSize(w, 32*1024)
	header := []byte("KTX1GKPK")
	if _, err := bw.Write(header); err != nil {
		return err
	}
	written := len(header)
	var pixel uint32
	for written < size {
		pixel += uint32((seed >> 8) & 0x1f)
		x := pixel & 0xff
		y := (pixel >> 8) & 0x7f
		a := (pixel >> 16) & 0x3f
		value := x | (y << 8) | (a << 16) | (x << 24)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], value)
		n := size - written
		if n > 4 {
			n = 4
		}
		if _, err := bw.Write(b[:n]); err != nil {
			return err
		}
		written += n
	}
	return bw.Flush()
}

func emitAudio(w io.Writer, size int, seed uint64) error {
	bw := bufio.NewWriterSize(w, 32*1024)
	if _, err := bw.WriteString("OggS-GKPK"); err != nil {
		return err
	}
	written := 8
	sample := int16((seed >> 7) & 0x3fff)
	for written < size {
		sample = int16(int32(sample)*15/16 + int32((seed>>uint(written%13))&0x1f) - 15)
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(sample))
		n := size - written
		if n > 2 {
			n = 2
		}
		if _, err := bw.Write(b[:n]); err != nil {
			return err
		}
		written += n
	}
	return bw.Flush()
}

func emitBinary(w io.Writer, size int, seed uint64) error {
	bw := bufio.NewWriterSize(w, 64*1024)
	if _, err := bw.WriteString("GKBIN002"); err != nil {
		return err
	}
	written := 8
	tag := uint32(seed >> 32)
	table := make([]uint32, 256)
	for i := range table {
		table[i] = tag | uint32((seed+uint64(i)*0x9e3779b9)%256)
	}
	for written < size {
		for _, v := range table {
			if written >= size {
				break
			}
			var b [4]byte
			value := v ^ (uint32(written) & 0xff)
			binary.LittleEndian.PutUint32(b[:], value)
			n := size - written
			if n > 4 {
				n = 4
			}
			if _, err := bw.Write(b[:n]); err != nil {
				return err
			}
			written += n
		}
	}
	return bw.Flush()
}

func emitNightly(w io.Writer, size int, seed uint64) error {
	bw := bufio.NewWriterSize(w, 64*1024)
	if _, err := bw.WriteString("GKNIGHT1"); err != nil {
		return err
	}
	written := 8
	cycle := uint32(0)
	for written < size {
		tag := cycle * 2654435761
		for i := 0; i < 256; i++ {
			if written >= size {
				break
			}
			var b [4]byte
			value := tag | uint32((seed+uint64(i)+uint64(cycle)*7)%256)
			binary.LittleEndian.PutUint32(b[:], value^(uint32(written)&0xff))
			n := size - written
			if n > 4 {
				n = 4
			}
			if _, err := bw.Write(b[:n]); err != nil {
				return err
			}
			written += n
		}
		cycle++
	}
	return bw.Flush()
}

func writeNoise(f *os.File, size int, seed uint64) error {
	if size < 64 {
		return nil
	}
	n := size * noisePercent / 100
	if n == 0 {
		return nil
	}
	if n < 16 {
		n = 16
	}
	start := int64(size - n)
	buf := make([]byte, 32*1024)
	x := seed
	for n > 0 {
		chunk := len(buf)
		if chunk > n {
			chunk = n
		}
		for i := 0; i < chunk; i++ {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			buf[i] = byte(x)
		}
		if _, err := f.WriteAt(buf[:chunk], start); err != nil {
			return err
		}
		start += int64(chunk)
		n -= chunk
	}
	return nil
}

func fnv64(s string) uint64 {
	const offset = 1469598103934665603
	const prime = 1099511628211
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}
