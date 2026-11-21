#include <catch2/catch_test_macros.hpp>

#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <unistd.h>

#include "media/checkpoint.hpp"
#include "media/fixture.hpp"
#include "media/pipeline.hpp"
#include "media/segmenter.hpp"
#include "media/silence.hpp"
#include "media/time.hpp"
#include "media/wav_reader.hpp"

namespace fs = std::filesystem;
using namespace media;

namespace {
// 每个测试用例使用独立临时目录，避免相互污染。
fs::path makeTempDir(const std::string& tag) {
  auto base = fs::temp_directory_path() /
              ("media_pipeline_test_" + tag + "_" +
               std::to_string(::getpid()));
  fs::remove_all(base);
  fs::create_directories(base);
  return base;
}
}  // namespace

TEST_CASE("整数采样时间基准换算无浮点漂移", "[time]") {
  CHECK(msToTicks(1000, 48000) == 48000);
  CHECK(msToTicks(500, 48000) == 24000);
  CHECK(ticksToMs(48000, 48000) == 1000);
  // 整除截断，结果确定。
  CHECK(msToTicks(1, 48000) == 48);
  CHECK(ticksToMs(48, 48000) == 1);
  SampleRange r{10, 20};
  CHECK(r.length() == 10);
  CHECK(r.contains(10));
  CHECK_FALSE(r.contains(20));  // 半开区间
}

TEST_CASE("夹具生成全部样例文件", "[fixture]") {
  auto dir = makeTempDir("fixture");
  fixture::FixtureSet fx = fixture::generateAll(dir.string());
  CHECK(fs::exists(fx.stereoPath));
  CHECK(fs::exists(fx.truncatedPath));
  CHECK(fs::exists(fx.nonIntegerPath));
  CHECK(fs::exists(fx.duplicatePath));
}

TEST_CASE("WAV 流式解析多声道并校验格式", "[wav]") {
  auto dir = makeTempDir("wav");
  auto fx = fixture::generateAll(dir.string());
  WavReader reader(fx.stereoPath, CorruptionPolicy::Skip);
  size_t blocks = 0;
  MediaError err = reader.read([&](const AudioBlock& b) {
    CHECK(b.channels == 2);
    blocks += b.frames;
    return true;
  });
  REQUIRE(err.ok());
  CHECK(reader.summary().format.channels == 2);
  CHECK(reader.summary().format.sampleRate == 48000);
  CHECK(reader.summary().totalFrames == 480000);
  CHECK(blocks == 480000);
  CHECK(reader.summary().digest != 0);
}

TEST_CASE("非整帧尾块报告原始字节偏移且后续可恢复", "[wav][corruption]") {
  auto dir = makeTempDir("tail");
  auto fx = fixture::generateAll(dir.string());
  WavReader reader(fx.nonIntegerPath, CorruptionPolicy::Skip);
  MediaError err = reader.read(nullptr);
  REQUIRE(err.ok());
  REQUIRE(reader.hadRecoverable());
  const auto& errs = reader.recoverableErrors();
  bool hasTail = false;
  for (const auto& e : errs) {
    if (e.code == ErrorCode::NonIntegerFrameTail) {
      hasTail = true;
      CHECK(e.byteOffset >= 0);
      CHECK(e.sampleIndex == 96000);
    }
  }
  CHECK(hasTail);
  CHECK(reader.summary().tailBytes == 3);
  CHECK(reader.summary().totalFrames == 96000);  // 完整帧仍被处理
}

TEST_CASE("帧中途截断按 Skip 策略恢复、按 Abort 终止", "[wav][corruption]") {
  auto dir = makeTempDir("trunc");
  auto fx = fixture::generateAll(dir.string());

  WavReader skip(fx.truncatedPath, CorruptionPolicy::Skip);
  MediaError e1 = skip.read(nullptr);
  CHECK(e1.ok());
  CHECK(skip.hadRecoverable());
  CHECK(skip.recoverableErrors().front().byteOffset >= 0);

  WavReader abort(fx.truncatedPath, CorruptionPolicy::Abort);
  MediaError e2 = abort.read(nullptr);
  CHECK_FALSE(e2.ok());
  CHECK(e2.code == ErrorCode::TruncatedChunk);
}

TEST_CASE("静音检测与峰值/RMS/削波统计", "[silence]") {
  auto dir = makeTempDir("silence");
  auto fx = fixture::generateAll(dir.string());
  SilenceDetector det(SilenceConfig{}, 48000);
  WavReader reader(fx.stereoPath, CorruptionPolicy::Skip);
  REQUIRE(reader.read([&](const AudioBlock& b) {
            det.process(b);
            return true;
          }).ok());
  auto ranges = det.finish(reader.summary().totalFrames);
  CHECK_FALSE(ranges.empty());
  CHECK(det.stats().clippedSamples == 96000);
  CHECK(det.stats().peak == INT32_MAX);
  CHECK(det.stats().totalSamples == 480000 * 2);
  CHECK(det.stats().rms > 0.0);
}

TEST_CASE("切分边界样本唯一归属、区间首尾相接", "[segmenter]") {
  const SampleTick total = 1000;
  SegmenterConfig cfg{.maxSamples = 300, .targetDurationTicks = 0};
  Segmenter seg(cfg, 48000, {}, total);
  const auto& parts = seg.segments();
  CHECK(parts.size() == 4);  // 300/300/300/100
  SampleTick expectStart = 0;
  for (const auto& s : parts) {
    CHECK(s.range.start == expectStart);
    CHECK(s.range.length() <= 300);
    expectStart = s.range.end;
  }
  CHECK(parts.back().range.end == total);
}

TEST_CASE("检查点不匹配拒绝续跑，匹配则幂等", "[checkpoint]") {
  auto dir = makeTempDir("cp");
  CheckpointStore store((dir / "state.json").string());
  CheckpointState a;
  a.params.inputPath = "in.wav";
  a.params.inputDigest = 12345;
  a.params.totalFrames = 100;
  a.transcodeDone = true;
  a.manifestDone = true;
  REQUIRE(store.save(a).ok());

  CheckpointState loaded;
  bool exists = false;
  REQUIRE(store.load(&loaded, &exists).ok());
  CHECK(exists);
  CHECK(loaded.transcodeDone);

  CheckpointParams match = a.params;
  CHECK(CheckpointStore::validate(loaded, match).ok());
  CheckpointParams mismatch = a.params;
  mismatch.inputDigest = 999;
  MediaError bad = CheckpointStore::validate(loaded, mismatch);
  CHECK(bad.code == ErrorCode::CheckpointMismatch);
}

TEST_CASE("完整流水线幂等恢复并验证时间线对齐", "[pipeline][resume]") {
  auto dir = makeTempDir("pipeline");
  auto fx = fixture::generateAll(dir.string());
  PipelineConfig cfg;
  cfg.inputPath = fx.stereoPath;
  cfg.outputPath = (dir / "out.m4a").string();
  cfg.manifestPath = (dir / "manifest.json").string();
  cfg.checkpointPath = (dir / "state.json").string();
  cfg.workDir = dir.string();
  cfg.targetBitrate = 192000;
  cfg.targetDurationMs = 2000;

  PipelineResult first = Pipeline(cfg).run();
  CHECK(first.status == ExitStatus::Ok);
  CHECK(first.alignmentErrorSamples == 0);
  CHECK(first.segmentsCount >= 2);
  CHECK(fs::exists(cfg.outputPath));
  CHECK(fs::exists(cfg.manifestPath));

  PipelineResult second = Pipeline(cfg).run();
  CHECK(second.status == ExitStatus::Ok);
  CHECK(second.resumed);
  CHECK(second.alignmentErrorSamples == 0);
  CHECK(second.segmentsBeforeRecovery == first.segmentsCount);
}

TEST_CASE("损坏素材恢复退出码 2 且清单标注缺口", "[pipeline][corruption]") {
  auto dir = makeTempDir("recover");
  auto fx = fixture::generateAll(dir.string());
  PipelineConfig cfg;
  cfg.inputPath = fx.truncatedPath;
  cfg.outputPath = (dir / "out.m4a").string();
  cfg.manifestPath = (dir / "manifest.json").string();
  cfg.checkpointPath = (dir / "state.json").string();
  cfg.workDir = dir.string();
  cfg.targetBitrate = 128000;

  PipelineResult r = Pipeline(cfg).run();
  CHECK(r.status == ExitStatus::CorruptionRecovered);
  CHECK(r.recoverableSegments >= 1);
  CHECK(r.badOffsetBytes >= 0);
  CHECK(r.alignmentErrorSamples == 0);

  std::ifstream mf(cfg.manifestPath);
  std::string content((std::istreambuf_iterator<char>(mf)),
                      std::istreambuf_iterator<char>());
  CHECK(content.find("bad_offset_bytes") != std::string::npos);
}

TEST_CASE("重复片段被识别且不重复写入输出", "[pipeline][dedup]") {
  auto dir = makeTempDir("dedup");
  auto fx = fixture::generateAll(dir.string());
  PipelineConfig cfg;
  cfg.inputPath = fx.duplicatePath;
  cfg.outputPath = (dir / "out.m4a").string();
  cfg.manifestPath = (dir / "manifest.json").string();
  cfg.checkpointPath = (dir / "state.json").string();
  cfg.workDir = dir.string();
  cfg.targetBitrate = 128000;
  cfg.targetDurationMs = 2000;

  PipelineResult r = Pipeline(cfg).run();
  REQUIRE(r.status == ExitStatus::Ok);
  CHECK(r.duplicatesCount == 1);
  CHECK(r.alignmentErrorSamples == 0);
}

TEST_CASE("路径越界写入被拒绝", "[security]") {
  auto dir = makeTempDir("security");
  auto fx = fixture::generateAll(dir.string());
  PipelineConfig cfg;
  cfg.inputPath = fx.stereoPath;
  cfg.outputPath = (dir / "../escape.m4a").string();
  cfg.manifestPath = (dir / "manifest.json").string();
  cfg.checkpointPath = (dir / "state.json").string();
  cfg.workDir = dir.string();
  PipelineResult r = Pipeline(cfg).run();
  CHECK(r.status == ExitStatus::Failure);
  CHECK(r.fatal.code == ErrorCode::InvalidArgument);
}
