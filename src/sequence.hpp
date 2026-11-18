#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

#include "frame.hpp"

// One manifest line: "<timestamp_us>\t<relative PPM path>".
struct ManifestEntry {
    std::size_t index = 0;
    uint64_t timestamp_us = 0;
    std::string relpath;
};

struct Manifest {
    std::string base_dir; // directory containing the manifest file
    std::vector<ManifestEntry> entries;
};

// Loads and validates a manifest. Rejects: malformed lines, absolute
// paths, ".." path components, duplicate timestamps and non-increasing
// timestamp order. Throws std::runtime_error with the line number.
Manifest load_manifest(const std::string& path);

// Joins the manifest base dir with an entry's relative path.
std::string manifest_entry_path(const Manifest& m, const ManifestEntry& e);

// Result of feeding one decoded frame into a FrameStream.
enum class PushResult {
    Reference,    // accepted as a new reference frame; no intensity
    Intensity,    // compared against the previous valid frame
    SizeMismatch, // rejected: dimensions differ from the stream's
};

// Streaming comparison chain. Holds only the previous frame, so memory
// stays constant regardless of sequence length. A skipped/corrupt frame
// must be signalled via cut(), which breaks the chain: the next valid
// frame becomes a fresh reference and yields no intensity.
class FrameStream {
public:
    // Takes ownership of a decoded frame. On SizeMismatch the frame is
    // not adopted and `err` describes the expected vs actual size.
    PushResult push(FrameBuffer f, double& out_intensity, std::string& err);

    // Breaks the comparison chain (skipped frame, corrupt frame, ...).
    void cut() { have_prev_ = false; }

    bool dims_known() const { return dims_set_; }
    std::size_t width() const { return w_; }
    std::size_t height() const { return h_; }

private:
    FrameBuffer prev_;
    bool have_prev_ = false;
    bool dims_set_ = false;
    std::size_t w_ = 0, h_ = 0;
};
