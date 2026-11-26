package encode

import (
	"errors"
	"os"
	"path/filepath"

	astiav "github.com/asticode/go-astiav"

	"mediapipe/internal/media"
)

// RemuxSegmentAtomic stream-copies packets whose PTS lies in [startSec,endSec)
// into an MP4 file. It writes a same-directory temporary file and renames it,
// so callers never observe a partial segment.
func RemuxSegmentAtomic(src, dst string, startSec, endSec float64, targetBytes int64) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, "", media.WrapError(media.CodeIO, "create segment directory", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".segment-*.mp4.tmp")
	if err != nil {
		return 0, "", media.WrapError(media.CodeIO, "create mux temp", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpName) // astiav/ffmpeg creates the IO target itself
	cleanup := func() { _ = os.Remove(tmpName) }

	fc := astiav.AllocFormatContext()
	defer fc.Free()
	if err := fc.OpenInput(src, nil, nil); err != nil {
		cleanup()
		return 0, "", media.WrapError(media.CodeDemux, "open source for remux", err)
	}
	defer fc.CloseInput()
	if err := fc.FindStreamInfo(nil); err != nil {
		cleanup()
		return 0, "", media.WrapError(media.CodeDemux, "find streams for remux", err)
	}
	oc, err := astiav.AllocOutputFormatContext(nil, "mp4", tmpName)
	if err != nil {
		cleanup()
		return 0, "", media.WrapError(media.CodeEncode, "alloc mp4 muxer", err)
	}
	defer oc.Free()
	outByIn := map[int]int{}
	for _, inSt := range fc.Streams() {
		outSt := oc.NewStream(nil)
		if outSt == nil {
			cleanup()
			return 0, "", media.NewError(media.CodeEncode, "alloc output stream", nil)
		}
		if cp := inSt.CodecParameters(); cp != nil {
			if err := cp.Copy(outSt.CodecParameters()); err != nil {
				cleanup()
				return 0, "", media.WrapError(media.CodeEncode, "copy codec parameters", err)
			}
		}
		outSt.SetTimeBase(inSt.TimeBase())
		outByIn[inSt.Index()] = outSt.Index()
	}
	ioCtx, err := astiav.OpenIOContext(tmpName, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	if err != nil {
		cleanup()
		return 0, "", media.WrapError(media.CodeIO, "open mux io", err)
	}
	defer ioCtx.Close()
	oc.SetPb(ioCtx)
	if err := oc.WriteHeader(nil); err != nil {
		cleanup()
		return 0, "", media.WrapError(media.CodeEncode, "write mp4 header", err)
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	muxed := 0
	for {
		if err := fc.ReadFrame(pkt); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				break
			}
			cleanup()
			return 0, "", media.WrapError(media.CodeDemux, "read for remux", err)
		}
		inIdx := pkt.StreamIndex()
		outIdx, ok := outByIn[inIdx]
		if !ok {
			pkt.Unref()
			continue
		}
		tb := fc.Streams()[inIdx].TimeBase()
		sec := float64(pkt.Pts()) * float64(tb.Num()) / float64(tb.Den())
		if sec < startSec || (endSec > startSec && sec >= endSec) {
			pkt.Unref()
			continue
		}
		pkt.SetStreamIndex(outIdx)
		pkt.RescaleTs(tb, oc.Streams()[outIdx].TimeBase())
		if err := oc.WriteInterleavedFrame(pkt); err != nil {
			pkt.Unref()
			cleanup()
			return 0, "", media.WrapError(media.CodeEncode, "write packet", err)
		}
		muxed++
		pkt.Unref()
	}
	if err := oc.WriteTrailer(); err != nil {
		cleanup()
		return 0, "", media.WrapError(media.CodeEncode, "write trailer", err)
	}
	if err := ioCtx.Close(); err != nil {
		cleanup()
		return 0, "", media.WrapError(media.CodeIO, "close mux io", err)
	}
	if muxed == 0 {
		cleanup()
		return 0, "", media.NewError(media.CodeEncode, "remux produced no packets", nil)
	}
	if targetBytes > 0 {
		if _, err := PadFileToBudget(tmpName, targetBytes); err != nil {
			cleanup()
			return 0, "", err
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		cleanup()
		return 0, "", media.WrapError(media.CodeIO, "atomic segment rename", err)
	}
	sum, err := media.FileSHA256(dst)
	if err != nil {
		return 0, "", err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return 0, "", media.WrapError(media.CodeIO, "stat remuxed segment", err)
	}
	return st.Size(), sum, nil
}
