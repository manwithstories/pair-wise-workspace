#include <array>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <string>

#include "mediacore/fixture.hpp"

namespace mediacore::fixture {

namespace fs = std::filesystem;

namespace {

// Run a shell command; returns the exit status. All sources are lavfi
// virtual inputs, so the generator never touches the network.
int run(const std::string& cmd) {
    return std::system(cmd.c_str());
}

std::string q(const fs::path& p) {
    return "'" + p.string() + "'";
}

}  // namespace

// Encode one MPEG-TS video segment. `patternIndex` varies the lavfi source
// so distinct segments produce distinct bytes (and thus distinct hashes).
bool makeVideoSegment(const fs::path& out, int seconds,
                      const std::string& size = "640x360",
                      int rate = 25, int patternIndex = 0) {
    const std::string pattern =
        (patternIndex % 2 == 0) ? "testsrc" : "smptebars";
    const std::string cmd =
        "ffmpeg -hide_banner -loglevel error -y -f lavfi -i " +
        pattern + "=duration=" + std::to_string(seconds) + ":size=" +
        size + ":rate=" + std::to_string(rate) +
        " -c:v libx264 -g " + std::to_string(rate) +
        " -keyint_min " + std::to_string(rate) +
        " -pix_fmt yuv420p -f mpegts " + q(out) + " 2>/dev/null";
    return run(cmd) == 0;
}

// Encode one ADTS AAC audio segment. `toneHz` varies the source so
// successive segments hash differently.
bool makeAudioSegment(const fs::path& out, int seconds,
                      int sampleRate = 48000, int channels = 2,
                      int toneHz = 440) {
    const std::string cmd =
        "ffmpeg -hide_banner -loglevel error -y -f lavfi -i "
        "sine=frequency=" + std::to_string(toneHz) + ":duration=" +
        std::to_string(seconds) + " -ar " +
        std::to_string(sampleRate) + " -ac " +
        std::to_string(channels) + " -c:a aac -f adts " + q(out) +
        " 2>/dev/null";
    return run(cmd) == 0;
}

// Encode an fMP4 DASH media chunk plus an initialization segment.
bool makeDashChunk(const fs::path& init, const fs::path& chunk,
                   int seconds) {
    const std::string cmd =
        "ffmpeg -hide_banner -loglevel error -y -f lavfi -i "
        "testsrc=duration=" + std::to_string(seconds) +
        ":size=640x360:rate=25 -c:v libx264 -g 25 -pix_fmt yuv420p "
        "-movflags +frag_keyframe+empty_moov+default_base_moof -f mp4 "
        + q(chunk) + " 2>/dev/null";
    if (run(cmd) != 0) return false;
    // Minimal init segment: fMP4 header produced from a zero-duration run.
    const std::string initCmd =
        "ffmpeg -hide_banner -loglevel error -y -f lavfi -i "
        "testsrc=duration=0.04:size=640x360:rate=25 -c:v libx264 "
        "-movflags +frag_keyframe+empty_moov+default_base_moof -f mp4 "
        + q(init) + " 2>/dev/null";
    return run(initCmd) == 0;
}

// Truncate a file in place to the given fraction of its size, simulating
// a damaged segment tail.
void truncateFile(const fs::path& path, double keepFraction) {
    std::error_code ec;
    const auto full = fs::file_size(path, ec);
    if (ec) return;
    const auto target =
        static_cast<std::uintmax_t>(full * keepFraction);
    FILE* f = std::fopen(path.string().c_str(), "r+b");
    if (!f) return;
    std::fclose(f);
    fs::resize_file(path, target, ec);
}

// Write a zero-byte file (zero-length range sample).
void makeEmptyFile(const fs::path& path) {
    std::FILE* f = std::fopen(path.string().c_str(), "wb");
    if (f) std::fclose(f);
}

}  // namespace mediacore::fixture
