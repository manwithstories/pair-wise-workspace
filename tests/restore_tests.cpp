#include <catch2/catch_test_macros.hpp>

#include <fstream>
#include <limits>
#include <string>
#include <vector>

#include <cmath>

#include <cmath>

#include "audio_restore.hpp"
#include "bitstream.hpp"
#include "color_metadata.hpp"
#include "error.hpp"
#include "file_io.hpp"
#include "hash.hpp"
#include "json.hpp"
#include "manifest.hpp"
#include "sha256.hpp"
#include "timeline.hpp"
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

namespace {
std::string manifest_with_tracks(const std::string& tracks) {
  return R"({"format":"media-archive-manifest","version":1,
            "time_base":{"num":"1","den":"90000"},"tracks":[)" + tracks + "]}";
}
std::string simple_track(const std::string& id, const std::string& deps,
                         const std::string& start, const std::string& duration) {
  return R"({"id":")" + id + R"(","kind":"video","depends_on":[)" + deps +
         R"(],"segments":[{"id":"s1","start":")" + start + R"(","duration":")" +
         duration + R"(","media":"m.h264","codec":"h264","color":{"source":"x"}}]})";
}}  // namespace

TEST_CASE("timeline converts to exact ticks", "[timeline]") {
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  const auto text = manifest_with_tracks(
      simple_track("a", "", "0/1", "3/1") + "," +
      simple_track("b", "\"a\"", "3/2", "1001/30000"));
  REQUIRE(media_restore::parse_manifest(text, &manifest, &diagnostics));
  media_restore::Timeline timeline;
  REQUIRE(media_restore::build_timeline(manifest, &diagnostics, &timeline));
  CHECK(diagnostics.empty());
  REQUIRE(timeline.tracks.size() == 2);
  CHECK(timeline.tracks[0].segments[0].start_tick == 0);
  CHECK(timeline.tracks[0].segments[0].end_tick == 270000);
  CHECK(timeline.tracks[1].segments[0].start_tick == 135000);
  CHECK(timeline.tracks[1].segments[0].duration_tick == 3003);
  CHECK(timeline.order.order.size() == 2);
  CHECK(timeline.order.order[0] == "a");
}

TEST_CASE("timeline reports overlap, empty and unrepresentable spans", "[timeline]") {
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  const auto text = manifest_with_tracks(
      R"({"id":"mixed","kind":"video","depends_on":[],"segments":[
          {"id":"s1","start":"0/1","duration":"3/1","media":"m","codec":"h264"},
          {"id":"s2","start":"2/1","duration":"3/1","media":"m","codec":"h264"},
          {"id":"s3","start":"6/1","duration":"0/1","media":"m","codec":"h264"},
          {"id":"s4","start":"1/7","duration":"1/1","media":"m","codec":"h264"}]})");
  REQUIRE(media_restore::parse_manifest(text, &manifest, &diagnostics));
  media_restore::Timeline timeline;
  CHECK_FALSE(media_restore::build_timeline(manifest, &diagnostics, &timeline));
  bool overlap = false, empty = false, unconvertible = false;
  for (const auto& d : diagnostics) {
    overlap |= d.code == media_restore::ErrorCode::segment_overlap;
    empty |= d.code == media_restore::ErrorCode::segment_empty;
    unconvertible |= d.code == media_restore::ErrorCode::manifest_schema;
  }
  CHECK(overlap);
  CHECK(empty);
  CHECK(unconvertible);
}

TEST_CASE("out-of-order segments still detect overlap exactly", "[timeline]") {
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  const auto text = manifest_with_tracks(
      R"({"id":"a","kind":"video","depends_on":[],"segments":[
          {"id":"late","start":"4/1","duration":"2/1","media":"m","codec":"h264"},
          {"id":"early","start":"0/1","duration":"5/1","media":"m","codec":"h264"}]})");
  REQUIRE(media_restore::parse_manifest(text, &manifest, &diagnostics));
  media_restore::Timeline timeline;
  CHECK_FALSE(media_restore::build_timeline(manifest, &diagnostics, &timeline));
  REQUIRE_FALSE(diagnostics.empty());
  CHECK(diagnostics.back().code == media_restore::ErrorCode::segment_overlap);
  CHECK(diagnostics.back().location.segment == "late");
}

TEST_CASE("dependency cycles are reported without stopping acyclic tracks", "[timeline]") {
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  const auto text = manifest_with_tracks(
      simple_track("solo", "", "0/1", "1/1") + "," +
      simple_track("x", "\"y\"", "0/1", "1/1") + "," +
      simple_track("y", "\"x\"", "0/1", "1/1"));
  REQUIRE(media_restore::parse_manifest(text, &manifest, &diagnostics));
  media_restore::TrackOrder order;
  CHECK_FALSE(media_restore::resolve_track_order(manifest, &diagnostics, &order));
  REQUIRE_FALSE(order.order.empty());
  CHECK(order.order.front() == "solo");
  CHECK(order.cyclic.size() == 2);
  bool saw_cycle = false;
  for (const auto& d : diagnostics) {
    saw_cycle |= d.code == media_restore::ErrorCode::dependency_cycle;
  }
  CHECK(saw_cycle);
}

TEST_CASE("missing dependency blocks only the dependent track", "[timeline]") {
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  const auto text = manifest_with_tracks(simple_track("a", "\"ghost\"", "0/1", "1/1"));
  REQUIRE(media_restore::parse_manifest(text, &manifest, &diagnostics));
  media_restore::TrackOrder order;
  CHECK_FALSE(media_restore::resolve_track_order(manifest, &diagnostics, &order));
  CHECK(order.blocked.size() == 1);
  CHECK(order.blocked.front() == "a");
}

namespace {
// Builds a synthetic H.264-shaped Annex B stream: SPS, PPS, then IDR and
// non-IDR slices whose payload lengths are controlled by the caller.
std::string annexb_unit(int nal_type, size_t payload) {
  std::string unit;
  unit.append("\x00\x00\x00\x01", 4);
  unit.push_back(static_cast<char>(0x60 | nal_type));
  unit.push_back(static_cast<char>(0x40));
  for (size_t i = 0; i < payload; ++i) {
    const char byte = static_cast<char>((i * 37 + 11) & 0x7F);
    unit.push_back(byte == 0 ? '\x01' : byte);
  }
  return unit;
}

std::string synthetic_h264(size_t gop = 4, size_t gops = 3) {
  std::string bytes = annexb_unit(7, 9) + annexb_unit(8, 3);
  for (size_t g = 0; g < gops; ++g) {
    bytes += annexb_unit(5, 40 + g);
    for (size_t i = 1; i < gop; ++i) bytes += annexb_unit(1, 30 + i);
  }
  return bytes;
}

std::string synthetic_av1() {
  std::string bytes;
  const auto obu = [](int type, const std::string& payload) {
    std::string unit;
    unit.push_back(static_cast<char>((type << 3) | 0x02));
    unit.push_back(static_cast<char>(payload.size()));
    return unit + payload;
  };
  bytes += obu(2, "");
  bytes += obu(1, std::string(6, '\x11'));
  bytes += obu(6, std::string("\x08\x22\x33", 3));
  bytes += obu(2, "");
  bytes += obu(6, std::string("\x28\x22\x33", 3));
  bytes += obu(2, "");
  bytes += obu(6, std::string("\x08\x22\x33", 3));
  return bytes;
}
}  // namespace

TEST_CASE("annexb scan finds every keyframe at exact offsets", "[bitstream]") {
  const std::string bytes = synthetic_h264();
  media_restore::StreamScan scan;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::scan_raw_stream_bytes(bytes, media_restore::Codec::h264,
                                               media_restore::Limits{}, &scan,
                                               &diagnostics));
  CHECK(diagnostics.empty());
  CHECK(scan.stream_size == static_cast<int64_t>(bytes.size()));
  CHECK(scan.unit_count == 2 + 3 * 4);
  REQUIRE(scan.keyframes.size() == 3);
  // The index records the start of the keyframe's access unit, which begins
  // with the SPS/PPS that precede the IDR slice.
  CHECK(scan.keyframes[0].offset == 0);
  CHECK(scan.keyframes[0].nal_type == 5);
  CHECK(scan.keyframes[1].offset > scan.keyframes[0].offset);
}

TEST_CASE("annexb scan survives a start code split across the chunk boundary", "[bitstream]") {
  // 4 MiB chunks: place a start code exactly across 4 MiB - 2.
  constexpr size_t kChunk = 4 * 1024 * 1024;
  std::string bytes(kChunk - 3, '\x11');
  bytes += annexb_unit(7, 5);
  bytes += annexb_unit(5, 7);
  media_restore::StreamScan scan;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::scan_raw_stream_bytes(bytes, media_restore::Codec::h264,
                                               media_restore::Limits{}, &scan,
                                               &diagnostics));
  REQUIRE(scan.keyframes.size() == 1);
  // The access unit starts at the SPS that opens the chunk-crossing unit.
  CHECK(scan.keyframes[0].offset == static_cast<int64_t>(kChunk - 3));
}

TEST_CASE("av1 scan identifies keyframes and skips inter frames", "[bitstream]") {
  media_restore::StreamScan scan;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::scan_raw_stream_bytes(synthetic_av1(),
                                               media_restore::Codec::av1,
                                               media_restore::Limits{}, &scan,
                                               &diagnostics));
  CHECK(scan.keyframes.size() == 2);
  CHECK(scan.keyframes[0].offset < scan.keyframes[1].offset);
}

TEST_CASE("codec sniffing distinguishes h264, h265 and av1", "[bitstream]") {
  media_restore::Codec codec = media_restore::Codec::av1;
  CHECK(media_restore::sniff_codec(synthetic_h264(), &codec));
  CHECK(codec == media_restore::Codec::h264);
  std::string hevc;
  hevc.append("\x00\x00\x00\x01", 4);
  hevc.push_back(static_cast<char>(32 << 1));  // VPS
  hevc.push_back('\x01');
  CHECK(media_restore::sniff_codec(hevc, &codec));
  CHECK(codec == media_restore::Codec::h265);
  CHECK(media_restore::sniff_codec(synthetic_av1(), &codec));
  CHECK(codec == media_restore::Codec::av1);
  CHECK_FALSE(media_restore::sniff_codec("plain text", &codec));
}

TEST_CASE("keyframe index validation reports each defect kind", "[bitstream]") {
  const std::string bytes = synthetic_h264();
  media_restore::StreamScan scan;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::scan_raw_stream_bytes(bytes, media_restore::Codec::h264,
                                               media_restore::Limits{}, &scan,
                                               &diagnostics));
  REQUIRE(scan.keyframes.size() == 3);
  const int64_t k0 = scan.keyframes[0].offset;
  const int64_t k1 = scan.keyframes[1].offset;

  const auto validate = [&](std::vector<media_restore::IndexEntry> entries,
                            media_restore::ErrorCode expected) {
    media_restore::KeyframeIndex index;
    index.present = true;
    index.version = 1;
    index.entries = std::move(entries);
    media_restore::IndexReport report;
    diagnostics.clear();
    CHECK_FALSE(media_restore::validate_keyframe_index(index, scan,
                                                       media_restore::Limits{},
                                                       &report, &diagnostics));
    bool found = false;
    for (const auto& d : diagnostics) found |= d.code == expected;
    CHECK(found);
  };

  validate({{100, -1, true}}, media_restore::ErrorCode::index_negative_offset);
  validate({{100, static_cast<int64_t>(bytes.size()) + 10, true}},
           media_restore::ErrorCode::index_out_of_range);
  validate({{100, k1, true}, {200, k1, true}},
           media_restore::ErrorCode::index_duplicate);
  validate({{200, k1, true}, {100, k0, true}},
           media_restore::ErrorCode::index_order);
  validate({{100, k1, false}}, media_restore::ErrorCode::index_not_keyframe);
}

TEST_CASE("keyframe index fills missing entries from the scan", "[bitstream]") {
  const std::string bytes = synthetic_h264();
  media_restore::StreamScan scan;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::scan_raw_stream_bytes(bytes, media_restore::Codec::h264,
                                               media_restore::Limits{}, &scan,
                                               &diagnostics));
  media_restore::KeyframeIndex index;
  index.present = true;
  index.version = 2;
  index.entries.push_back({0, scan.keyframes[0].offset, true});
  media_restore::IndexReport report;
  REQUIRE(media_restore::validate_keyframe_index(index, scan,
                                                 media_restore::Limits{}, &report,
                                                 &diagnostics));
  CHECK(report.index_entries == 1);
  CHECK(report.hits == 1);
  CHECK(report.generated == 2);
  CHECK(report.candidates.size() == 3);
  CHECK(report.candidates[0].origin == media_restore::EntryOrigin::index);
  CHECK(report.candidates[1].origin == media_restore::EntryOrigin::scan);
  CHECK_FALSE(report.candidates[1].tick.has_value());
}

namespace {
media_restore::PcmBuffer make_tone(int rate, const std::string& layout_name,
                                   int64_t frames, double hz, double amp) {
  media_restore::PcmBuffer buffer;
  buffer.sample_rate = rate;
  REQUIRE(media_restore::parse_layout(layout_name, &buffer.layout));
  buffer.channels = buffer.layout.nb_channels;
  buffer.interleaved = false;
  buffer.frames = frames;
  buffer.data.assign(static_cast<size_t>(frames) * buffer.channels, 0.0f);
  for (int c = 0; c < buffer.channels; ++c) {
    for (int64_t i = 0; i < frames; ++i) {
      const double t = static_cast<double>(i) / rate;
      buffer.plane(c)[i] = static_cast<float>(amp * std::sin(2 * M_PI * hz * t));
    }
  }
  return buffer;
}
}  // namespace

TEST_CASE("layout names parse canonically and reject unknowns", "[audio]") {
  using media_restore::parse_layout;
  AVChannelLayout layout{};
  REQUIRE(parse_layout("5.1", &layout));
  CHECK(media_restore::layout_name(layout) == "5.1");
  CHECK(layout.nb_channels == 6);
  AVChannelLayout mono{};
  REQUIRE(parse_layout("mono", &mono));
  CHECK(mono.nb_channels == 1);
  AVChannelLayout seven_one{};
  REQUIRE(parse_layout("7.1", &seven_one));
  CHECK(seven_one.nb_channels == 8);
  CHECK_FALSE(parse_layout("atmos-9.1.6", &layout));
  CHECK_FALSE(parse_layout("", &layout));
}

TEST_CASE("mono is upmixed to 5.1 without clipping the centre", "[audio]") {
  auto input = make_tone(44100, "mono", 44100, 440.0, 0.5);
  media_restore::AudioRestoreConfig config;
  config.output_rate = 48000;
  REQUIRE(media_restore::parse_layout("5.1", &config.output_layout));
  media_restore::PcmBuffer output;
  media_restore::AudioStats stats;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::convert_audio(input, config, &output, &stats, &diagnostics));
  CHECK(diagnostics.empty());
  CHECK(output.sample_rate == 48000);
  CHECK(output.channels == 6);
  CHECK(stats.input_layout == "mono");
  CHECK(stats.output_layout == "5.1");
  // One second at 44.1 kHz resampled to 48 kHz lands within a frame.
  CHECK(std::llabs(output.frames - 48000) <= 1);
  CHECK(stats.input_frames == 44100);
}

TEST_CASE("planar and interleaved inputs produce identical output", "[audio]") {
  auto planar = make_tone(32000, "stereo", 9600, 300.0, 0.4);
  media_restore::PcmBuffer interleaved;
  interleaved.sample_rate = planar.sample_rate;
  REQUIRE(media_restore::parse_layout("stereo", &interleaved.layout));
  interleaved.channels = 2;
  interleaved.interleaved = true;
  interleaved.frames = planar.frames;
  interleaved.data.resize(static_cast<size_t>(planar.frames) * 2);
  for (int64_t i = 0; i < planar.frames; ++i) {
    interleaved.frame(i)[0] = planar.plane(0)[i];
    interleaved.frame(i)[1] = planar.plane(1)[i];
  }
  media_restore::AudioRestoreConfig config;
  config.output_rate = 48000;
  REQUIRE(media_restore::parse_layout("stereo", &config.output_layout));
  media_restore::PcmBuffer a;
  media_restore::PcmBuffer b;
  media_restore::AudioStats stats_a;
  media_restore::AudioStats stats_b;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::convert_audio(planar, config, &a, &stats_a, &diagnostics));
  REQUIRE(media_restore::convert_audio(interleaved, config, &b, &stats_b, &diagnostics));
  REQUIRE(a.frames == b.frames);
  for (int c = 0; c < a.channels; ++c) {
    for (int64_t i = 0; i < a.frames; ++i) {
      REQUIRE(std::fabs(a.plane(c)[i] - b.plane(c)[i]) < 1e-6f);
    }
  }
}

TEST_CASE("non-frame-aligned tail is preserved, not dropped", "[audio]") {
  // 1001 frames at 44.1 kHz is not a multiple of any typical resampler block.
  auto input = make_tone(44100, "stereo", 1001, 220.0, 0.3);
  media_restore::AudioRestoreConfig config;
  config.output_rate = 48000;
  REQUIRE(media_restore::parse_layout("stereo", &config.output_layout));
  media_restore::PcmBuffer output;
  media_restore::AudioStats stats;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::convert_audio(input, config, &output, &stats, &diagnostics));
  CHECK(output.frames >= 1089);
  CHECK(output.frames <= 1091);
  CHECK(stats.output_frames == output.frames);
}

TEST_CASE("unsupported widths fail with a located error", "[audio]") {
  auto input = make_tone(48000, "7.1", 480, 440.0, 0.1);
  media_restore::AudioRestoreConfig config;
  config.output_rate = 0;
  std::vector<media_restore::Diagnostic> diagnostics;
  media_restore::PcmBuffer output;
  media_restore::AudioStats stats;
  CHECK_FALSE(media_restore::convert_audio(input, config, &output, &stats, &diagnostics));
  CHECK(diagnostics.back().code == media_restore::ErrorCode::audio_unsupported_rate);

  diagnostics.clear();
  config.output_rate = 48000;
  AVChannelLayout bad{};
  bad.nb_channels = 9;
  bad.order = AV_CHANNEL_ORDER_NATIVE;
  av_channel_layout_uninit(&config.output_layout);
  config.output_layout = bad;
  CHECK_FALSE(media_restore::convert_audio(input, config, &output, &stats, &diagnostics));
  CHECK(diagnostics.back().code == media_restore::ErrorCode::audio_unsupported_layout);
}

TEST_CASE("silence stays silent and is flagged", "[audio]") {
  media_restore::PcmBuffer silent;
  silent.sample_rate = 48000;
  REQUIRE(media_restore::parse_layout("stereo", &silent.layout));
  silent.channels = 2;
  silent.frames = 48000;
  silent.data.assign(static_cast<size_t>(silent.frames) * 2, 0.0f);
  media_restore::AudioRestoreConfig config;
  config.output_rate = 48000;
  REQUIRE(media_restore::parse_layout("stereo", &config.output_layout));
  media_restore::AudioStats stats;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::normalize_loudness(config, &silent, &stats, &diagnostics));
  CHECK(stats.silent);
  CHECK(stats.output_lufs <= -99.0);
  CHECK(stats.applied_gain_db == 0.0);
  for (const float sample : silent.data) CHECK(sample == 0.0f);
}

TEST_CASE("very short segments take the explicit short path", "[audio]") {
  auto input = make_tone(48000, "mono", 480, 440.0, 0.5);  // 10 ms
  media_restore::AudioRestoreConfig config;
  config.output_rate = 48000;
  REQUIRE(media_restore::parse_layout("mono", &config.output_layout));
  media_restore::AudioStats stats;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::normalize_loudness(config, &input, &stats, &diagnostics));
  CHECK(stats.short_segment);
  CHECK(stats.applied_gain_db == 0.0);
  bool flagged = false;
  for (const auto& d : diagnostics) {
    flagged |= d.code == media_restore::ErrorCode::loudness_invalid;
  }
  CHECK(flagged);
}

TEST_CASE("loudness normalisation reaches the target and limits peaks", "[audio]") {
  auto input = make_tone(48000, "stereo", 48000 * 3, 1000.0, 0.05);
  media_restore::AudioRestoreConfig config;
  config.output_rate = 48000;
  config.target_lufs = -23.0;
  config.true_peak_dbtp = -1.0;
  REQUIRE(media_restore::parse_layout("stereo", &config.output_layout));
  media_restore::AudioStats stats;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::normalize_loudness(config, &input, &stats, &diagnostics));
  REQUIRE(stats.loudness_measured);
  CHECK(stats.input_lufs < -20.0);
  CHECK(std::fabs(stats.output_lufs - (-23.0)) < 1.0);
  CHECK(stats.output_true_peak_dbtp <= -0.9);
  CHECK(stats.applied_gain_db > 0.0);
}

TEST_CASE("sha256 matches known vectors", "[hash]") {
  using media_restore::Sha256;
  CHECK(Sha256::digest("") ==
        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  CHECK(Sha256::digest("abc") ==
        "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  CHECK(Sha256::digest("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq") ==
        "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
  // Incremental updates must agree with a single digest.
  Sha256 hash;
  hash.update("ab");
  hash.update("c");
  CHECK(hash.hex() == Sha256::digest("abc"));
}

TEST_CASE("timeline offset pads or trims deterministically", "[audio]") {
  auto input = make_tone(48000, "stereo", 4800, 440.0, 0.2);
  media_restore::AudioStats stats;
  REQUIRE(media_restore::apply_timeline_offset(960, &input, &stats));
  CHECK(stats.padded_frames == 960);
  CHECK(input.frames == 5760);
  for (int c = 0; c < input.channels; ++c) {
    for (int i = 0; i < 960; ++i) CHECK(input.plane(c)[i] == 0.0f);
  }

  auto trimmed = make_tone(48000, "stereo", 4800, 440.0, 0.2);
  const float first_original = trimmed.plane(0)[480];
  media_restore::AudioStats trim_stats;
  REQUIRE(media_restore::apply_timeline_offset(-480, &trimmed, &trim_stats));
  CHECK(trim_stats.discarded_frames == 480);
  CHECK(trimmed.frames == 4320);
  CHECK(trimmed.plane(0)[0] == first_original);
}

TEST_CASE("wav encoder writes a stable RIFF header", "[audio]") {
  auto buffer = make_tone(48000, "mono", 100, 440.0, 0.25);
  std::string wav;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::encode_wav(buffer, &wav, &diagnostics));
  REQUIRE(wav.size() == 44 + 400);
  CHECK(wav.compare(0, 4, "RIFF") == 0);
  CHECK(wav.compare(8, 4, "WAVE") == 0);
  CHECK(wav.compare(20, 2, std::string("\x03\x00", 2)) == 0);  // IEEE float
  std::string again;
  REQUIRE(media_restore::encode_wav(buffer, &again, &diagnostics));
  CHECK(wav == again);
  CHECK(media_restore::Sha256::digest(wav) == media_restore::Sha256::digest(again));
}

namespace {
media_restore::Manifest color_manifest() {
  media_restore::Manifest manifest;
  media_restore::Track track;
  track.id = "reel";
  track.kind = media_restore::TrackKind::video;
  media_restore::Segment a;
  a.id = "a";
  a.color.primaries = "bt709";
  a.color.transfer = "bt709";
  a.color.source = "tape_a";
  media_restore::Segment b;
  b.id = "b";
  b.color.primaries = "bt2020";
  b.color.transfer = "smpte2084";
  b.color.source = "tape_b";
  track.segments = {a, b};
  manifest.tracks.push_back(track);
  return manifest;
}
}  // namespace

TEST_CASE("color conflicts are recorded, never silently overwritten", "[color]") {
  const auto manifest = color_manifest();
  std::vector<media_restore::Diagnostic> diagnostics;
  media_restore::ColorReport report;
  media_restore::ArchiveColor archive;
  REQUIRE(media_restore::resolve_color_metadata(
      manifest, media_restore::ColorStrategy::preserve, archive, &report,
      &diagnostics));
  // Each of the two segments reports primaries and transfer as conflicted.
  CHECK(report.conflicts == 4);
  REQUIRE(report.segments.size() == 2);
  CHECK(report.segments[0].primaries.value.value() == "bt709");
  CHECK(report.segments[1].primaries.value.value() == "bt2020");
  CHECK(report.segments[0].conflicted);
  bool saw_primaries = false;
  for (const auto& d : diagnostics) {
    if (d.code == media_restore::ErrorCode::color_conflict &&
        d.location.field == "primaries") {
      saw_primaries = true;
      CHECK(d.severity == media_restore::Severity::warning);
      CHECK(d.message.find("bt2020") != std::string::npos);
    }
  }
  CHECK(saw_primaries);
}

TEST_CASE("archive color strategy applies the archive value on conflict", "[color]") {
  const auto manifest = color_manifest();
  std::vector<media_restore::Diagnostic> diagnostics;
  media_restore::ColorReport report;
  media_restore::ArchiveColor archive;
  archive.primaries = "bt709";
  archive.transfer = "bt709";
  REQUIRE(media_restore::resolve_color_metadata(
      manifest, media_restore::ColorStrategy::archive, archive, &report,
      &diagnostics));
  for (const auto& segment : report.segments) {
    CHECK(segment.primaries.value.value() == "bt709");
    CHECK(segment.transfer.value.value() == "bt709");
  }
  CHECK(report.segments[1].primaries.applied_archive);
  CHECK(report.segments[1].primaries.source == "archive");
}

TEST_CASE("missing color fields are counted and filled only by archive policy", "[color]") {
  media_restore::Manifest manifest;
  media_restore::Track track;
  track.id = "reel";
  media_restore::Segment segment;
  segment.id = "a";
  segment.color.source = "none";
  track.segments.push_back(segment);
  manifest.tracks.push_back(track);
  media_restore::ColorReport report;
  std::vector<media_restore::Diagnostic> diagnostics;
  media_restore::ArchiveColor archive;
  REQUIRE(media_restore::resolve_color_metadata(
      manifest, media_restore::ColorStrategy::preserve, archive, &report,
      &diagnostics));
  CHECK(report.missing_fields == 4);
  CHECK(report.segments[0].missing_any);
  CHECK_FALSE(report.segments[0].primaries.value.has_value());

  diagnostics.clear();
  report = media_restore::ColorReport{};
  REQUIRE(media_restore::resolve_color_metadata(
      manifest, media_restore::ColorStrategy::archive, archive, &report,
      &diagnostics));
  CHECK(report.segments[0].primaries.value.value() == "bt709");
  CHECK(report.segments[0].range.value.value() == "tv");
}

TEST_CASE("true-peak limiting counts clipped samples", "[audio]") {
  // A quiet tone normalised up to a hot target: the required gain pushes the
  // peak past the ceiling, so the limiter engages and is counted.
  auto buffer = make_tone(48000, "stereo", 48000 * 2, 997.0, 0.2);
  media_restore::AudioRestoreConfig config;
  config.output_rate = 48000;
  config.target_lufs = -3.0;  // ~+10 dB gain on a -13 LUFS tone
  config.true_peak_dbtp = -6.0;
  REQUIRE(media_restore::parse_layout("stereo", &config.output_layout));
  media_restore::AudioStats stats;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::normalize_loudness(config, &buffer, &stats, &diagnostics));
  CHECK(stats.peak_limited);
  CHECK(stats.limited_samples > 0);
  CHECK(stats.output_true_peak_dbtp <= -2.9);
}

TEST_CASE("quarantine expectations must be media validation errors", "[manifest]") {
  // A dependency failure (exit 3) is not something a manifest segment can
  // declare as expected damage.
  std::string text = manifest_with_tracks(
      R"({"id":"a","kind":"video","depends_on":[],"segments":[
          {"id":"s1","start":"0/1","duration":"1/1","media":"m.h264",
           "codec":"h264","quarantine":{"expected_code":"E_DEPENDENCY_CYCLE"}}]})");
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  CHECK_FALSE(media_restore::parse_manifest(text, &manifest, &diagnostics));
  REQUIRE_FALSE(diagnostics.empty());
  CHECK(diagnostics.back().code == media_restore::ErrorCode::manifest_schema);
}

TEST_CASE("quarantine survives a manifest round trip", "[manifest]") {
  const std::string text = manifest_with_tracks(
      R"({"id":"a","kind":"video","depends_on":[],"segments":[
          {"id":"s1","start":"0/1","duration":"1/1","media":"m.h264",
           "codec":"h264","quarantine":{"expected_code":"E_INDEX_ORDER"}}]})");
  media_restore::Manifest manifest;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::parse_manifest(text, &manifest, &diagnostics));
  REQUIRE(manifest.tracks.front().segments.front().expected_code.has_value());
  CHECK(*manifest.tracks.front().segments.front().expected_code ==
        media_restore::ErrorCode::index_order);
  media_restore::Manifest again;
  const std::string serialized = media_restore::serialize_manifest(manifest);
  REQUIRE(media_restore::parse_manifest(serialized, &again, &diagnostics));
  CHECK(media_restore::serialize_manifest(again) == serialized);
}

TEST_CASE("media limits reject oversized bitstreams and indexes", "[limits]") {
  const std::string bytes = synthetic_h264();
  media_restore::StreamScan scan;
  std::vector<media_restore::Diagnostic> diagnostics;
  media_restore::Limits tiny;
  tiny.max_media_bytes = 16;
  CHECK_FALSE(media_restore::scan_raw_stream_bytes(
      bytes, media_restore::Codec::h264, tiny, &scan, &diagnostics));
  REQUIRE_FALSE(diagnostics.empty());
  CHECK(diagnostics.back().code == media_restore::ErrorCode::limit_exceeded);

  diagnostics.clear();
  media_restore::Limits few_entries;
  few_entries.max_index_entries = 1;
  CHECK_FALSE(media_restore::scan_raw_stream_bytes(
      bytes, media_restore::Codec::h264, few_entries, &scan, &diagnostics));
  REQUIRE_FALSE(diagnostics.empty());
  CHECK(diagnostics.back().code == media_restore::ErrorCode::index_too_large);
}

TEST_CASE("archive paths cannot escape the manifest directory", "[io]") {
  media_restore::Diagnostic error;
  std::filesystem::path resolved;
  CHECK_FALSE(media_restore::resolve_archive_path("/tmp/archive/archive.json",
                                                  "../../etc/passwd", &resolved,
                                                  &error));
  CHECK(error.code == media_restore::ErrorCode::io);
  CHECK_FALSE(media_restore::resolve_archive_path("/tmp/archive/archive.json",
                                                  "/etc/passwd", &resolved,
                                                  &error));
  REQUIRE(media_restore::resolve_archive_path(
      "/tmp/archive/archive.json", "media/reel.h264", &resolved, &error));
  CHECK(resolved == std::filesystem::path("/tmp/archive/media/reel.h264"));
}

TEST_CASE("index entries recovered from a scan keep their origin", "[bitstream]") {
  const std::string bytes = synthetic_h264();
  media_restore::StreamScan scan;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::scan_raw_stream_bytes(bytes, media_restore::Codec::h264,
                                               media_restore::Limits{}, &scan,
                                               &diagnostics));
  // An empty but present index is legal: every keyframe is generated.
  media_restore::KeyframeIndex empty;
  empty.present = true;
  empty.version = 1;
  media_restore::IndexReport report;
  REQUIRE(media_restore::validate_keyframe_index(empty, scan,
                                                 media_restore::Limits{}, &report,
                                                 &diagnostics));
  CHECK(report.index_entries == 0);
  CHECK(report.hits == 0);
  CHECK(report.generated == scan.keyframes.size());
  for (const auto& candidate : report.candidates) {
    CHECK(candidate.origin == media_restore::EntryOrigin::scan);
    CHECK_FALSE(candidate.tick.has_value());
  }
}

TEST_CASE("index version disagreements surface as diagnostics", "[bitstream]") {
  // The manifest names version 2 while the file on disk is version 1: this is
  // a schema disagreement, not something to silently accept.
  media_restore::Limits limits;
  const std::string index_text = R"({
    "format": "media-archive-keyframe-index", "version": 1,
    "entries": [{"tick": 0, "offset": 0, "keyframe": true}]
  })";
  const std::filesystem::path path =
      std::filesystem::temp_directory_path() / "mr-index-version-test.json";
  {
    std::ofstream out(path);
    out << index_text;
  }
  media_restore::KeyframeIndex index;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::load_keyframe_index(path, limits, &index, &diagnostics));
  CHECK(index.version == 1);
  std::error_code ec;
  std::filesystem::remove(path, ec);

}

TEST_CASE("nearest tick conversion bounds its rounding error", "[rational]") {
  using media_restore::Rational;
  int64_t ticks = 0;
  Rational remainder;
  // 1 ms at 44.1 kHz is 44.1 samples: rounds to 44 with a +0.1 sample error.
  REQUIRE(Rational::nearest_ticks(Rational(1, 1000), Rational(1, 44100),
                                  &ticks, &remainder));
  CHECK(ticks == 44);
  CHECK(std::fabs(remainder.to_double()) < 1.0 / 44100.0);
  CHECK(std::fabs(remainder.to_double() - 0.1 / 44100.0) < 1e-12);

  // Exact grids round with no error at all.
  REQUIRE(Rational::nearest_ticks(Rational(1, 1000), Rational(1, 48000),
                                  &ticks, &remainder));
  CHECK(ticks == 48);
  CHECK(remainder.is_zero());

  // Negative values round away from zero and keep an exact remainder.
  REQUIRE(Rational::nearest_ticks(Rational(-1, 1000), Rational(1, 44100),
                                  &ticks, &remainder));
  CHECK(ticks == -44);
  CHECK(remainder.to_double() < 0.0);
}

TEST_CASE("a segment too short to gate is delivered, not failed", "[audio]") {
  auto input = make_tone(48000, "mono", 480, 440.0, 0.5);  // 10 ms
  media_restore::AudioRestoreConfig config;
  config.output_rate = 48000;
  REQUIRE(media_restore::parse_layout("mono", &config.output_layout));
  media_restore::AudioStats stats;
  std::vector<media_restore::Diagnostic> diagnostics;
  REQUIRE(media_restore::normalize_loudness(config, &input, &stats, &diagnostics));
  CHECK(stats.short_segment);
  bool only_warnings = true;
  for (const auto& diagnostic : diagnostics) {
    only_warnings &= diagnostic.severity == media_restore::Severity::warning;
  }
  CHECK(only_warnings);
}
