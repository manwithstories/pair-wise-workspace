#include <catch2/catch_test_macros.hpp>

#include <algorithm>
#include <cstdint>
#include <vector>

#include "byte_io.hpp"
#include "keyframe_index.hpp"
#include "pipeline.hpp"
#include "sample_generator.hpp"

using namespace mfs;

namespace {

GeneratorOptions pipeline_options() {
  GeneratorOptions options;
  options.seed = 21;
  options.frames = 120;
  options.width = 64;
  options.height = 36;
  options.keyframe_interval = 12;
  options.loss_events = 3;
  options.loss_burst = 4;
  options.duplicate_rate = 0.08;
  options.reorder_rate = 0.2;
  options.reorder_window = 12;
  options.timestamp_defects = 6;
  return options;
}

std::vector<uint8_t> make_bytes(const GeneratorOptions& options) {
  std::vector<Packet> packets;
  generate_sample(options, packets);
  return encode_container(packets, 42);
}

}  // namespace

TEST_CASE("clean pipeline run meets all acceptance metrics", "[pipeline]") {
  const auto bytes = make_bytes(pipeline_options());
  const RepairResult result = run_pipeline(bytes);

  CHECK(result.first_error_offset < 0);
  CHECK(result.errors.empty());
  CHECK(result.out_of_window_packets == 0);
  CHECK(result.checksum_errors == 0);
  CHECK(result.truncated_records == 0);
  CHECK(result.repaired_frames > 0);
  CHECK(result.duplicate_packets > 0);
  CHECK(result.lost_ranges > 0);
  CHECK(result.decode_errors == 0);
  CHECK(result.truncated_records == 0);
  CHECK(result.roundtrip_match_rate >= 0.99);
  CHECK(result.keyframe_index_hit_rate >= 0.98);
  CHECK(result.segment_hash_match_rate >= 1.0);
  CHECK(result.timestamps.strictly_increasing);
  CHECK(meets_thresholds(result, AcceptanceThresholds{}));
}

TEST_CASE("packets beyond a narrow reorder window are reported with offsets",
          "[pipeline]") {
  // The clip is shuffled by up to 12 slots; a static window of 2 cannot absorb
  // that, so late packets must surface as OutOfWindow with their offsets.
  PipelineOptions options;
  options.reorder.base_window = 2;
  options.reorder.adaptive_window = false;
  const RepairResult result = run_pipeline(make_bytes(pipeline_options()), options);
  CHECK(result.out_of_window_packets > 0);
  CHECK(result.first_error_offset >= 0);
  CHECK(std::any_of(result.errors.begin(), result.errors.end(),
                    [](const RepairError& e) {
                      return e.type == ErrorType::OutOfWindow && e.offset > 0;
                    }));
  CHECK(result.repaired_frames > 0);
}

TEST_CASE("repaired frames are strictly increasing and keep raw clocks",
          "[pipeline]") {
  RepairResult result = run_pipeline(make_bytes(pipeline_options()));
  REQUIRE(result.frames.size() > 10);
  for (size_t i = 1; i < result.frames.size(); ++i) {
    CHECK(result.frames[i].ts_us > result.frames[i - 1].ts_us);
    CHECK(result.frames[i].seq > result.frames[i - 1].seq);
  }
  CHECK(result.timestamps.wrap_fixes > 0);
  CHECK(result.timestamps.missing_timestamps > 0);
  CHECK(result.timestamps.duplicate_timestamps > 0);
  CHECK(result.timestamps.backward_timestamps > 0);
}

TEST_CASE("decoded frames carry geometry and content digests", "[pipeline]") {
  const RepairResult result = run_pipeline(make_bytes(pipeline_options()));
  REQUIRE(!result.frames.empty());
  for (const RepairedFrame& frame : result.frames) {
    CHECK(frame.decoded);
    CHECK(frame.decoded_width == 64);
    CHECK(frame.decoded_height == 36);
    CHECK(frame.source_offset >= kFileHeaderSize);
    CHECK(frame.pts == microseconds_to_ticks(frame.ts_us, frame.time_base));
  }
}

TEST_CASE("a corrupted payload is reported by offset and later frames survive",
          "[pipeline]") {
  auto bytes = make_bytes(pipeline_options());
  // Locate the payload of a packet well inside the stream and flip one byte.
  // Body layout ends with the 32-byte checksum, so one byte before it is
  // payload as long as the payload is non-empty.
  PacketReader probe(bytes);
  Packet packet;
  size_t target = 0;
  size_t offset = 0;
  for (int index = 0; probe.next(packet); ++index) {
    if (index == 5) {
      offset = static_cast<size_t>(packet.raw_offset);
      target = offset + 8 + packet.record_length - kSha256Size - 1;
      REQUIRE(packet.payload.size() > 1);
      break;
    }
  }
  REQUIRE(target > 0);
  bytes[target] ^= 0xFF;

  const RepairResult result = run_pipeline(bytes);
  CHECK(result.checksum_errors == 1);
  CHECK(result.first_error_offset == static_cast<int64_t>(offset));
  CHECK(result.repaired_frames > 0);
  CHECK(result.roundtrip_match_rate == 1.0);  // rejected record is not counted
}

TEST_CASE("a truncated tail is reported without losing earlier frames",
          "[pipeline]") {
  auto bytes = make_bytes(pipeline_options());
  const size_t full_size = bytes.size();
  bytes.resize(full_size - 37);

  const RepairResult result = run_pipeline(bytes);
  CHECK(result.truncated_input);
  CHECK(result.truncated_records == 1);
  CHECK(result.first_error_offset > 0);
  CHECK(result.repaired_frames > 0);
  CHECK(result.segment_hash_match_rate == 1.0);
}

TEST_CASE("a truncated file header is unrecoverable", "[pipeline]") {
  std::vector<uint8_t> bytes{0x4D, 0x46, 0x53, 0x31};
  const RepairResult result = run_pipeline(bytes);
  CHECK(result.empty_sequence);
  CHECK(result.errors.front().type == ErrorType::FileHeaderTruncated);
  CHECK(result.first_error_offset == 0);
}

TEST_CASE("an empty file is unrecoverable", "[pipeline]") {
  const RepairResult result = run_pipeline(std::vector<uint8_t>{});
  CHECK(result.empty_sequence);
  CHECK_FALSE(result.errors.empty());
  CHECK(result.errors[0].type == ErrorType::FileHeaderTruncated);
}

TEST_CASE("a header-only file yields an empty sequence", "[pipeline]") {
  const auto bytes = make_bytes(pipeline_options());
  const std::vector<uint8_t> header(bytes.begin(), bytes.begin() + kFileHeaderSize);
  const RepairResult result = run_pipeline(header);
  CHECK(result.empty_sequence);
  CHECK(result.repaired_frames == 0);
  CHECK(result.errors.back().type == ErrorType::EmptySequence);
}

TEST_CASE("the segment budget is enforced", "[pipeline]") {
  PipelineOptions options;
  options.max_segments = 2;
  PipelineOptions reorder_options;
  const auto bytes = make_bytes(pipeline_options());
  const RepairResult result = run_pipeline(bytes, options);
  CHECK(result.index.size() == 2);
  CHECK(std::any_of(result.errors.begin(), result.errors.end(),
                    [](const RepairError& e) {
                      return e.type == ErrorType::SegmentLimit;
                    }));
}

TEST_CASE("a memory limit stops parsing with a typed error", "[pipeline]") {
  PipelineOptions options;
  options.parse.max_total_bytes = 4096;
  const auto bytes = make_bytes(pipeline_options());
  const RepairResult result = run_pipeline(bytes, options);
  CHECK(result.limit_hit);
  CHECK(std::any_of(result.errors.begin(), result.errors.end(),
                    [](const RepairError& e) {
                      return e.type == ErrorType::LimitExceeded;
                    }));
}

TEST_CASE("threshold evaluation fails when a metric is short", "[pipeline]") {
  RepairResult result;
  result.roundtrip_match_rate = 1.0;
  result.keyframe_index_hit_rate = 0.5;
  result.segment_hash_match_rate = 1.0;
  CHECK_FALSE(meets_thresholds(result, AcceptanceThresholds{}));
}
