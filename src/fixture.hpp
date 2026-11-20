#pragma once
// Deterministic fixture generator. All sample data is synthesised locally:
// no network, devices or external assets. A fixed seed produces byte-identical
// captures across runs, covering success, reorder, loss, truncation,
// corruption and missing index pages.
#include <map>
#include <vector>

#include "capture.hpp"
#include "common.hpp"

namespace mb {

struct FixtureOptions {
  u64 seed = 417;
  double reorder_probability = 0.08;   // packets swapped within the window
  double loss_probability = 0.01;      // packets dropped entirely
  double corrupt_probability = 0.005;  // payload replaced by 0xFF marker
  bool truncate_tail = true;           // cut the capture before EOS
  bool omit_index_pages = true;        // force segmenter index rebuild
  u32 video_frames = 240;              // ~8 s at 30 fps
  u32 audio_packets = 400;             // ~8 s at 20 ms per packet
  u32 sr_every = 50;                   // RTCP SR interval in packets
};

struct GeneratedFixture {
  std::vector<u8> capture;             // container bytes (possibly truncated)
  std::vector<u8> complete_capture;    // reference capture with EOS
  std::vector<TrackInfo> tracks;
  // Generator-side ground truth for self-test verification.
  std::map<u32, std::vector<std::array<u8, 32>>> reference_segment_hashes;
  u64 expected_packets = 0;
  u64 expected_segments = 0;
};

class FixtureGenerator {
 public:
  explicit FixtureGenerator(FixtureOptions opt) : opt_(opt) {}
  GeneratedFixture generate();

 private:
  struct DeterministicRng {
    u64 state;
    explicit DeterministicRng(u64 s) : state(s ? s : 0x9E3779B97F4A7C15ULL) {}
    u64 next() {  // splitmix64
      u64 z = (state += 0x9E3779B97F4A7C15ULL);
      z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ULL;
      z = (z ^ (z >> 27)) * 0x94D049BB133111EBULL;
      return z ^ (z >> 31);
    }
    double uniform() { return double(next() >> 11) / double(1ULL << 53); }
  };
  std::vector<u8> synth_video(DeterministicRng& rng, u32 frame, bool keyframe);
  std::vector<u8> synth_audio(DeterministicRng& rng, u32 packet, u16 channels,
                              u32 sample_rate, bool& silence);
  FixtureOptions opt_;
};

}  // namespace mb
