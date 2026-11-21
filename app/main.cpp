#include <cstdlib>
#include <filesystem>
#include <iostream>
#include <string>

#include "media/fixture.hpp"
#include "media/pipeline.hpp"

namespace fs = std::filesystem;
using namespace media;

namespace {

struct Args {
  std::string command;
  std::string input, output, manifest, checkpoint, work = "./tmp";
  int64_t bitrate = 192000;
  SampleTick maxSamples = 0;
  SampleTick targetDurationMs = 0;
  bool splitOnSilence = false;
  bool resume = false;
  bool generateFixtures = false;
  std::string policy = "skip";
};

std::string valueOf(int argc, char** argv, int& i, const std::string& key) {
  std::string arg = argv[i];
  auto eq = arg.find('=');
  if (eq != std::string::npos) return arg.substr(eq + 1);
  if (i + 1 < argc) return argv[++i];
  throw std::runtime_error("missing value for " + key);
}

Args parse(int argc, char** argv) {
  Args a;
  if (argc < 2) throw std::runtime_error("missing command");
  a.command = argv[1];
  for (int i = 2; i < argc; ++i) {
    std::string k = argv[i];
    if (k == "--input" || k.rfind("--input=", 0) == 0) a.input = valueOf(argc, argv, i, k);
    else if (k == "--output" || k.rfind("--output=", 0) == 0) a.output = valueOf(argc, argv, i, k);
    else if (k == "--manifest" || k.rfind("--manifest=", 0) == 0) a.manifest = valueOf(argc, argv, i, k);
    else if (k == "--checkpoint" || k.rfind("--checkpoint=", 0) == 0) a.checkpoint = valueOf(argc, argv, i, k);
    else if (k == "--work" || k.rfind("--work=", 0) == 0) a.work = valueOf(argc, argv, i, k);
    else if (k == "--bitrate" || k.rfind("--bitrate=", 0) == 0) a.bitrate = std::stoll(valueOf(argc, argv, i, k));
    else if (k == "--max-samples" || k.rfind("--max-samples=", 0) == 0) a.maxSamples = std::stoll(valueOf(argc, argv, i, k));
    else if (k == "--duration-ms" || k.rfind("--duration-ms=", 0) == 0) a.targetDurationMs = std::stoll(valueOf(argc, argv, i, k));
    else if (k == "--split-silence") a.splitOnSilence = true;
    else if (k == "--resume") a.resume = true;
    else if (k == "--generate-fixtures") a.generateFixtures = true;
    else if (k == "--policy" || k.rfind("--policy=", 0) == 0) a.policy = valueOf(argc, argv, i, k);
    else throw std::runtime_error("unknown argument: " + k);
  }
  return a;
}

int runProcess(const Args& a) {
  if (a.input.empty() || a.output.empty() || a.manifest.empty() ||
      a.checkpoint.empty()) {
    std::cerr << "process 需要 --input --output --manifest --checkpoint\n";
    return static_cast<int>(ExitStatus::Failure);
  }
  PipelineConfig cfg;
  cfg.inputPath = a.input;
  cfg.outputPath = a.output;
  cfg.manifestPath = a.manifest;
  cfg.checkpointPath = a.checkpoint;
  // 未显式给 --work 时，以输出文件所在目录为路径限定根，
  // 保证验收命令（输出位于用户指定目录）可直接运行。
  cfg.workDir = (!a.work.empty() && a.work != "./tmp")
                    ? a.work
                    : fs::path(a.output).parent_path().string();
  cfg.targetBitrate = a.bitrate;
  cfg.maxSegmentSamples = a.maxSamples;
  cfg.targetDurationMs = a.targetDurationMs;
  cfg.splitOnSilence = a.splitOnSilence;
  cfg.resume = a.resume;
  cfg.corruptionPolicy = a.policy == "abort" ? CorruptionPolicy::Abort
                                             : CorruptionPolicy::Skip;

  PipelineResult r = Pipeline(cfg).run();
  if (r.status == ExitStatus::Failure) {
    std::cerr << "失败: " << r.fatal.what() << "\n";
    return static_cast<int>(ExitStatus::Failure);
  }
  std::cout << "status=" << (r.status == ExitStatus::Ok ? "ok" : "recovered")
            << "\n";
  std::cout << "target_bitrate_bps=" << r.targetBitrateBps << "\n";
  std::cout << "actual_bitrate_bps=" << r.actualBitrateBps << "\n";
  std::cout << "segments_count=" << r.segmentsCount << "\n";
  std::cout << "silence_ms=" << r.silenceMs << "\n";
  std::cout << "clipped_samples=" << r.clippedSamples << "\n";
  std::cout << "duplicates_count=" << r.duplicatesCount << "\n";
  std::cout << "manifest_alignment_error_samples="
            << r.alignmentErrorSamples << "\n";
  std::cout << "bad_offset_bytes=" << r.badOffsetBytes << "\n";
  std::cout << "recoverable_segments=" << r.recoverableSegments << "\n";
  std::cout << "segments_before_recovery=" << r.segmentsBeforeRecovery
            << "\n";
  std::cout << "resumed=" << (r.resumed ? "true" : "false") << "\n";
  return static_cast<int>(r.status);
}

int runSelftest(const Args& a) {
  fs::create_directories(a.work);
  if (a.generateFixtures) {
    fixture::FixtureSet fx = fixture::generateAll(a.work);
    std::cout << "fixtures_generated:\n"
              << "  stereo=" << fx.stereoPath << "\n"
              << "  truncated=" << fx.truncatedPath << "\n"
              << "  non_integer=" << fx.nonIntegerPath << "\n"
              << "  duplicate=" << fx.duplicatePath << "\n";
  }
  // 演示流水线：正常素材，默认 2 秒一片以暴露段间重复内容。
  Args proc = a;
  proc.input = (fs::path(a.work) / "stereo.wav").string();
  proc.output = (fs::path(a.work) / "out.m4a").string();
  proc.manifest = (fs::path(a.work) / "manifest.json").string();
  proc.checkpoint = (fs::path(a.work) / "state.json").string();
  if (proc.targetDurationMs == 0) proc.targetDurationMs = 2000;
  return runProcess(proc);
}

}  // namespace

int main(int argc, char** argv) {
  Args a;
  try {
    a = parse(argc, argv);
  } catch (const std::exception& e) {
    std::cerr << "参数错误: " << e.what() << "\n";
    return static_cast<int>(ExitStatus::Failure);
  }
  try {
    if (a.command == "process") return runProcess(a);
    if (a.command == "selftest") return runSelftest(a);
    std::cerr << "未知命令: " << a.command
              << "（支持 process / selftest）\n";
    return static_cast<int>(ExitStatus::Failure);
  } catch (const std::exception& e) {
    std::cerr << "运行失败: " << e.what() << "\n";
    return static_cast<int>(ExitStatus::Failure);
  }
}
