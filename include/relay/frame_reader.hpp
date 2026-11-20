#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "relay/types.hpp"

namespace relay {

/// A frame as listed in the self-describing capture file.  `verified`
/// is false when SHA-256 or the FFmpeg decode disagreed with the record.
struct CatalogFrame {
  FrameRecord record;
  bool verified = false;
  std::string issue;
};

struct Arrival {
  std::uint64_t arrival_index = 0;
  std::uint64_t source_frame = 0;
};

struct TrackInput {
  TrackSpec spec;
  Timebase timebase;
  std::vector<CatalogFrame> catalog;
  std::vector<Arrival> arrivals;
  std::size_t checksum_failures = 0;
  std::size_t decode_failures = 0;
  std::string file_digest;
};

struct ReadResult {
  std::vector<TrackInput> tracks;
  std::string input_digest;
  std::size_t checksum_failures = 0;
  std::size_t decode_failures = 0;
};

/// Parses `<input_dir>/*.frames` (header + FRAME metadata + PNG payload +
/// SHA-256) and the matching `*.arrival` schedule.  Verification failures
/// quarantine the single frame and keep the rest of the track usable.
ReadResult read_frame_inputs(const std::filesystem::path& input_dir,
                             ErrorSink& errors);

/// Decodes one PNG payload through libavformat/libavcodec and checks that
/// its dimensions and pixel format match the declared metadata.
bool verify_decoded_frame(const std::vector<std::uint8_t>& payload,
                          const FrameRecord& record, std::string& reason);

}  // namespace relay
