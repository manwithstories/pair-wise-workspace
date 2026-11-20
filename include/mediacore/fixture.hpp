#pragma once

#include <filesystem>
#include <string>
#include <vector>

namespace mediacore::fixture {

struct FixtureLayout {
    std::filesystem::path root;
    // Logical tracks written into the fixture set.
    struct Track {
        std::string id;
        std::string role;
        std::string mediaType;   // video / audio
        std::string playlist;    // manifest file name
        int channels = 0;
        int sampleRate = 0;
    };
    std::vector<Track> tracks;
};

// Generate a fully offline fixture set under `root` using the local
// ffmpeg binary: healthy HLS master + media playlists, a DASH MPD with a
// truncated/corrupt segment, zero-length and boundary-timestamp samples,
// plus tracks.json. Returns the populated layout.
FixtureLayout generate(const std::filesystem::path& root);

}  // namespace mediacore::fixture
