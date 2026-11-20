#include "media/generate.hpp"

#include <catch2/catch_test_macros.hpp>

#include <filesystem>
#include <string>
#include <vector>

#include "fixture_factory.hpp"
#include "media/delivery.hpp"
#include "media/frame_reader.hpp"
#include "media/sha256.hpp"

using namespace mediaqc;
using mediaqc::testing::TempDir;

namespace {

GenerateResult generate(const TempDir& dir, Scenario scenario,
                        std::uint64_t frames = 300) {
  GenerateOptions options;
  options.scenario = scenario;
  options.frames = frames;
  return generate_delivery(dir.path().string(), options, true);
}

// Reads a track's metadata and the matching media packets, returning how many
// frames disagreed with their recorded checksum.
int verify_track(const std::string& dir, const DeliveryTrack& track,
                 std::size_t& frame_count, std::size_t& packet_count) {
  std::string meta_path;
  std::string media_path;
  Error error;
  REQUIRE(resolve_delivery_path(dir, track.metadata_file, meta_path, error));
  REQUIRE(resolve_delivery_path(dir, track.media_file, media_path, error));

  std::vector<FrameMetadata> frames;
  REQUIRE(load_frame_metadata(meta_path, track.desc, frames, error));
  frame_count = frames.size();

  FrameReader reader;
  REQUIRE(reader.open(media_path, track.desc, FrameReader::Options{}, error));
  FrameReader::Packet packet;
  bool eof = false;
  std::size_t index = 0;
  int mismatches = 0;
  while (reader.next(packet, eof, error)) {
    if (index < frames.size()) {
      Sha256Digest digest = sha256(packet.data.data(), packet.data.size());
      if (digest != frames[index].sha) ++mismatches;
    }
    ++index;
  }
  packet_count = index;
  return mismatches;
}

}  // namespace

TEST_CASE("normal delivery round-trips every checksum", "[generate]") {
  TempDir dir;
  auto result = generate(dir, Scenario::kNormal);
  REQUIRE(result.ok);
  CHECK(result.tracks == 2);
  CHECK(result.frames == 300);
  CHECK(result.first_corrupt_offset == -1);

  auto delivery = load_delivery(dir.path().string());
  REQUIRE(delivery.ok);
  REQUIRE(delivery.delivery.tracks.size() == 2);
  for (const auto& track : delivery.delivery.tracks) {
    std::size_t frames = 0;
    std::size_t packets = 0;
    CHECK(verify_track(dir.path().string(), track, frames, packets) == 0);
    CHECK(frames == packets);
  }
}

TEST_CASE("generation is byte-for-byte reproducible", "[generate]") {
  TempDir a;
  TempDir b;
  REQUIRE(generate(a, Scenario::kBoundary).ok);
  REQUIRE(generate(b, Scenario::kBoundary).ok);
  std::string error;
  for (const char* name : {"delivery.json", "video_main.frames.jsonl",
                           "audio_main.frames.jsonl"}) {
    Sha256Digest da = sha256_of_file((a.path() / name).string(), error);
    Sha256Digest db = sha256_of_file((b.path() / name).string(), error);
    CHECK(da == db);
  }
}

TEST_CASE("bad-checksum scenario damages exactly one frame at a known offset",
          "[generate]") {
  TempDir dir;
  auto result = generate(dir, Scenario::kBadChecksum);
  REQUIRE(result.ok);
  REQUIRE(result.first_corrupt_offset > 0);

  auto delivery = load_delivery(dir.path().string());
  REQUIRE(delivery.ok);
  const DeliveryTrack& video = delivery.delivery.tracks[0];
  std::size_t frames = 0;
  std::size_t packets = 0;
  CHECK(verify_track(dir.path().string(), video, frames, packets) == 1);

  // The reported offset must coincide with a metadata line start.
  std::string meta_path;
  Error error;
  REQUIRE(resolve_delivery_path(dir.path().string(), video.metadata_file,
                                meta_path, error));
  std::vector<FrameMetadata> metadata;
  REQUIRE(load_frame_metadata(meta_path, video.desc, metadata, error));
  bool found = false;
  for (const auto& frame : metadata) {
    if (frame.offset == static_cast<std::uint64_t>(result.first_corrupt_offset)) {
      found = true;
    }
  }
  CHECK(found);
}

TEST_CASE("truncated-tail scenario ends the media early", "[generate]") {
  TempDir dir;
  REQUIRE(generate(dir, Scenario::kTruncatedTail).ok);
  auto delivery = load_delivery(dir.path().string());
  REQUIRE(delivery.ok);
  const DeliveryTrack& video = delivery.delivery.tracks[0];
  std::string meta_path;
  std::string media_path;
  Error error;
  REQUIRE(resolve_delivery_path(dir.path().string(), video.metadata_file,
                                meta_path, error));
  REQUIRE(resolve_delivery_path(dir.path().string(), video.media_file, media_path,
                                error));
  std::vector<FrameMetadata> frames;
  REQUIRE(load_frame_metadata(meta_path, video.desc, frames, error));
  FrameReader reader;
  REQUIRE(reader.open(media_path, video.desc, FrameReader::Options{}, error));
  FrameReader::Packet packet;
  bool eof = false;
  std::size_t count = 0;
  while (reader.next(packet, eof, error)) ++count;
  // Far fewer packets remain than metadata records: the tail is gone.
  CHECK(count < frames.size());
}

TEST_CASE("wraparound scenario records a counter rollover", "[generate]") {
  TempDir dir;
  REQUIRE(generate(dir, Scenario::kWraparound).ok);
  auto delivery = load_delivery(dir.path().string());
  REQUIRE(delivery.ok);
  const DeliveryTrack& video = delivery.delivery.tracks[0];
  CHECK(video.desc.clock.bits == 32);
  std::string meta_path;
  Error error;
  REQUIRE(resolve_delivery_path(dir.path().string(), video.metadata_file,
                                meta_path, error));
  std::vector<FrameMetadata> frames;
  REQUIRE(load_frame_metadata(meta_path, video.desc, frames, error));
  REQUIRE(frames.size() > 10);
  // The raw counter must decrease somewhere, proving a genuine rollover.
  bool decreased = false;
  for (std::size_t i = 1; i < frames.size(); ++i) {
    if (frames[i].capture_ticks < frames[i - 1].capture_ticks) decreased = true;
  }
  CHECK(decreased);
}

TEST_CASE("duplicate scenario repeats a sequence number", "[generate]") {
  TempDir dir;
  REQUIRE(generate(dir, Scenario::kDuplicate).ok);
  auto delivery = load_delivery(dir.path().string());
  REQUIRE(delivery.ok);
  const DeliveryTrack& video = delivery.delivery.tracks[0];
  std::string meta_path;
  Error error;
  REQUIRE(resolve_delivery_path(dir.path().string(), video.metadata_file,
                                meta_path, error));
  std::vector<FrameMetadata> frames;
  REQUIRE(load_frame_metadata(meta_path, video.desc, frames, error));
  bool duplicate = false;
  for (std::size_t i = 1; i < frames.size(); ++i) {
    if (frames[i].seq == frames[i - 1].seq) duplicate = true;
  }
  CHECK(duplicate);
}

TEST_CASE("scenario names parse round-trip", "[generate]") {
  for (const std::string& name : scenario_names()) {
    Scenario scenario;
    REQUIRE(parse_scenario(name, scenario));
    CHECK(std::string(to_string(scenario)) == name);
  }
  Scenario ignored;
  CHECK_FALSE(parse_scenario("no-such-scenario", ignored));
}

TEST_CASE("refuses to write into a non-empty directory without consent",
          "[generate]") {
  TempDir dir;
  REQUIRE(dir.write("stray.txt", "occupied"));
  GenerateOptions options;
  options.frames = 30;
  // A non-empty directory may hold user data; refuse without allow_existing.
  CHECK_FALSE(generate_delivery(dir.path().string(), options, false).ok);
  CHECK(generate_delivery(dir.path().string(), options, true).ok);
}
