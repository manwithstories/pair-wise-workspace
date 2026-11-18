#pragma once

#include <array>
#include <cstdint>
#include <string>
#include <vector>

#include "frame.hpp"
#include "segments.hpp"

// One row of the per-frame report.
struct FrameRow {
    std::size_t index = 0;
    uint64_t timestamp_us = 0;
    bool ok = true;            // false: frame was skipped (corrupt/missing)
    bool has_intensity = false;
    double intensity = 0.0;
    bool has_bits = false;
    uint64_t bits = 0;
    std::string note;          // skip reason when ok == false
};

// Writes "index,timestamp_us,status,intensity,bits,note" rows.
// Throws std::runtime_error on I/O failure.
void write_frames_csv(const std::string& path,
                      const std::vector<FrameRow>& rows);

// Writes "begin,end" half-open segment rows.
// Throws std::runtime_error on I/O failure.
void write_segments_csv(const std::string& path,
                        const std::vector<Segment>& segments);

// Renders a 10-bucket histogram as a 256x128 grayscale image (one bar
// per bucket, heights normalized to the tallest bucket). Deterministic.
FrameBuffer render_histogram(const std::array<uint64_t, 10>& hist);

// Sum of all pixel byte values (row padding excluded).
uint64_t pixel_sum(const FrameBuffer& img);

// FNV-1a 64-bit hash over pixel bytes in row order (padding excluded).
uint64_t fnv1a64(const FrameBuffer& img);

struct HistogramSummary {
    uint64_t pixels_sum = 0;
    uint64_t fnv = 0;
};

// Renders the histogram and writes it as a P6 PPM; returns the summary
// figures for the terminal report. Throws std::runtime_error on I/O
// failure.
HistogramSummary write_histogram_ppm(const std::string& path,
                                     const std::array<uint64_t, 10>& hist);
