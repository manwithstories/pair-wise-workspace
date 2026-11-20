#include <catch2/catch_test_macros.hpp>

#include <filesystem>
#include <fstream>
#include <unistd.h>

#include "mediacore/manifest.hpp"

namespace fs = std::filesystem;
using namespace mediacore;

namespace {

fs::path makeTempDir() {
    auto dir = fs::temp_directory_path() /
               ("mcaudit_manifest_" + std::to_string(
                   static_cast<long>(::getpid())));
    fs::remove_all(dir);
    fs::create_directories(dir);
    return dir;
}

void writeFile(const fs::path& p, const std::string& body) {
    std::ofstream out(p);
    out << body;
}

}  // namespace

TEST_CASE("HLS playlist parses segments and truncation", "[manifest]") {
    auto dir = makeTempDir();
    const auto m3u8 = dir / "v0.m3u8";
    writeFile(m3u8,
              "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n"
              "#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:2.0,\nseg10.ts\n"
              "#EXT-X-DISCONTINUITY\n#EXTINF:1.968,\nseg11.ts\n"
              "#EXT-X-ENDLIST\n");
    std::ofstream(dir / "seg10.ts") << "x";
    std::ofstream(dir / "seg11.ts") << "x";

    ErrorLog errors;
    ParseOptions opts;
    opts.root = dir;
    auto m = parseManifest(m3u8, opts, errors);
    REQUIRE(m.format == ManifestFormat::Hls);
    REQUIRE(m.playlists.size() == 1);
    const auto& rep = m.playlists[0].representations[0];
    REQUIRE(rep.segments.size() == 2);
    REQUIRE(rep.segments[0].sequence == 10);
    REQUIRE(rep.segments[1].discontinuity == 1);
    REQUIRE(rep.segments[1].duration.num == 246);  // 1.968 == 246/125
    REQUIRE(rep.segments[1].duration.den == 125);
    REQUIRE(m.playlists[0].ended);
    REQUIRE_FALSE(errors.hasFatal());

    auto rt = roundTripCompare(m);
    REQUIRE(rt.consistency() >= 0.99);
    REQUIRE(rt.mismatches.empty());
}

TEST_CASE("HLS missing ENDLIST is a located truncation", "[manifest]") {
    auto dir = makeTempDir();
    const auto m3u8 = dir / "cut.m3u8";
    writeFile(m3u8, "#EXTM3U\n#EXTINF:2.0,\nseg0.ts\n");
    std::ofstream(dir / "seg0.ts") << "x";
    ErrorLog errors;
    ParseOptions opts;
    opts.root = dir;
    auto m = parseManifest(m3u8, opts, errors);
    REQUIRE(errors.count(ErrorCode::ManifestTruncated) == 1);
    const auto& e = errors.items().front();
    REQUIRE(e.segment == 0);
    REQUIRE_FALSE(m.playlists[0].ended);
}

TEST_CASE("path traversal in HLS URI is rejected", "[manifest]") {
    auto dir = makeTempDir();
    writeFile(dir / "bad.m3u8",
              "#EXTM3U\n#EXTINF:1.0,\n../../../etc/hosts\n#EXT-X-ENDLIST\n");
    ErrorLog errors;
    ParseOptions opts;
    opts.root = dir;
    parseManifest(dir / "bad.m3u8", opts, errors);
    REQUIRE(errors.count(ErrorCode::PathEscape) == 1);
    REQUIRE_FALSE(errors.items().front().recoverable);
}

TEST_CASE("DASH template with SegmentTimeline expands segments",
          "[manifest]") {
    auto dir = makeTempDir();
    for (int i = 1; i <= 3; ++i)
        std::ofstream(dir / ("chunk-" + std::to_string(i) + ".m4s")) << "x";
    std::ofstream(dir / "init.m4s") << "x";
    writeFile(
        dir / "broken.mpd",
        R"(<?xml version="1.0"?>
<MPD type="static">
  <AdaptationSet contentType="video">
    <SegmentTemplate media="chunk-$Number$.m4s"
      initialization="init.m4s" timescale="1000" startNumber="1">
      <SegmentTimeline>
        <S t="0" d="1000" r="2"/>
      </SegmentTimeline>
    </SegmentTemplate>
    <Representation id="v0" bandwidth="2000000" codecs="avc1.64001f"
      width="1280" height="720"/>
  </AdaptationSet>
</MPD>)");
    ErrorLog errors;
    ParseOptions opts;
    opts.root = dir;
    auto m = parseManifest(dir / "broken.mpd", opts, errors);
    REQUIRE(m.format == ManifestFormat::Dash);
    const auto& rep = m.playlists[0].representations[0];
    REQUIRE(rep.segments.size() == 3);
    REQUIRE(rep.segments[0].uri == "chunk-1.m4s");
    REQUIRE(rep.segments[2].startTick == 2000);
    REQUIRE(rep.segments[0].tickBase.den == 1000);
    REQUIRE(rep.width == 1280);
    auto rt = roundTripCompare(m);
    REQUIRE(rt.consistency() >= 0.99);
}

TEST_CASE("truncated MPD reports fatal truncation", "[manifest]") {
    auto dir = makeTempDir();
    writeFile(dir / "cut.mpd",
              R"(<MPD type="static"><AdaptationSet contentType="audio">)");
    ErrorLog errors;
    ParseOptions opts;
    opts.root = dir;
    parseManifest(dir / "cut.mpd", opts, errors);
    REQUIRE(errors.count(ErrorCode::ManifestTruncated) == 1);
    REQUIRE_FALSE(errors.items().front().recoverable);
}

TEST_CASE("DASH missing segment file is a recoverable error",
          "[manifest]") {
    auto dir = makeTempDir();
    writeFile(
        dir / "m.mpd",
        R"(<MPD><AdaptationSet contentType="audio">
<SegmentTemplate media="a-$Number$.m4s" initialization="i.m4s"
 timescale="48000" startNumber="1" duration="48000"/>
<Representation id="a0" bandwidth="64000"/>
</AdaptationSet></MPD>)");
    ErrorLog errors;
    ParseOptions opts;
    opts.root = dir;
    parseManifest(dir / "m.mpd", opts, errors);
    // No chunk files exist: zero expansion (fixed template probes disk),
    // so explicitly write a timeline-based manifest expecting one file.
    writeFile(
        dir / "m2.mpd",
        R"(<MPD><AdaptationSet contentType="audio">
<SegmentTemplate media="a-$Number$.m4s" initialization="i.m4s"
 timescale="48000" startNumber="1">
<SegmentTimeline><S t="0" d="48000"/></SegmentTimeline>
</SegmentTemplate>
<Representation id="a0" bandwidth="64000"/>
</AdaptationSet></MPD>)");
    ErrorLog errors2;
    auto m2 = parseManifest(dir / "m2.mpd", opts, errors2);
    REQUIRE(m2.playlists[0].representations[0].segments.size() == 1);
    REQUIRE(errors2.count(ErrorCode::SegmentMissing) == 1);
    REQUIRE(errors2.items().front().recoverable);
}
