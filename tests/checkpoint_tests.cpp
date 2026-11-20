#include "media/checkpoint.hpp"

#include <catch2/catch_test_macros.hpp>

#include <filesystem>
#include <string>

#include "fixture_factory.hpp"

using namespace mediaqc;
using mediaqc::testing::TempDir;

TEST_CASE("checkpoint round-trips all resume state", "[checkpoint]") {
  TempDir dir;
  const std::string path = (dir.path() / "state.json").string();

  Checkpoint checkpoint;
  checkpoint.input_sha256 = std::string(64, 'a');
  checkpoint.config_sha256 = std::string(64, 'b');
  checkpoint.output_path = "/tmp/out";
  checkpoint.frames_committed = 1234;
  checkpoint.frames_repaired = 7;
  checkpoint.records_consumed = {600, 634};
  checkpoint.peak_queue_frames = 8;
  checkpoint.phase = "writing";
  TimelineState timeline;
  timeline.started = true;
  timeline.clock_anchored = true;
  timeline.last_slot = 42;
  timeline.last_device_ns = 840000000;
  checkpoint.timeline.push_back(timeline);
  CommittedSegment segment;
  segment.track = "video_main";
  segment.path = "media/video_main.seg00000.mp4";
  segment.sha256 = std::string(64, 'c');
  segment.first_frame = 0;
  segment.frames = 500;
  segment.start_ns = 0;
  segment.end_ns = 20000000000LL;
  checkpoint.segments.push_back(segment);
  CommittedEntry entry;
  entry.time_ns = 40000000;
  entry.frame = 1;
  entry.frame_index = 1;
  entry.track = "video_main";
  entry.segment = 0;
  entry.payload_offset = 44;
  entry.payload_size = 680;
  entry.keyframe = true;
  entry.sha256 = std::string(64, 'd');
  checkpoint.entries.push_back(entry);

  Error error;
  REQUIRE(save_checkpoint(path, checkpoint, error));

  Checkpoint loaded;
  bool found = false;
  REQUIRE(load_checkpoint(path, loaded, found, error));
  CHECK(found);
  CHECK(loaded.input_sha256 == checkpoint.input_sha256);
  CHECK(loaded.config_sha256 == checkpoint.config_sha256);
  CHECK(loaded.frames_committed == 1234);
  CHECK(loaded.records_consumed == std::vector<std::uint64_t>{600, 634});
  REQUIRE(loaded.timeline.size() == 1);
  CHECK(loaded.timeline[0].last_slot == 42);
  REQUIRE(loaded.segments.size() == 1);
  CHECK(loaded.segments[0].end_ns == 20000000000LL);
  REQUIRE(loaded.entries.size() == 1);
  CHECK(loaded.entries[0].payload_offset == 44);
}

TEST_CASE("a missing checkpoint is reported as not found", "[checkpoint]") {
  TempDir dir;
  Checkpoint checkpoint;
  bool found = true;
  Error error;
  bool ok = load_checkpoint((dir.path() / "absent.json").string(), checkpoint,
                            found, error);
  CHECK_FALSE(ok);
  CHECK_FALSE(found);
  CHECK(error.message.empty());
}

TEST_CASE("a corrupt checkpoint fails to load", "[checkpoint]") {
  TempDir dir;
  REQUIRE(dir.write("state.json", "{ not json"));
  Checkpoint checkpoint;
  bool found = false;
  Error error;
  CHECK_FALSE(load_checkpoint((dir.path() / "state.json").string(), checkpoint,
                              found, error));
  CHECK(error.code == "checkpoint_corrupt");
  CHECK(error.exit_code() == 4);
}

TEST_CASE("config digest changes with the configuration", "[checkpoint]") {
  std::string base = config_digest("/in", "/out", 8, 100, 500);
  CHECK(base == config_digest("/in", "/out", 8, 100, 500));
  CHECK(base != config_digest("/in", "/out", 4, 100, 500));
  CHECK(base != config_digest("/in", "/other", 8, 100, 500));
  CHECK(base != config_digest("/in", "/out", 8, 100, 1000));
}

TEST_CASE("checkpoint save is atomic and leaves no temp file", "[checkpoint]") {
  TempDir dir;
  const std::string path = (dir.path() / "state.json").string();
  Checkpoint checkpoint;
  checkpoint.input_sha256 = std::string(64, 'a');
  checkpoint.config_sha256 = std::string(64, 'b');
  checkpoint.output_path = "/tmp/out";
  Error error;
  REQUIRE(save_checkpoint(path, checkpoint, error));
  CHECK(std::filesystem::exists(path));
  CHECK_FALSE(std::filesystem::exists(path + ".tmp"));
}
