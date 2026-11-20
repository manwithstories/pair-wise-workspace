#include <fstream>
#include <string>

#include "mediacore/fixture.hpp"

namespace mediacore::fixture {

namespace fs = std::filesystem;

// Helpers implemented in fixture_media.cpp.
bool makeVideoSegment(const fs::path&, int,
                      const std::string& size = "640x360", int rate = 25,
                      int patternIndex = 0);
bool makeAudioSegment(const fs::path&, int, int = 48000, int = 2,
                      int toneHz = 440);
bool makeDashChunk(const fs::path&, const fs::path&, int);
void truncateFile(const fs::path&, double);
void makeEmptyFile(const fs::path&);

namespace {

void write(const fs::path& p, const std::string& body) {
    std::ofstream out(p);
    out << body;
}

// HLS media playlist for a set of equal-duration segments.
std::string hlsPlaylist(const std::vector<std::string>& uris,
                        int targetDuration, bool ended) {
    std::string s = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:" +
                    std::to_string(targetDuration) +
                    "\n#EXT-X-MEDIA-SEQUENCE:0\n";
    for (const auto& u : uris)
        s += "#EXTINF:" + std::to_string(targetDuration) + ".0,\n" +
             u + "\n";
    if (ended) s += "#EXT-X-ENDLIST\n";
    return s;
}

}  // namespace

FixtureLayout generate(const fs::path& root) {
    fs::remove_all(root);
    fs::create_directories(root / "video");
    fs::create_directories(root / "audio");
    fs::create_directories(root / "dash");
    FixtureLayout layout;
    layout.root = root;

    // ---- Video track: 3 healthy 2 s segments ------------------------
    std::vector<std::string> vUris;
    for (int i = 0; i < 3; ++i) {
        const std::string name =
            "v" + std::to_string(i) + ".ts";
        makeVideoSegment(root / "video" / name, 2, "640x360", 25, i);
        vUris.push_back("video/" + name);
    }
    write(root / "video.m3u8", hlsPlaylist(vUris, 2, true));
    layout.tracks.push_back(
        {"interview", "studio-interview", "video", "video.m3u8", 0, 0});

    // ---- Audio track: 4 healthy 1 s segments ------------------------
    std::vector<std::string> aUris;
    for (int i = 0; i < 4; ++i) {
        const std::string name =
            "a" + std::to_string(i) + ".aac";
        makeAudioSegment(root / "audio" / name, 1, 48000, 2,
                         330 + i * 60);
        aUris.push_back("audio/" + name);
    }
    write(root / "audio.m3u8", hlsPlaylist(aUris, 1, true));
    layout.tracks.push_back(
        {"narration", "remote-narration", "audio", "audio.m3u8", 2, 48000});

    // ---- Boundary playlist: a genuinely non-divisible duration ------
    // 1/7 s cannot map exactly onto a 1/1,000,000 unified tick scale.
    write(root / "boundary.m3u8",
          "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"
          "#EXTINF:0.142857142857,\nvideo/v0.ts\n#EXT-X-ENDLIST\n");
    write(root / "empty.ts", "");  // create via dedicated helper below
    makeEmptyFile(root / "empty.ts");
    write(root / "zero.m3u8",
          "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:0.0,\nempty.ts\n"
          "#EXT-X-ENDLIST\n");

    // ---- Master playlist --------------------------------------------
    write(root / "master.m3u8",
          "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1500000,"
          "CODECS=\"avc1.64001f\"\nvideo.m3u8\n"
          "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a0\",NAME=\"narration\","
          "URI=\"audio.m3u8\"\n");

    // ---- DASH fixture: healthy chunks + damaged tail + missing ------
    // Chunks 1-4 are healthy (enough readable segments for >=4 hashes),
    // chunk 5 has a damaged tail, chunk 6 is declared but never written.
    makeDashChunk(root / "dash" / "init.m4s",
                  root / "dash" / "chunk-1.m4s", 2);
    for (int i = 2; i <= 4; ++i)
        makeVideoSegment(root / ("dash/chunk-" + std::to_string(i) +
                                 ".m4s"),
                         2, "640x360", 25, i);
    makeVideoSegment(root / "dash" / "chunk-5.m4s", 2, "640x360", 25, 5);
    // Corrupt the tail: keep 55% so duration-coverage detection fires.
    truncateFile(root / "dash" / "chunk-5.m4s", 0.55);
    // chunk-6 is referenced by the timeline but intentionally absent.
    write(
        root / "broken.mpd",
        R"(<?xml version="1.0"?>
<MPD type="static" mediaPresentationDuration="PT12S">
  <AdaptationSet contentType="video" mimeType="video/mp4">
    <SegmentTemplate media="dash/chunk-$Number$.m4s"
      initialization="dash/init.m4s" timescale="1000" startNumber="1">
      <SegmentTimeline>
        <S t="0" d="2000" r="5"/>
      </SegmentTimeline>
    </SegmentTemplate>
    <Representation id="bcam" bandwidth="1500000" codecs="avc1.64001f"
      width="640" height="360"/>
  </AdaptationSet>
</MPD>)");

    // ---- tracks.json (line-oriented subset understood by loadTracks) -
    std::string tj;
    for (const auto& t : layout.tracks) {
        tj += "{\n  \"id\": \"" + t.id + "\",\n  \"role\": \"" +
              t.role + "\",\n  \"playlist\": \"" + t.playlist +
              "\",\n  \"expected_media_type\": \"" + t.mediaType +
              "\",\n  \"expected_channels\": " +
              std::to_string(t.channels) +
              ",\n  \"expected_sample_rate\": " +
              std::to_string(t.sampleRate) + "\n}\n";
    }
    write(root / "tracks.json", tj);
    return layout;
}

}  // namespace mediacore::fixture
