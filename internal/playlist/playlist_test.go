package playlist

import "testing"

func TestMediaPlaylistRoundTrip(t *testing.T) {
	media := &MediaPlaylist{
		Rendition: 2, Name: "540p", BandwidthBps: 1600000, Resolution: "960x540",
		TargetDuration: 4, MediaSequence: 0, FileName: "media-r2.m3u8",
		Entries: []MediaEntry{
			{URI: "r2-s0.ts", DurationS: 4, Seq: 0, SourceTSMS: 0, MeasuredMS: 4012, DeclaredMS: 4000},
			{URI: "gap-r2-s000001.ts", DurationS: 4, IsGap: true, Seq: 1, SourceTSMS: 4000, MeasuredMS: 4000, DeclaredMS: 4000},
			{URI: "r2-s2.ts", DurationS: 4, Seq: 2, SourceTSMS: 8000, MeasuredMS: 3990, DeclaredMS: 4000},
		},
	}
	parsed, err := Parse([]byte(RenderMedia(media)))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if parsed.Kind != "media" || len(parsed.Media.Entries) != 3 {
		t.Fatalf("unexpected parse result: %+v", parsed)
	}
	if !parsed.Media.Entries[1].IsGap {
		t.Fatalf("gap marker lost in round trip")
	}
	if parsed.Media.BandwidthBps != 1600000 || parsed.Media.Name != "540p" || parsed.Media.Resolution != "960x540" {
		t.Fatalf("info header lost: %+v", parsed.Media)
	}
	first := parsed.Media.Entries[0]
	if first.MeasuredMS != 4012 || first.SourceTSMS != 0 || first.Seq != 0 {
		t.Fatalf("meta lost in round trip: %+v", first)
	}
}

func TestApplyWindow(t *testing.T) {
	media := &MediaPlaylist{MediaSequence: 0}
	for seq := int64(0); seq < 10; seq++ {
		media.Entries = append(media.Entries, MediaEntry{Seq: seq, URI: "x"})
	}
	ApplyWindow(media, 3)
	if len(media.Entries) != 3 || media.MediaSequence != 7 || media.Entries[0].Seq != 7 {
		t.Fatalf("window result wrong: seq0=%d ms=%d len=%d",
			media.Entries[0].Seq, media.MediaSequence, len(media.Entries))
	}
	ApplyWindow(media, 0)
	if len(media.Entries) != 3 {
		t.Fatalf("window 0 must be a no-op")
	}
}

func TestParseRejectsGarbageWithoutPanic(t *testing.T) {
	garbage := [][]byte{
		nil,
		{},
		[]byte("not a playlist at all\n"),
		[]byte("#EXTM3U\n#EXTINF:NaN,\nu.ts\n"),
		[]byte("#EXTM3U\n#EXT-X-TARGETDURATION:oops\n"),
		[]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=abc\nm.m3u8\n"),
	}
	for _, data := range garbage {
		if playlist, err := Parse(data); err == nil {
			t.Fatalf("expected error for %q, got %+v", string(data), playlist)
		}
	}
}

func TestMasterRoundTrip(t *testing.T) {
	master := &MasterPlaylist{Version: 3, TargetDuration: 4, FileName: "master.m3u8",
		Media: []MediaRef{{BandwidthBps: 400000, Resolution: "426x240", Name: "240p", URI: "media-r0.m3u8"}}}
	parsed, err := Parse([]byte(RenderMaster(master)))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if parsed.Kind != "master" || len(parsed.Master.Media) != 1 ||
		parsed.Master.Media[0].Name != "240p" {
		t.Fatalf("master round trip failed: %+v", parsed)
	}
}
