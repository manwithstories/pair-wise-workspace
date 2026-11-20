#include <catch2/catch_test_macros.hpp>

#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <string>
#include <unistd.h>

#include "mediacore/probe.hpp"

namespace fs = std::filesystem;
using namespace mediacore;

namespace {

fs::path probeDir() {
    auto dir = fs::temp_directory_path() /
               ("mcaudit_probe_" + std::to_string(
                   static_cast<long>(::getpid())));
    fs::remove_all(dir);
    fs::create_directories(dir);
    return dir;
}

// Tests are offline: the fixture is synthesized by the locally installed
// ffmpeg binary, no network access.
void synthesizeMpegTs(const fs::path& out) {
    const std::string cmd =
        "ffmpeg -hide_banner -loglevel error -y -f lavfi -i "
        "testsrc=duration=2:size=320x240:rate=25 -f lavfi -i "
        "sine=frequency=440:duration=2 -c:v mpeg2video -c:a mp2 "
        "-f mpegts '" + out.string() + "' 2>/dev/null";
    REQUIRE(std::system(cmd.c_str()) == 0);
}

}  // namespace

TEST_CASE("probe reads container, streams and keyframes", "[probe]") {
    auto dir = probeDir();
    const auto seg = dir / "good.ts";
    synthesizeMpegTs(seg);

    ErrorLog errors;
    ProbeOptions opts;
    auto probe = probeSegment(seg, "v0", 0, opts, errors);
    REQUIRE(probe.opened);
    REQUIRE(probe.containerFormat.find("mpegts") != std::string::npos);
    REQUIRE(probe.packetCount > 0);
    bool hasVideo = false, hasAudio = false;
    for (const auto& s : probe.streams) {
        if (s.kind == MediaKind::Video) {
            hasVideo = true;
            REQUIRE(s.width == 320);
            REQUIRE(s.height == 240);
            REQUIRE_FALSE(s.codecName.empty());
        }
        if (s.kind == MediaKind::Audio) {
            hasAudio = true;
            REQUIRE(s.sampleRate > 0);
            REQUIRE(s.channels > 0);
        }
    }
    REQUIRE(hasVideo);
    REQUIRE(hasAudio);
    REQUIRE_FALSE(probe.keyframes.empty());
    REQUIRE_FALSE(errors.hasFatal());

    auto gaps = analyzeKeyframeGaps(probe);
    // Regular testsrc GOP produces no anomalous gaps.
    for (const auto& g : gaps)
        REQUIRE(g.actualInterval != 0);
}

TEST_CASE("zero-length segment is a located recoverable error",
          "[probe]") {
    auto dir = probeDir();
    const auto seg = dir / "empty.ts";
    std::ofstream(seg) << "";
    ErrorLog errors;
    ProbeOptions opts;
    auto probe = probeSegment(seg, "v0", 3, opts, errors);
    REQUIRE_FALSE(probe.opened);
    REQUIRE(errors.count(ErrorCode::ZeroLengthRange) == 1);
    const auto& e = errors.items().front();
    REQUIRE(e.segment == 3);
    REQUIRE(e.recoverable);
}

TEST_CASE("truncated tail is reported as corrupt", "[probe]") {
    auto dir = probeDir();
    const auto good = dir / "good.mp4";
    const auto cut = dir / "cut.mp4";
    const std::string make =
        "ffmpeg -hide_banner -loglevel error -y -f lavfi -i "
        "testsrc=duration=2:size=320x240:rate=25 -f lavfi -i "
        "sine=frequency=440:duration=2 -c:v libx264 -c:a aac "
        "-movflags +frag_keyframe+empty_moov -f mp4 '" +
        good.string() + "' 2>/dev/null";
    REQUIRE(std::system(make.c_str()) == 0);
    auto size = fs::file_size(good);
    std::ifstream in(good, std::ios::binary);
    std::string body(size, '\0');
    in.read(body.data(), static_cast<std::streamsize>(size));
    // Keep the first quarter: the demuxer opens but packets cover far less
    // than the declared 2 s, which the duration-coverage check locates.
    body.resize(size / 4);
    std::ofstream out(cut, std::ios::binary);
    out.write(body.data(), static_cast<std::streamsize>(body.size()));

    ErrorLog errors;
    ProbeOptions opts;
    opts.declaredDuration = Rational::reduced(2, 1);
    auto probe = probeSegment(cut, "v0", 1, opts, errors);
    const bool located =
        errors.count(ErrorCode::SegmentCorrupt) > 0 ||
        errors.count(ErrorCode::ZeroLengthRange) > 0 || !probe.opened;
    REQUIRE(located);
    REQUIRE(probe.tailCorrupt);
}

TEST_CASE("keyframe index hit evaluation requires boundary match",
          "[probe]") {
    auto dir = probeDir();
    const auto seg = dir / "kf.ts";
    synthesizeMpegTs(seg);
    ErrorLog errors;
    ProbeOptions opts;
    auto probe = probeSegment(seg, "v0", 0, opts, errors);
    REQUIRE(probe.opened);

    std::vector<SegmentProbe> probes;
    probes.push_back(std::move(probe));
    const auto& kf = probes.front().keyframes.front();
    // Correct boundary: map the first keyframe tick onto its own base.
    TimeRange good{kf.pts, kf.pts + 1, kf.base};
    TimeRange bad{kf.pts + 12345, kf.pts + 12346, kf.base};
    auto hit = evaluateKeyframeIndex(probes, {good});
    auto miss = evaluateKeyframeIndex(probes, {bad});
    REQUIRE(hit.hits == 1);
    REQUIRE(hit.hitRate() == 1.0);
    REQUIRE(miss.hits == 0);
    REQUIRE(miss.evaluated == 1);
}
