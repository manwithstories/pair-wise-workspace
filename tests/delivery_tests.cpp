#include "media/delivery.hpp"

#include <catch2/catch_test_macros.hpp>

#include <string>

#include "fixture_factory.hpp"

using namespace mediaqc;
using mediaqc::testing::TempDir;

namespace {

const char* kGoodDelivery = R"({
  "schema_version": 1,
  "producer": "test-kit",
  "created": "2026-11-21T00:00:00Z",
  "tracks": [
    {
      "id": "video_main",
      "kind": "video",
      "codec": "ffv1",
      "pix_fmt": "yuv420p",
      "width": 64,
      "height": 48,
      "rate": {"num": 25, "den": 1},
      "clock": {"kind": "ticks", "hz": 90000, "bits": 32},
      "depends_on": [],
      "required_fields": ["camera_id"],
      "optional_fields": ["lens"],
      "metadata_file": "video_main.frames.jsonl",
      "media_file": "video_main.mkv"
    },
    {
      "id": "audio_main",
      "kind": "audio",
      "codec": "pcm_s16le",
      "sample_rate": 48000,
      "channels": 1,
      "rate": {"num": 50, "den": 1},
      "clock": {"kind": "ns"},
      "metadata_file": "audio_main.frames.jsonl",
      "media_file": "audio_main.mkv"
    }
  ]
})";

const char* kTrack = R"({"id":"v","kind":"video","codec":"ffv1","pix_fmt":"yuv420p",
  "width":16,"height":16,"rate":{"num":25,"den":1},
  "clock":{"kind":"ticks","hz":90000},
  "metadata_file":"v.jsonl","media_file":"v.mkv"})";

TrackDesc video_track() {
  TrackDesc t;
  t.id = "v";
  t.kind = TrackKind::Video;
  t.rate = Rational{25, 1};
  t.clock = ClockSpec{90000, 0};
  t.pix_fmt = "yuv420p";
  t.width = 16;
  t.height = 16;
  return t;
}

}  // namespace

TEST_CASE("a well-formed delivery loads with all fields", "[delivery]") {
  TempDir dir;
  REQUIRE(dir.write("delivery.json", kGoodDelivery));
  auto result = load_delivery(dir.path().string());
  REQUIRE(result.ok);
  CHECK(result.delivery.schema_version == 1);
  CHECK(result.delivery.producer == "test-kit");
  REQUIRE(result.delivery.tracks.size() == 2);

  const DeliveryTrack& video = result.delivery.tracks[0];
  CHECK(video.desc.id == "video_main");
  CHECK(video.desc.kind == TrackKind::Video);
  CHECK(video.desc.width == 64);
  CHECK(video.desc.height == 48);
  CHECK(video.desc.rate == Rational{25, 1});
  CHECK(video.desc.clock.hz == 90000);
  CHECK(video.desc.clock.bits == 32);
  CHECK(video.desc.required_fields == std::vector<std::string>{"camera_id"});
  CHECK(video.desc.optional_fields == std::vector<std::string>{"lens"});

  const DeliveryTrack& audio = result.delivery.tracks[1];
  CHECK(audio.desc.kind == TrackKind::Audio);
  CHECK(audio.desc.sample_rate == 48000);
  CHECK(audio.desc.channels == 1);
  CHECK(audio.desc.clock.hz == static_cast<std::uint32_t>(kNsPerSecond));
}

TEST_CASE("unknown top-level fields are preserved", "[delivery]") {
  TempDir dir;
  REQUIRE(dir.write("delivery.json", R"({
    "schema_version": 1, "tracks": [], "batch_id": "archive-2026-11",
    "notes": {"owner": "docs"}
  })"));
  auto result = load_delivery(dir.path().string());
  REQUIRE(result.ok);
  REQUIRE(result.delivery.extra.find("batch_id") != nullptr);
  CHECK(result.delivery.extra.find("batch_id")->as_string() == "archive-2026-11");
}

TEST_CASE("unsupported schema version is rejected", "[delivery]") {
  TempDir dir;
  REQUIRE(dir.write("delivery.json", R"({"schema_version": 99, "tracks": []})"));
  auto result = load_delivery(dir.path().string());
  CHECK_FALSE(result.ok);
  CHECK(result.error.code == "unsupported_schema_version");
  CHECK(result.error.exit_code() == 2);
}

TEST_CASE("missing required delivery fields are reported", "[delivery]") {
  TempDir dir;
  REQUIRE(dir.write("delivery.json", R"({"tracks": []})"));
  auto result = load_delivery(dir.path().string());
  CHECK_FALSE(result.ok);
  CHECK(result.error.code == "missing_field");
  CHECK(result.error.message.find("schema_version") != std::string::npos);
}

TEST_CASE("missing track media file is rejected", "[delivery]") {
  TempDir dir;
  REQUIRE(dir.write("delivery.json", R"({"schema_version":1,"tracks":[
    {"id":"v","kind":"video","codec":"ffv1","width":16,"height":16,
     "rate":{"num":25,"den":1},"metadata_file":"v.jsonl"}]})"));
  auto result = load_delivery(dir.path().string());
  CHECK_FALSE(result.ok);
  CHECK(result.error.message.find("media_file") != std::string::npos);
}

TEST_CASE("frame metadata parses and preserves unknown optional fields",
          "[delivery]") {
  std::string line =
      R"({"seq":7,"capture_ticks":9000,"keyframe":1,"sha256":")" +
      std::string(64, 'a') +
      R"(","camera_id":"cam-a","lens":{"focal_mm":50,"note":"prime"}})";
  auto result = parse_frame_metadata(line, 3, 120, video_track());
  REQUIRE(result.ok);
  CHECK(result.frame.seq == 7);
  CHECK(result.frame.has_capture);
  CHECK(result.frame.capture_ticks == 9000);
  CHECK(result.frame.keyframe);
  CHECK(result.frame.pix_fmt == PixelFormatId::kYuv420p);
  REQUIRE(result.frame.attrs.find("camera_id") != nullptr);
  CHECK(result.frame.attrs.find("camera_id")->as_string() == "cam-a");
  REQUIRE(result.frame.attrs.find("lens") != nullptr);
  CHECK(result.frame.attrs.find("lens")->find("focal_mm")->as_int() == 50);
}

TEST_CASE("missing required frame field points at the line offset",
          "[delivery]") {
  TrackDesc track = video_track();
  track.required_fields = {"camera_id"};
  std::string line = R"({"seq":1,"capture_ticks":0,"sha256":")" +
                     std::string(64, 'b') + R"("})";
  auto result = parse_frame_metadata(line, 5, 4096, track);
  CHECK_FALSE(result.ok);
  CHECK(result.error.code == "missing_field");
  REQUIRE(result.error.has_offset);
  CHECK(result.error.offset == 4096);
  CHECK(result.error.message.find("camera_id") != std::string::npos);
}

TEST_CASE("null capture_ticks means the timestamp is missing", "[delivery]") {
  std::string line = R"({"seq":1,"capture_ticks":null,"sha256":")" +
                     std::string(64, 'c') + R"("})";
  auto result = parse_frame_metadata(line, 1, 0, video_track());
  REQUIRE(result.ok);
  CHECK_FALSE(result.frame.has_capture);
}

TEST_CASE("malformed checksum and JSON carry exact offsets", "[delivery]") {
  {
    std::string line = R"({"seq":1,"capture_ticks":0,"sha256":"nothex"})";
    auto result = parse_frame_metadata(line, 2, 77, video_track());
    CHECK_FALSE(result.ok);
    CHECK(result.error.code == "bad_checksum");
    CHECK(result.error.offset == 77);
  }
  {
    std::string line = R"({"seq":1,"capture_ticks":)";
    auto result = parse_frame_metadata(line, 2, 88, video_track());
    CHECK_FALSE(result.ok);
    CHECK(result.error.code == "bad_frame_json");
    CHECK(result.error.offset == 88);
  }
}

TEST_CASE("JSONL loader reports the offset of the first bad line",
          "[delivery]") {
  TempDir dir;
  std::string good1 =
      R"({"seq":0,"capture_ticks":0,"sha256":)" + std::string("\"") +
      std::string(64, 'd') + "\"}";
  std::string good2 =
      R"({"seq":1,"capture_ticks":3600,"sha256":)" + std::string("\"") +
      std::string(64, 'e') + "\"}";
  std::string bad = R"({"seq":2,"capture_ticks":7200,"sha256":"short"})";
  std::string content = good1 + "\n" + good2 + "\n" + bad + "\n";
  REQUIRE(dir.write("v.frames.jsonl", content));

  std::vector<FrameMetadata> frames;
  Error error;
  bool ok = load_frame_metadata(
      (dir.path() / "v.frames.jsonl").string(), video_track(), frames, error);
  CHECK_FALSE(ok);
  CHECK(error.code == "bad_checksum");
  // Offset of the third line: both good lines plus their newlines.
  CHECK(error.has_offset);
  CHECK(error.offset == good1.size() + 1 + good2.size() + 1);
  CHECK(frames.size() == 2);  // the two good records were retained
}

TEST_CASE("path escapes outside the delivery directory are rejected",
          "[delivery]") {
  TempDir dir;
  std::string absolute;
  Error error;
  CHECK(resolve_delivery_path(dir.path().string(), "video_main.mkv", absolute,
                              error));
  // macOS resolves /var -> /private/var, so compare canonical directories.
  auto canonical_base = std::filesystem::weakly_canonical(dir.path());
  CHECK(std::filesystem::path(absolute).parent_path() == canonical_base);

  CHECK_FALSE(resolve_delivery_path(dir.path().string(), "../escape.mkv",
                                    absolute, error));
  CHECK(error.code == "path_escape");

  error = Error{};
  CHECK_FALSE(resolve_delivery_path(dir.path().string(), "/etc/passwd",
                                    absolute, error));
  CHECK(error.code == "absolute_path");
}
