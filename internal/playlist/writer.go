package playlist

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AtomicWriteFile writes data to path via a temp file in the same directory
// followed by an atomic rename. A crash mid-write can never leave a truncated
// playlist at the final path.
func AtomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempName)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func formatDuration(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 3, 64)
}

// RenderMedia serializes a media playlist to HLS text. Lines starting with
// #EXTM3U8CTL- carry round-trip metadata consumed by this project's own
// parser; standard HLS players ignore unknown tags.
func RenderMedia(media *MediaPlaylist) string {
	var builder strings.Builder
	builder.WriteString("#EXTM3U\n")
	builder.WriteString("#EXT-X-VERSION:3\n")
	builder.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", media.TargetDuration))
	builder.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", media.MediaSequence))
	if len(media.Entries) == 0 {
		builder.WriteString("#EXT-X-ENDLIST\n")
		return builder.String()
	}
	if media.MediaSequence == 0 {
		builder.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	}
	builder.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	builder.WriteString(fmt.Sprintf("#EXTM3U8CTL-RENDITION:%d\n", media.Rendition))
	builder.WriteString(fmt.Sprintf("#EXTM3U8CTL-INFO:name=%q,bandwidth=%d,resolution=%s\n",
		media.Name, media.BandwidthBps, media.Resolution))
	builder.WriteString(fmt.Sprintf("#EXTM3U8CTL-SLOT-MS:%d\n", int64(media.TargetDuration)*1000))
	builder.WriteString(fmt.Sprintf("#EXTM3U8CTL-BASE-TS:%d\n", firstSourceTS(media)))
	for _, entry := range media.Entries {
		builder.WriteString(fmt.Sprintf("#EXTINF:%s,\n", formatDuration(entry.DurationS)))
		builder.WriteString(fmt.Sprintf("#EXTM3U8CTL-META:seq=%d,ts=%d,meas=%d,decl=%d\n",
			entry.Seq, entry.SourceTSMS, entry.MeasuredMS, entry.DeclaredMS))
		if entry.IsGap {
			builder.WriteString("#EXT-X-GAP\n")
		}
		builder.WriteString(entry.URI)
		builder.WriteByte('\n')
	}
	if media.MediaSequence == 0 {
		builder.WriteString("#EXT-X-ENDLIST\n")
	}
	return builder.String()
}

func firstSourceTS(media *MediaPlaylist) int64 {
	if len(media.Entries) == 0 {
		return 0
	}
	return media.Entries[0].SourceTSMS - media.Entries[0].Seq*int64(media.TargetDuration)*1000
}

// RenderMaster serializes the master playlist referencing every rendition.
func RenderMaster(master *MasterPlaylist) string {
	var builder strings.Builder
	builder.WriteString("#EXTM3U\n")
	builder.WriteString("#EXT-X-VERSION:3\n")
	builder.WriteString(fmt.Sprintf("#EXTM3U8CTL-TARGET-DURATION:%d\n", master.TargetDuration))
	for _, media := range master.Media {
		builder.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%s,NAME=%q\n",
			media.BandwidthBps, media.Resolution, media.Name))
		builder.WriteString(media.URI)
		builder.WriteByte('\n')
	}
	return builder.String()
}

// WriteMedia atomically writes one media playlist.
func WriteMedia(dir string, media *MediaPlaylist) error {
	return AtomicWriteFile(filepath.Join(dir, media.FileName), []byte(RenderMedia(media)))
}

// WriteMaster atomically writes the master playlist.
func WriteMaster(dir string, master *MasterPlaylist) error {
	return AtomicWriteFile(filepath.Join(dir, master.FileName), []byte(RenderMaster(master)))
}

// ApplyWindow keeps only the last window entries of a media playlist and
// shifts EXT-X-MEDIA-SEQUENCE accordingly. window <= 0 means "no window".
func ApplyWindow(media *MediaPlaylist, window int) {
	if window <= 0 || len(media.Entries) <= window {
		return
	}
	dropped := len(media.Entries) - window
	media.MediaSequence = media.Entries[dropped].Seq
	media.Entries = append([]MediaEntry(nil), media.Entries[dropped:]...)
}
