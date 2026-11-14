package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"
)

// gomp4Containers mirrors the container set using go-mp4 box types so
// the box-count cross-check stays independent of our box package.
var gomp4Containers = map[string]bool{
	"moov": true, "trak": true, "edts": true, "mdia": true,
	"minf": true, "dinf": true, "stbl": true, "dref": true, "stsd": true,
}

func gomp4Count(r io.ReadSeeker, end uint64, depth int) (int, error) {
	if depth > 16 {
		return 0, fmt.Errorf("go-mp4 walk: too deep")
	}
	n := 0
	for {
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return n, err
		}
		if uint64(pos) >= end {
			return n, nil
		}
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			return n, err
		}
		n++
		typ := bi.Type.String()
		if gomp4Containers[typ] {
			child := bi.Offset + bi.HeaderSize
			if typ == "stsd" || typ == "dref" {
				child += 8
			}
			if _, err := r.Seek(int64(child), io.SeekStart); err != nil {
				return n, err
			}
			c, err := gomp4Count(r, bi.Offset+bi.Size, depth+1)
			if err != nil {
				return n, err
			}
			n += c
		}
		if _, err := r.Seek(int64(bi.Offset+bi.Size), io.SeekStart); err != nil {
			return n, err
		}
	}
}

// selftestGoMP4 cross-checks our parser against go-mp4 on normal.mp4.
func selftestGoMP4(dir string) error {
	f, err := os.Open(filepath.Join(dir, "normal.mp4"))
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	total, err := gomp4Count(f, uint64(fi.Size()), 0)
	if err != nil {
		return err
	}
	if err := expectEq(total, 312, "gomp4 boxes"); err != nil {
		return err
	}
	// moov offset via top-level walk
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var moovOff uint64
	for {
		pos, _ := f.Seek(0, io.SeekCurrent)
		if uint64(pos) >= uint64(fi.Size()) {
			break
		}
		bi, err := mp4.ReadBoxInfo(f)
		if err != nil {
			return err
		}
		if bi.Type == mp4.BoxTypeMoov() {
			moovOff = bi.Offset
			break
		}
		if _, err := f.Seek(int64(bi.Offset+bi.Size), io.SeekStart); err != nil {
			return err
		}
	}
	if err := expectEq(moovOff, uint64(48213), "gomp4 moov_offset"); err != nil {
		return err
	}
	// sample table alignment on the first (video) trak
	stszPath := mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()}
	bps, err := mp4.ExtractBoxWithPayload(f, nil, stszPath)
	if err != nil {
		return err
	}
	if len(bps) == 0 {
		return fmt.Errorf("gomp4: stsz not found")
	}
	stsz := bps[0].Payload.(*mp4.Stsz)
	if err := expectEq(int(stsz.SampleCount), 300, "gomp4 samples"); err != nil {
		return err
	}
	stssPath := mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss()}
	bps, err = mp4.ExtractBoxWithPayload(f, nil, stssPath)
	if err != nil {
		return err
	}
	stss := bps[0].Payload.(*mp4.Stss)
	if err := expectEq(len(stss.SampleNumber), 25, "gomp4 keyframes"); err != nil {
		return err
	}
	elstPath := mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeEdts(), mp4.BoxTypeElst()}
	bps, err = mp4.ExtractBoxWithPayload(f, nil, elstPath)
	if err != nil {
		return err
	}
	elst := bps[0].Payload.(*mp4.Elst)
	return expectEq(len(elst.Entries), 1, "gomp4 elst_segments")
}
