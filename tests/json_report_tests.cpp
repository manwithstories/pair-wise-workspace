#include <catch2/catch_test_macros.hpp>

#include <string>

#include "json_report.hpp"
#include "pipeline.hpp"
#include "sha256.hpp"

using namespace mfs;

namespace {

RepairResult sample_result() {
  RepairResult result;
  result.roundtrip_match_rate = 0.995;
  result.keyframe_index_hit_rate = 0.996667;
  result.segment_hash_match_rate = 1.0;
  result.repaired_frames = 1100;
  result.duplicate_packets = 99;
  result.lost_ranges = 20;
  result.decode_errors = 2;
  result.first_error_offset = 25221;
  IndexEntry entry;
  entry.segment_id = 0;
  entry.keyframe_seq = 0;
  entry.start_ts_us = 1000;
  entry.end_ts_us = 4000;
  entry.pts = 90;
  entry.frame_count = 3;
  entry.source_offset = 32;
  entry.segment_sha256 = Sha256::hash("segment");
  result.index.push_back(entry);
  result.lost.push_back(LostRange{5, 7, 10, 100, 3});
  result.errors.push_back(
      RepairError{ErrorType::OutOfWindow, 4096, 3, "late \"quoted\" packet"});
  RepairedFrame frame;
  frame.seq = 4;
  frame.ts_us = 5000;
  frame.original_ts_us = 5000;
  frame.decoded = true;
  frame.source_offset = 200;
  frame.frame_sha256 = Sha256::hash("frame");
  result.frames.push_back(frame);
  return result;
}

}  // namespace

TEST_CASE("report contains every summary metric", "[json]") {
  const std::string json = report_json(sample_result(), PipelineOptions{});
  CHECK(json.find("\"metrics\": {") != std::string::npos);
  CHECK(json.find("\"roundtrip_match_rate\": 0.995000") != std::string::npos);
  CHECK(json.find("\"keyframe_index_hit_rate\": 0.996667") != std::string::npos);
  CHECK(json.find("\"segment_hash_match_rate\": 1.000000") != std::string::npos);
  CHECK(json.find("\"first_error_offset\": 25221") != std::string::npos);
  CHECK(json.find(Sha256::hex(Sha256::hash("segment"))) != std::string::npos);
  CHECK(json.find("\\\"quoted\\\"") != std::string::npos);
}

TEST_CASE("report metrics round-trip through the parser", "[json]") {
  const RepairResult result = sample_result();
  const std::string json = report_json(result, PipelineOptions{});
  ReportMetrics metrics;
  REQUIRE(parse_report_metrics(json, metrics));
  CHECK(metrics.roundtrip_match_rate == result.roundtrip_match_rate);
  CHECK(metrics.keyframe_index_hit_rate == result.keyframe_index_hit_rate);
  CHECK(metrics.segment_hash_match_rate == result.segment_hash_match_rate);
  CHECK(metrics.repaired_frames == result.repaired_frames);
  CHECK(metrics.duplicate_packets == result.duplicate_packets);
  CHECK(metrics.lost_ranges == result.lost_ranges);
  CHECK(metrics.decode_errors == result.decode_errors);
  CHECK(metrics.first_error_offset == result.first_error_offset);
}

TEST_CASE("an empty report is rejected", "[json]") {
  ReportMetrics metrics;
  CHECK_FALSE(parse_report_metrics("{}", metrics));
  CHECK_FALSE(parse_report_metrics("not json", metrics));
  CHECK_FALSE(parse_report_metrics(
      "{\"roundtrip_match_rate\": 1.0}", metrics));
}

TEST_CASE("arrays and nested objects are well-formed", "[json]") {
  const std::string json = report_json(sample_result(), PipelineOptions{});
  // Structural sanity: balanced braces and brackets, no trailing commas.
  int braces = 0;
  int brackets = 0;
  for (size_t i = 0; i < json.size(); ++i) {
    const char c = json[i];
    if (c == '{') {
      ++braces;
    } else if (c == '}') {
      --braces;
      CHECK(braces >= 0);
    } else if (c == '[') {
      ++brackets;
    } else if (c == ']') {
      --brackets;
      CHECK(brackets >= 0);
    } else if (c == ',' && i + 1 < json.size()) {
      const char next = json[i + 1];
      CHECK(next != '}');
      CHECK(next != ']');
    }
  }
  CHECK(braces == 0);
  CHECK(brackets == 0);
  CHECK(json.front() == '{');
  CHECK(json.back() == '}');
  // Every object member is either a key/value or a nested container: `{`
  // must be followed by a newline+quote (object) or `"` (array item).
  for (size_t i = 0; i + 1 < json.size(); ++i) {
    if (json[i] == '{' && json[i + 1] != '\n' && json[i + 1] != '}') {
      FAIL("object opened without a key: " << json.substr(i, 40));
    }
  }
}
