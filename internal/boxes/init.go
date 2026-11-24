package boxes

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"

	"bcast/internal/media"
)

func scanInit(path string) (*InitInfo, error) {
	info := &InitInfo{Path: path, Timescales: map[uint32]uint32{}}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, media.Corrupt("scan", "missing_init", path, 0, 0, "init segment not found")
		}
		return nil, media.IO("scan", path, 0, 0, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, media.IO("scan", path, 0, 0, err)
	}
	size := st.Size()
	foundMoov := false
	err = forChildren(f, 0, size, func(bi *mp4.BoxInfo) error {
		if boxType(bi.Type) != "moov" {
			return nil
		}
		foundMoov = true
		payloadStart := int64(bi.Offset) + int64(bi.HeaderSize)
		end := int64(bi.Offset) + int64(bi.Size)
		return parseMoov(f, payloadStart, end, info)
	})
	if err != nil {
		return nil, mapScanError(err, path, 0)
	}
	if !foundMoov || len(info.Timescales) == 0 {
		return nil, media.Corrupt("scan", "missing_moov_or_timescale", path, 0, 0, "init.mp4 contains no usable moov/trak/mdia/mdhd")
	}
	return info, nil
}

func parseMoov(f *os.File, start, end int64, info *InitInfo) error {
	return forChildren(f, start, end, func(bi *mp4.BoxInfo) error {
		if boxType(bi.Type) != "trak" {
			return nil
		}
		trackID, timescale, err := parseTrak(f, bi)
		if err != nil {
			return err
		}
		if trackID != 0 && timescale != 0 {
			info.Timescales[trackID] = timescale
		}
		return nil
	})
}

func parseTrak(f *os.File, trak *mp4.BoxInfo) (uint32, uint32, error) {
	start := int64(trak.Offset) + int64(trak.HeaderSize)
	end := int64(trak.Offset) + int64(trak.Size)
	var trackID, timescale uint32
	err := forChildren(f, start, end, func(bi *mp4.BoxInfo) error {
		switch boxType(bi.Type) {
		case "tkhd":
			buf, boxStart, err := readPayload(f, bi, 1<<20)
			if err != nil {
				return err
			}
			id, perr := parseTKHD(buf)
			if perr != nil {
				return &boxScanError{offset: boxStart + int64(bi.HeaderSize), kind: "invalid_tkhd", err: perr}
			}
			trackID = id
		case "mdia":
			s := int64(bi.Offset) + int64(bi.HeaderSize)
			e := int64(bi.Offset) + int64(bi.Size)
			return forChildren(f, s, e, func(md *mp4.BoxInfo) error {
				if boxType(md.Type) != "mdhd" {
					return nil
				}
				buf, boxStart, err := readPayload(f, md, 1<<20)
				if err != nil {
					return err
				}
				ts, err := parseMDHD(buf)
				if err != nil {
					return &boxScanError{offset: boxStart + int64(md.HeaderSize), kind: "invalid_mdhd", err: err}
				}
				timescale = ts
				return nil
			})
		}
		return nil
	})
	return trackID, timescale, err
}

func parseTKHD(p []byte) (uint32, error) {
	if len(p) < 4 {
		return 0, errors.New("short full box")
	}
	version := p[0]
	if version == 1 {
		if len(p) < 24 {
			return 0, errors.New("short v1 tkhd")
		}
		return beUint32(p[20:24]), nil
	}
	if len(p) < 16 {
		return 0, errors.New("short v0 tkhd")
	}
	return beUint32(p[12:16]), nil
}

func parseMDHD(p []byte) (uint32, error) {
	if len(p) < 4 {
		return 0, errors.New("short full box")
	}
	version := p[0]
	if version == 1 {
		if len(p) < 24 {
			return 0, errors.New("short v1 mdhd")
		}
		return beUint32(p[20:24]), nil
	}
	if len(p) < 16 {
		return 0, errors.New("short v0 mdhd")
	}
	return beUint32(p[12:16]), nil
}

func mapScanError(err error, path string, fragment uint32) error {
	if me, ok := media.AsError(err); ok {
		return me
	}
	var be *boxScanError
	if errors.As(err, &be) {
		return media.Corrupt("scan", be.kind, filepath.Clean(path), firstNonZero(fragment, be.fragment), be.offset, "%v", be.err)
	}
	return media.Corrupt("scan", "read_failure", filepath.Clean(path), fragment, -1, "%v", err)
}

func firstNonZero(a, b uint32) uint32 {
	if a != 0 {
		return a
	}
	return b
}
