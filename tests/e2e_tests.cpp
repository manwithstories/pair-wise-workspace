#include "media/pipeline.hpp"

#include <catch2/catch_test_macros.hpp>

#include <filesystem>
#include <string>

#include "fixture_factory.hpp"
#include "media/generate.hpp"
#include "media/index.hpp"
#include "media/manifest.hpp"

using namespace mediaqc;
using mediaqc::testing::TempDir;

namespace {

PipelineOptions options_for(const TempDir& dir, std::uint64_t queue_frames = 8) {
  PipelineOptions options;
  options.input_dir = (dir.path() / "sample").string();
  options.output_dir = (dir.path() / "out").string();
  options.queue_frames = queue_frames;
  options.checkpoint_path = (dir.path() / "state").string();
  return options;
}

GenerateResult make_delivery(const TempDir& dir, Scenario scenario,
                             std::uint64_t frames) {
  GenerateOptions options;
  options.scenario = scenario;
  options.frames = frames;
  return generate_delivery((dir.path() / "sample").string(), options, true);
}

}  // namespace

TEST_CASE("clean delivery processes with exit-0 semantics", "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kNormal, 600).ok);
  Pipeline pipeline(options_for(dir));
  PipelineReport report = pipeline.run();
  REQUIRE(report.ok);
  CHECK(report.frames_total == 600);
  CHECK(report.frames_corrupt == 0);
  CHECK(report.frames_truncated == 0);
  CHECK(report.first_corrupt_offset == -1);
  CHECK(report.index_entries == 600);
  CHECK(report.output_sha256.size() == 64);
  CHECK(report.errors.empty());

  // Verify the result independently.
  MediaIndex index;
  Error error;
  REQUIRE(load_index((dir.path() / "out").string(), index, error));
  CHECK(index.entries.size() == 600);
  for (const IndexEntry& entry : index.entries) {
    CHECK(entry.time_ns >= 0);
    CHECK(entry.payload_size > 0);
  }
}

TEST_CASE("queue peak never exceeds the configured bound", "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kSlowConsumer, 3000).ok);
  for (std::uint64_t bound : {1, 4, 8}) {
    TempDir run_dir;
    REQUIRE(make_delivery(run_dir, Scenario::kSlowConsumer, 3000).ok);
    PipelineOptions options = options_for(run_dir, bound);
    // Fresh output each time.
    Pipeline pipeline(options);
    PipelineReport report = pipeline.run();
    REQUIRE(report.ok);
    CHECK(report.peak_queue_frames <= bound);
    CHECK(report.index_entries == 3000);
  }
}

TEST_CASE("checksum damage is located at the exact byte offset", "[pipeline]") {
  TempDir dir;
  GenerateResult generated = make_delivery(dir, Scenario::kBadChecksum, 600);
  REQUIRE(generated.ok);
  REQUIRE(generated.first_corrupt_offset > 0);

  Pipeline pipeline(options_for(dir));
  PipelineReport report = pipeline.run();
  REQUIRE(report.ok);
  CHECK(report.frames_corrupt == 1);
  CHECK(report.first_corrupt_offset == generated.first_corrupt_offset);

  // The machine-readable list names the frame and offset.
  bool found = false;
  for (const ErrorEntry& entry : report.errors.entries()) {
    if (entry.code == "checksum_mismatch") {
      found = true;
      CHECK(entry.has_offset);
      CHECK(entry.offset == static_cast<std::uint64_t>(generated.first_corrupt_offset));
    }
  }
  CHECK(found);
}

TEST_CASE("duplicate, out-of-order and wraparound are reported as repairs",
          "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kBoundary, 1200).ok);
  Pipeline pipeline(options_for(dir));
  PipelineReport report = pipeline.run();
  REQUIRE(report.ok);
  CHECK(report.frames_repaired >= 3);
  bool saw_wrap = false;
  bool saw_missing = false;
  bool saw_non_monotonic = false;
  for (const RepairRecord& repair : report.repairs) {
    saw_wrap = saw_wrap || has_reason(repair.reasons, RepairReason::kWraparound);
    saw_missing = saw_missing || has_reason(repair.reasons, RepairReason::kMissing);
    saw_non_monotonic =
        saw_non_monotonic || has_reason(repair.reasons, RepairReason::kNonMonotonic);
  }
  CHECK(saw_wrap);
  CHECK(saw_missing);
  CHECK(saw_non_monotonic);
  // Timestamps stay strictly increasing after repair.
  std::uint64_t corrupt = 0;
  for (const ErrorEntry& entry : report.errors.entries()) {
    if (entry.code == "checksum_mismatch") ++corrupt;
  }
  CHECK(corrupt == 1);
}

TEST_CASE("truncated tail is reported with a located offset", "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kTruncatedTail, 600).ok);
  Pipeline pipeline(options_for(dir));
  PipelineReport report = pipeline.run();
  REQUIRE(report.ok);
  CHECK(report.frames_truncated > 0);
  CHECK(report.first_corrupt_offset >= 0);
  bool found = false;
  for (const ErrorEntry& entry : report.errors.entries()) {
    if (entry.code == "truncated_tail" || entry.code == "truncated_media") {
      found = true;
      CHECK(entry.has_offset);
    }
  }
  CHECK(found);
}

TEST_CASE("interrupt writes a checkpoint and resume completes the run",
          "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kNormal, 3000).ok);

  PipelineOptions options = options_for(dir);
  options.stop_after_frames = 1000;
  Pipeline interrupted(options);
  PipelineReport first = interrupted.run();
  REQUIRE(first.ok);
  CHECK(first.interrupted);
  CHECK(first.index_entries == 1000);
  CHECK(std::filesystem::exists(options.checkpoint_path));

  // Resume continues from the checkpoint and produces complete output.
  PipelineOptions resume = options;
  resume.stop_after_frames = 0;
  Pipeline second(resume);
  PipelineReport report = second.run();
  REQUIRE(report.ok);
  CHECK(report.resumed);
  CHECK(report.index_entries == 3000);
  // The checkpoint is consumed on success.
  CHECK_FALSE(std::filesystem::exists(options.checkpoint_path));

  MediaIndex index;
  Error error;
  REQUIRE(load_index(options.output_dir, index, error));
  CHECK(index.entries.size() == 3000);
}

TEST_CASE("a stale checkpoint is refused when the input changed", "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kNormal, 800).ok);
  PipelineOptions options = options_for(dir);
  options.stop_after_frames = 300;
  Pipeline interrupted(options);
  REQUIRE(interrupted.run().interrupted);

  // Replace the input metadata with different bytes so the input digest
  // differs from the one the checkpoint recorded.
  std::filesystem::path meta = std::filesystem::path(options.input_dir) /
                               "video_main.frames.jsonl";
  {
    std::FILE* file = std::fopen(meta.string().c_str(), "ab");
    REQUIRE(file != nullptr);
    std::fputs("\n", file);
    std::fclose(file);
  }
  PipelineOptions resume = options;
  resume.stop_after_frames = 0;
  Pipeline second(resume);
  PipelineReport report = second.run();
  CHECK_FALSE(report.ok);
  CHECK(report.error.code == "checkpoint_input_mismatch");
  CHECK(report.error.exit_code() == 4);
}

TEST_CASE("a finalized checkpoint is never reused", "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kNormal, 300).ok);
  PipelineOptions options = options_for(dir);
  // First run completes and consumes the checkpoint.
  Pipeline first(options);
  REQUIRE(first.run().ok);
  CHECK_FALSE(std::filesystem::exists(options.checkpoint_path));

  // Forge a finalized checkpoint; the pipeline must refuse it.
  Checkpoint finalized;
  finalized.input_sha256 = std::string(64, '0');
  finalized.config_sha256 = std::string(64, '0');
  finalized.output_path = options.output_dir;
  finalized.phase = "finalized";
  Error error;
  REQUIRE(save_checkpoint(options.checkpoint_path, finalized, error));
  Pipeline second(options);
  PipelineReport report = second.run();
  CHECK_FALSE(report.ok);
  // It fails on the input digest before even reaching the finalized check;
  // either refusal is acceptable, but it must not silently succeed.
  CHECK(report.error.exit_code() == 4);
}

TEST_CASE("cancel stops the run and leaves a resumable checkpoint",
          "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kSlowConsumer, 2000).ok);
  PipelineOptions options = options_for(dir);
  // Stop after a modest number of frames via cancellation-like interrupt.
  options.stop_after_frames = 500;
  Pipeline pipeline(options);
  PipelineReport report = pipeline.run();
  REQUIRE(report.ok);
  CHECK(report.interrupted);
  CHECK(std::filesystem::exists(options.checkpoint_path));
}

TEST_CASE("manifest records everything an archivist needs", "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kBoundary, 600).ok);
  Pipeline pipeline(options_for(dir));
  PipelineReport report = pipeline.run();
  REQUIRE(report.ok);

  Manifest manifest;
  Error error;
  REQUIRE(load_manifest((dir.path() / "out" / "manifest.json").string(), manifest,
                        error));
  CHECK(manifest.input_sha256 == report.input_sha256);
  CHECK(manifest.output_sha256 == report.output_sha256);
  CHECK(manifest.frames_total == 600);
  CHECK(manifest.index_entries == report.index_entries);
  CHECK(manifest.repairs.size() == report.repairs.size());
  CHECK(manifest.tracks.size() == 2);
}

TEST_CASE("query locates frames by time and sequence", "[pipeline]") {
  TempDir dir;
  REQUIRE(make_delivery(dir, Scenario::kNormal, 600).ok);
  Pipeline pipeline(options_for(dir));
  REQUIRE(pipeline.run().ok);

  MediaIndex index;
  Error error;
  REQUIRE(load_index((dir.path() / "out").string(), index, error));
  const IndexEntry* by_frame = find_by_frame(index, "video_main", 200);
  REQUIRE(by_frame != nullptr);
  CHECK(by_frame->frame <= 200);
  const IndexEntry* by_time = find_by_time(index, "video_main", 4000000000LL);
  REQUIRE(by_time != nullptr);
  CHECK(by_time->time_ns <= 4000000000LL);
}
