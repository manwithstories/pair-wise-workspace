#include <catch2/catch_test_macros.hpp>

#include <limits>
#include <string>
#include <vector>

#include "error.hpp"
#include "hash.hpp"
#include "json.hpp"
#include "manifest.hpp"
#include "rational.hpp"

TEST_CASE("error codes map to the documented exit codes", "[error]") {
  using media_restore::ErrorCode;
  CHECK(media_restore::exit_code_for(ErrorCode::usage) == 1);
  CHECK(media_restore::exit_code_for(ErrorCode::json_parse) == 1);
  CHECK(media_restore::exit_code_for(ErrorCode::index_order) == 2);
  CHECK(media_restore::exit_code_for(ErrorCode::dependency_cycle) == 3);
  CHECK(media_restore::exit_code_for(ErrorCode::audio_unsupported_layout) == 3);
}

TEST_CASE("diagnostics render stable fields", "[error]") {
  media_restore::Diagnostic diag;
  diag.code = media_restore::ErrorCode::index_negative_offset;
  diag.message = "offset must not be negative";
  diag.location.file = "media/reel.h264";
  diag.location.byte_offset = 42;
  diag.location.track = "reel_video";
  const std::string text = media_restore::format_diagnostic(diag);
  CHECK(text.find("code=E_INDEX_NEGATIVE_OFFSET") != std::string::npos);
  CHECK(text.find("track=reel_video") != std::string::npos);
  CHECK(text.find("offset=42") != std::string::npos);
}

TEST_CASE("rational time keeps exact integer ticks", "[rational]") {
  using media_restore::Rational;
  int64_t ticks = 0;
  // 1/3 second at a 90 kHz tick is 30000 ticks exactly.
  REQUIRE(Rational::exact_ticks(Rational(1, 3), Rational(1, 90000), &ticks));
  CHECK(ticks == 30000);
  // 1001/30000 s at 48 kHz needs flooring.
  REQUIRE(Rational::floor_ticks(Rational(1001, 30000), Rational(1, 48000), &ticks));
  CHECK(ticks == 1601);
  REQUIRE(Rational::sample_count(Rational(3, 50), 48000, &ticks));
  CHECK(ticks == 2880);
}

TEST_CASE("rational overflow is rejected, not wrapped", "[rational]") {
  using media_restore::Rational;
  const int64_t big = std::numeric_limits<int64_t>::max();
  Rational sum;
  CHECK_FALSE(Rational::add(Rational(big, 2), Rational(big, 3), &sum));
  int64_t ticks = 0;
  CHECK_FALSE(Rational::exact_ticks(Rational(big, 1), Rational(1, big), &ticks));
  CHECK_FALSE(Rational(1, 0).valid());
}

TEST_CASE("json round-trips with stable key order", "[json]") {
  const std::string text = R"({"b": 1, "a": [true, null, "xé"], "c": -2.5})";
  std::string error;
  size_t offset = 0;
  const auto doc = media_restore::Json::parse(text, &error, &offset);
  REQUIRE(error.empty());
  REQUIRE(doc.is_object());
  CHECK(doc.members()[0].first == "b");
  CHECK(doc.members()[1].first == "a");
  CHECK(doc.find("c")->as_number() == -2.5);
  const auto again = media_restore::Json::parse(doc.dump(), &error);
  CHECK(again == doc);
}

TEST_CASE("json rejects malformed input with an offset", "[json]") {
  std::string error;
  size_t offset = 99;
  const auto doc = media_restore::Json::parse(R"({"a": 1,})", &error, &offset);
  CHECK(doc.is_null());
  CHECK_FALSE(error.empty());
  CHECK(offset == 8);
}

TEST_CASE("stable hash does not depend on run or pointer", "[hash]") {
  const auto a = media_restore::StableHash::hash_text("archive");
  const auto b = media_restore::StableHash::hash_text("archive");
  const auto c = media_restore::StableHash::hash_text("archivf");
  CHECK(a == b);
  CHECK(a != c);
  CHECK(a.size() == 16);
}

namespace {
const char* kSampleManifest = R"({
  "format": "media-archive-manifest",
  "version": 1,
  "time_base": {"num": "1", "den": "90000"},
  "tracks": [
    {"id": "reel_video", "kind": "video", "depends_on": [],
     "segments": [
       {"id": "seg-0001", "start": "0/1", "duration": "3/1",
        "media": "media/reel_a.h264", "codec": "h264",
        "index": {"path": "media/reel_a.idx", "version": 1,
                  "entries": [{"tick": 0, "offset": 0, "keyframe": true},
                              {"tick": 45000, "offset": 1200, "keyframe": true}]},
        "color": {"primaries": "bt709", "transfer": "bt709", "source": "tape"}}
     ]},
    {"id": "reel_audio", "kind": "audio", "depends_on": ["reel_video"],
     "segments": [
       {"id": "seg-0001", "start": "0/1", "duration": "3/1",
        "media": "media/reel_a.wav", "codec": "h264",
        "color": {"source": "none"},
        "audio": {"sample_rate": 44100, "layout": "stereo", "offset": "-1/100"}}
     ]}
  ]
})";
}  // namespace

TEST_CASE("manifest round-trips exactly", "[manifest]") {
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  const bool parsed = media_restore::parse_manifest(kSampleManifest, &manifest, &diagnostics);
  REQUIRE(parsed);
  CHECK(diagnostics.empty());
  REQUIRE(manifest.tracks.size() == 2);
  CHECK(manifest.time_base.num == 1);
  CHECK(manifest.time_base.den == 90000);
  const auto* segment = manifest.find_segment("reel_video", "seg-0001");
  REQUIRE(segment != nullptr);
  CHECK(segment->index.entries.size() == 2);
  CHECK(segment->color.primaries.value() == "bt709");

  const std::string text = media_restore::serialize_manifest(manifest);
  media_restore::Manifest again;
  REQUIRE(media_restore::parse_manifest(text, &again, &diagnostics));
  CHECK(media_restore::serialize_manifest(again) == text);
}

TEST_CASE("manifest rejects unknown codec and bad index version", "[manifest]") {
  std::string text = kSampleManifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  media_restore::Manifest manifest;
  auto unknown = text;
  const auto pos = unknown.find("\"h264\"");
  unknown.replace(pos, 6, "\"h264x\"");
  CHECK_FALSE(media_restore::parse_manifest(unknown, &manifest, &diagnostics));
  REQUIRE_FALSE(diagnostics.empty());
  CHECK(diagnostics.back().code == media_restore::ErrorCode::unknown_codec);
  CHECK(diagnostics.back().location.segment == "seg-0001");

  diagnostics.clear();
  auto bad_index = text;
  const std::string key = "\"version\": ";
  const auto idx = bad_index.find(key, bad_index.find("\"index\""));
  bad_index.replace(idx, key.size() + 1, "\"version\": 9");
  CHECK_FALSE(media_restore::parse_manifest(bad_index, &manifest, &diagnostics));
  REQUIRE_FALSE(diagnostics.empty());
  CHECK(diagnostics.back().code == media_restore::ErrorCode::index_version);
}

TEST_CASE("manifest reports a JSON error offset", "[manifest]") {
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  CHECK_FALSE(media_restore::parse_manifest("{\"format\": ", &manifest, &diagnostics));
  REQUIRE_FALSE(diagnostics.empty());
  CHECK(diagnostics.back().code == media_restore::ErrorCode::json_parse);
  CHECK(diagnostics.back().location.byte_offset >= 0);
}
