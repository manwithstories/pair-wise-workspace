#include <catch2/catch_test_macros.hpp>

#include <algorithm>
#include <cstdint>
#include <vector>

#include "packet_reader.hpp"
#include "pipeline.hpp"
#include "sample_generator.hpp"
#include "sha256.hpp"

using namespace mfs;

namespace {

GeneratorOptions base_options() {
  GeneratorOptions options;
  options.seed = 11;
  options.frames = 90;
  options.width = 48;
  options.height = 27;
  options.keyframe_interval = 15;
  options.loss_events = 1;
  options.loss_burst = 2;
  options.duplicate_rate = 0.05;
  options.reorder_rate = 0.1;
  options.reorder_window = 8;
  options.timestamp_defects = 3;
  return options;
}

std::vector<Packet> sampled_packets() {
  std::vector<Packet> packets;
  generate_sample(base_options(), packets);
  return packets;
}

}  // namespace

TEST_CASE("a decode failure is recorded and later frames still decode",
          "[pipeline][errors]") {
  std::vector<Packet> packets = sampled_packets();
  REQUIRE(packets.size() > 12);
  // Replace one payload with garbage that still produces a matching checksum,
  // so the parser accepts the record and only FFmpeg can reject it.
  Packet& victim = packets[packets.size() / 2];
  const uint64_t victim_seq = victim.header.frame_seq;
  victim.payload.assign(64, 0xAB);
  victim.checksum = Sha256::hash(
      std::span<const uint8_t>(victim.payload.data(), victim.payload.size()));

  const auto bytes = encode_container(packets, 5);
  // The generator works on unparsed packets, so recover the record offset by
  // re-parsing the encoded container and matching the sequence number.
  uint64_t victim_offset = 0;
  {
    PacketReader reader(bytes);
    Packet parsed;
    while (reader.next(parsed)) {
      if (parsed.header.frame_seq == victim_seq &&
          parsed.payload == victim.payload) {
        victim_offset = parsed.raw_offset;
        break;
      }
    }
  }
  REQUIRE(victim_offset > 0);
  const RepairResult result = run_pipeline(bytes);

  CHECK(result.decode_errors == 1);
  const auto it = std::find_if(
      result.errors.begin(), result.errors.end(),
      [](const RepairError& e) { return e.type == ErrorType::DecodeError; });
  REQUIRE(it != result.errors.end());
  CHECK(it->offset == victim_offset);
  CHECK(it->record_index == victim_seq);

  // The damaged frame is excluded from the index, but the stream continues.
  const auto failed = std::find_if(
      result.frames.begin(), result.frames.end(),
      [victim_seq](const RepairedFrame& f) { return f.seq == victim_seq; });
  REQUIRE(failed != result.frames.end());
  CHECK_FALSE(failed->decoded);
  CHECK(std::count_if(result.frames.begin(), result.frames.end(),
                      [](const RepairedFrame& f) { return f.decoded; }) > 0);
  CHECK(result.segment_hash_match_rate == 1.0);
  CHECK((result.keyframe_index_hit_rate >= 0.98));
}

TEST_CASE("a stalled link widens the reorder window at pipeline level",
          "[pipeline][reorder]") {
  GeneratorOptions options = base_options();
  options.reorder_rate = 0.0;
  options.duplicate_rate = 0.0;
  options.loss_events = 0;
  // A long stall followed by a burst, expressed in frame slots.
  options.arrival_gaps = {24};

  std::vector<Packet> packets;
  generate_sample(options, packets);
  const auto bytes = encode_container(packets, 6);

  PipelineOptions pipeline;
  pipeline.reorder.adaptive_window = true;
  pipeline.reorder.base_window = 4;
  pipeline.reorder.max_window = 64;
  const RepairResult result = run_pipeline(bytes, pipeline);
  CHECK(result.repaired_frames > 0);
  CHECK(result.lost_ranges == 0);
  CHECK(result.out_of_window_packets == 0);
}

TEST_CASE("a fixed window with arrival gaps still recovers the stream",
          "[pipeline][reorder]") {
  GeneratorOptions options = base_options();
  options.reorder_rate = 0.0;
  options.duplicate_rate = 0.0;
  options.loss_events = 0;
  options.arrival_gaps = {20};
  std::vector<Packet> packets;
  generate_sample(options, packets);
  const auto bytes = encode_container(packets, 7);

  PipelineOptions pipeline;
  pipeline.reorder.adaptive_window = false;
  pipeline.reorder.base_window = 4;
  const RepairResult result = run_pipeline(bytes, pipeline);
  CHECK(result.repaired_frames > 0);
  CHECK(result.segment_hash_match_rate == 1.0);
}

TEST_CASE("the per-frame size limit rejects oversized payloads",
          "[pipeline][limits]") {
  std::vector<Packet> packets = sampled_packets();
  const size_t largest = std::max_element(
                             packets.begin(), packets.end(),
                             [](const Packet& a, const Packet& b) {
                               return a.payload.size() < b.payload.size();
                             })
                             ->payload.size();
  REQUIRE(largest > 16);

  const auto bytes = encode_container(packets, 8);
  PipelineOptions pipeline;
  pipeline.parse.max_frame_bytes = largest / 2;
  const RepairResult result = run_pipeline(bytes, pipeline);
  CHECK(result.limit_hit);
  CHECK(result.decode_errors == 0);
  CHECK(std::any_of(result.errors.begin(), result.errors.end(),
                    [](const RepairError& e) {
                      return e.type == ErrorType::LimitExceeded && e.offset > 0;
                    }));
}

TEST_CASE("repeated packets never inflate the frame count",
          "[pipeline][dedupe]") {
  GeneratorOptions options = base_options();
  options.duplicate_rate = 0.5;
  options.reorder_rate = 0.0;
  std::vector<Packet> packets;
  const GenerationStats stats = generate_sample(options, packets);
  REQUIRE(stats.duplicates > 0);

  const auto bytes = encode_container(packets, 9);
  const RepairResult result = run_pipeline(bytes);
  CHECK(result.duplicate_packets == stats.duplicates);
  // Every unique packet appears exactly once in the repaired stream.
  std::vector<uint64_t> seqs;
  for (const RepairedFrame& frame : result.frames) {
    seqs.push_back(frame.seq);
  }
  CHECK(std::adjacent_find(seqs.begin(), seqs.end()) == seqs.end());
  CHECK(std::is_sorted(seqs.begin(), seqs.end()));
}

TEST_CASE("no frames are fabricated for lost sequence ranges",
          "[pipeline][loss]") {
  const auto bytes = encode_container(sampled_packets(), 10);
  const RepairResult result = run_pipeline(bytes);
  REQUIRE(!result.lost.empty());
  for (const LostRange& range : result.lost) {
    for (const RepairedFrame& frame : result.frames) {
      const bool inside =
          frame.seq >= range.first_seq && frame.seq <= range.last_seq;
      CHECK_FALSE(inside);
    }
    CHECK(range.first_seq <= range.last_seq);
    CHECK((range.estimated_frames == range.last_seq - range.first_seq + 1));
    CHECK(range.estimated_start_us <= range.estimated_end_us);
  }
}
