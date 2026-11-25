// Package manifest 解析本地 HLS m3u8 与 DASH MPD 清单并做双清单互查。
// 所有时间均换算为 model.Rat 纳秒时间轴，不做浮点累加。
package manifest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"relaycheck/internal/model"
)

// Playlist 是一份媒体播放清单的解析结果。
type Playlist struct {
	Kind     string // "hls" 或 "dash"
	Seq0     int    // 首个分片的媒体序号
	Refs     []model.SegmentRef
}

// 清单级错误：调用方据此以退出码 2 终止。
var (
	ErrNotPlaylist = fmt.Errorf("not a media playlist")
	ErrBadPlaylist = fmt.Errorf("malformed playlist")
)

// ParseHLS 解析 HLS 媒体播放清单（#EXT-X-MEDIA-SEQUENCE + #EXTINF）。
func ParseHLS(r io.Reader) (Playlist, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	header := false
	seq0 := 0
	nextSeq := -1
	var refs []model.SegmentRef
	var pendingDur model.Rat
	haveDur := false
	cursor := model.Nano(0)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTM3U") {
			header = true
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if !haveDur {
				return Playlist{}, fmt.Errorf("%w: segment %q without EXTINF", ErrBadPlaylist, line)
			}
			seq := seq0
			if nextSeq >= 0 {
				seq = nextSeq
			}
			refs = append(refs, model.SegmentRef{
				Seq:      seq,
				URI:      line,
				Start:    cursor,
				Duration: pendingDur,
			})
			cursor = cursor.Add(pendingDur)
			nextSeq = seq + 1
			haveDur = false
			continue
		}
		tag, val, _ := strings.Cut(line, ":")
		switch tag {
		case "#EXT-X-MEDIA-SEQUENCE":
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil || n < 0 {
				return Playlist{}, fmt.Errorf("%w: bad media sequence %q", ErrBadPlaylist, val)
			}
			seq0 = n
			nextSeq = n
		case "#EXTINF":
			durStr, _, _ := strings.Cut(val, ",")
			d, err := model.FromDecimalSeconds(strings.TrimSpace(durStr))
			if err != nil || d.Num < 0 {
				return Playlist{}, fmt.Errorf("%w: bad EXTINF %q", ErrBadPlaylist, val)
			}
			pendingDur = d
			haveDur = true
		}
	}
	if err := sc.Err(); err != nil {
		return Playlist{}, err
	}
	if !header {
		return Playlist{}, ErrNotPlaylist
	}
	return Playlist{Kind: "hls", Seq0: seq0, Refs: refs}, nil
}

// ParseHLSFile 从本地路径读取并解析 HLS 清单。
func ParseHLSFile(path string) (Playlist, error) {
	f, err := os.Open(path)
	if err != nil {
		return Playlist{}, err
	}
	defer f.Close()
	return ParseHLS(f)
}

// LoadPair 从输入目录定位 HLS 与 DASH 清单文件。
// 约定文件名：playlist.m3u8 与 manifest.mpd。
func LoadPair(dir string) (hlsPath, dashPath string, err error) {
	hlsPath = filepath.Join(dir, "playlist.m3u8")
	dashPath = filepath.Join(dir, "manifest.mpd")
	if _, e := os.Stat(hlsPath); e != nil {
		return "", "", fmt.Errorf("hls playlist: %w", e)
	}
	if _, e := os.Stat(dashPath); e != nil {
		return "", "", fmt.Errorf("dash manifest: %w", e)
	}
	return hlsPath, dashPath, nil
}
