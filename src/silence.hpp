#pragma once
// Silence analysis on the shared timeline. Audio packets carry packed
// little-endian 16-bit PCM; libswresample converts them to float for RMS
// measurement, per channel. Decode errors cut the detection window and
// sub-minimum tail windows never produce silence reports.
#include <vector>

#include "common.hpp"
#include "segmenter.hpp"

namespace mb {

struct SilenceRange {
  i64 start_ns = 0;
  i64 end_ns = 0;
  u16 channel = 0;
  double mean_rms = 0.0;
};

struct SilenceResult {
  std::vector<SilenceRange> ranges;
  u64 silence_ns = 0;             // union over channels, counted once
  u64 window_count = 0;
  u64 decode_errors = 0;
  std::vector<u64> per_channel_ns;
};

class SilenceDetector {
 public:
  SilenceDetector(u32 sample_rate, u16 channels);
  ~SilenceDetector();
  SilenceDetector(const SilenceDetector&) = delete;
  SilenceDetector& operator=(const SilenceDetector&) = delete;

  // Analyse every segment of one audio track; corrupt packets split windows.
  SilenceResult analyse(const std::vector<Segment>& segments,
                        const std::vector<PacketRecord>& packets);

 private:
  struct Impl;
  Impl* impl_;
};

// PCM helpers shared by generator, detector and tests.
std::vector<float> pcm16le_to_float(const std::vector<u8>& payload,
                                    u16 channels);
i64 pcm_sample_duration_ns(std::size_t frame_count, u32 sample_rate);

}  // namespace mb
