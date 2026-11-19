#pragma once

// Deterministic sample-clip generator.
//
// Everything is synthesised locally: frames are simple procedural patterns
// encoded with the in-tree PNG encoder, packets are stamped with a simulated
// capture clock, and network perturbations are driven by a user-visible seed.
// The same seed always produces the same bytes on disk.

#include <cstdint>
#include <string>
#include <vector>

#include "media_types.hpp"

namespace mfs {

struct GeneratorOptions {
  uint32_t seed{42};
  uint32_t frames{1200};
  uint32_t width{320};
  uint32_t height{180};
  uint32_t keyframe_interval{30};
  uint32_t frame_rate{30};
  // Number of consecutive frames dropped per loss burst.
  uint32_t loss_burst{7};
  // Number of bursts spread over the clip.
  uint32_t loss_events{12};
  double duplicate_rate{0.08};
  double reorder_rate{0.15};
  uint32_t reorder_window{16};
  uint32_t timestamp_defects{8};
  uint32_t sample_width{0};  // 0 -> every frame is a full image
  std::vector<uint32_t> arrival_gaps;  // induced stalls, in frame slots
};

struct GenerationStats {
  uint32_t frames{0};
  uint32_t packets{0};
  uint32_t duplicates{0};
  uint32_t reordered{0};
  uint32_t dropped{0};
  uint32_t clock_defects{0};
  uint64_t bytes{0};
};

// Renders the clip in memory. `packets` is the on-disk delivery order, i.e.
// after duplication, reordering and loss.
GenerationStats generate_sample(const GeneratorOptions& options,
                                std::vector<Packet>& packets);

// Serialises packets into an MFS1 container.
std::vector<uint8_t> encode_container(const std::vector<Packet>& packets,
                                      uint64_t stream_id);

// Writes the container to disk. Throws MediaError on I/O failure.
void write_container(const std::string& path,
                     const std::vector<Packet>& packets, uint64_t stream_id);

}  // namespace mfs
