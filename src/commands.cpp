// commands.cpp — generate-fixture / analyze / verify 处理器
#include "commands.h"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <fstream>
#include <iostream>
#include <string>
#include <vector>

#include "clip_detector.h"
#include "fixture.h"
#include "interpolator.h"
#include "repair_pipeline.h"
#include "report_writer.h"
#include "wav_io.h"

namespace popfix {

namespace {

// 轻量具名参数读取（与 main 的 Args 同风格，处理器内自给自足）。
struct Opt {
  static bool get(const std::vector<std::string>& t, const std::string& name,
                  std::string& out) {
    for (std::size_t i = 0; i + 1 < t.size(); ++i)
      if (t[i] == name) {
        out = t[i + 1];
        return true;
      }
    return false;
  }
  static bool get(const std::vector<std::string>& t, const std::string& name,
                  double& out) {
    std::string s;
    if (!get(t, name, s)) return false;
    try {
      std::size_t used = 0;
      out = std::stod(s, &used);
      return used == s.size();
    } catch (...) {
      return false;
    }
  }
  template <typename Int>
  static bool get_int(const std::vector<std::string>& t,
                      const std::string& name, Int& out) {
    std::string s;
    if (!get(t, name, s)) return false;
    try {
      std::size_t used = 0;
      long long v = std::stoll(s, &used);
      if (used != s.size() || v < 0) return false;
      out = static_cast<Int>(v);
      return true;
    } catch (...) {
      return false;
    }
  }
  static bool has(const std::vector<std::string>& t,
                  const std::string& name) {
    return std::find(t.begin(), t.end(), name) != t.end();
  }
};

}  // namespace

ExitCode cmd_generate_fixture(const std::vector<std::string>& tokens) {
  std::string out;
  if (!Opt::get(tokens, "--out", out)) {
    std::cerr << "generate-fixture 缺少 --out 参数\n";
    return ExitCode::UsageOrInputError;
  }
  FixtureSpec spec;
  Opt::get_int(tokens, "--rate", spec.sample_rate);
  Opt::get_int(tokens, "--channels", spec.channels);
  Opt::get_int(tokens, "--frames", spec.frames);
  unsigned long long seed = spec.seed;
  if (Opt::get_int(tokens, "--seed", seed)) spec.seed = seed;

  if (spec.channels != 1 && spec.channels != 2) {
    std::cerr << "--channels 仅支持 1 或 2\n";
    return ExitCode::UsageOrInputError;
  }
  if (spec.sample_rate == 0 || spec.frames == 0) {
    std::cerr << "--rate/--frames 必须为正数\n";
    return ExitCode::UsageOrInputError;
  }
  if (spec.frames > kMaxFrames) {
    std::cerr << "帧数超过安全上限 " << kMaxFrames << "\n";
    return ExitCode::UsageOrInputError;
  }
  // 注入位置固定，过小的帧装不下事件。
  if (spec.frames < 73000) {
    std::cerr << "--frames 过小，夹具至少需要 73000 帧容纳注入事件\n";
    return ExitCode::UsageOrInputError;
  }

  try {
    AudioBuffer audio = generate_fixture(spec);
    write_wav_atomic(out, audio);
  } catch (const WavIOError& e) {
    std::cerr << "generate-fixture 失败: " << e.what() << "\n";
    return e.write_failure() ? ExitCode::OutputFailure
                             : ExitCode::UsageOrInputError;
  }
  std::cout << "fixture=" << out << " frames=" << spec.frames
            << " channels=" << spec.channels << " rate=" << spec.sample_rate
            << " seed=" << spec.seed << "\n";
  return ExitCode::Ok;
}

ExitCode cmd_analyze(const std::vector<std::string>& tokens) {
  std::string in, repair, report;
  if (!Opt::get(tokens, "--in", in)) {
    std::cerr << "analyze 缺少 --in 参数\n";
    return ExitCode::UsageOrInputError;
  }
  Opt::get(tokens, "--repair", repair);
  Opt::get(tokens, "--report", report);
  const bool dry_run =
      Opt::has(tokens, "--dry-run") || repair.empty();

  DetectorConfig dcfg;
  RepairConfig rcfg;
  Opt::get(tokens, "--threshold", dcfg.threshold);
  Opt::get_int(tokens, "--context", rcfg.context);
  Opt::get_int(tokens, "--threads", rcfg.threads);
  if (rcfg.threads == 0) rcfg.threads = 1;
  if (!(dcfg.threshold > 0.0 && dcfg.threshold <= 1.0)) {
    std::cerr << "--threshold 必须在 (0, 1] 区间\n";
    return ExitCode::UsageOrInputError;
  }

  WavReadInfo info;
  AudioBuffer audio;
  try {
    audio = read_wav(in, info);
    if (!repair.empty()) validate_output_path(repair);
    if (!report.empty()) validate_output_path(report);
  } catch (const WavIOError& e) {
    std::cerr << "analyze 输入错误: " << e.what() << "\n";
    return e.write_failure() ? ExitCode::OutputFailure
                             : ExitCode::UsageOrInputError;
  }

  RepairPipeline pipeline(dcfg, rcfg);
  RepairResult result = pipeline.run(audio, dry_run);

  // 正常模式：先写修复音频，再写报告；任一落盘失败都不得宣称成功。
  if (!dry_run) {
    try {
      write_wav_atomic(repair, audio);
    } catch (const WavIOError& e) {
      std::cerr << "修复音频写出失败: " << e.what() << "\n";
      return ExitCode::OutputFailure;
    }
  }
  if (!report.empty()) {
    try {
      // 报告需要原始参数与源元信息（记录在 RepairResult 内）。
      write_report(report, result, dcfg, rcfg, dry_run);
    } catch (const WavIOError& e) {
      std::cerr << "报告写出失败: " << e.what() << "\n";
      return ExitCode::OutputFailure;
    }
  }

  for (const auto& line : build_summary_lines(result, audio.frames()))
    std::cout << line << "\n";
  std::cout << "dry_run=" << (dry_run ? "true" : "false") << "\n";
  if (!result.success) return ExitCode::DetectOrRepairFailure;
  return ExitCode::Ok;
}

// verify：重读原始与修复文件，核对帧数/声道/采样合法性，并复算残差。
ExitCode cmd_verify(const std::vector<std::string>& tokens) {
  std::string orig_path, rep_path, report_path;
  if (!Opt::get(tokens, "--original", orig_path) ||
      !Opt::get(tokens, "--repaired", rep_path) ||
      !Opt::get(tokens, "--report", report_path)) {
    std::cerr << "verify 需要 --original、--repaired、--report 三个参数\n";
    return ExitCode::UsageOrInputError;
  }

  WavReadInfo info_o, info_r;
  AudioBuffer original, repaired;
  StoredReport stored;
  try {
    original = read_wav(orig_path, info_o);
    repaired = read_wav(rep_path, info_r);
    stored = read_report(report_path);
  } catch (const WavIOError& e) {
    std::cerr << "verify 读取失败: " << e.what() << "\n";
    return e.write_failure() ? ExitCode::OutputFailure
                             : ExitCode::UsageOrInputError;
  }

  std::vector<std::string> failures;

  // 1. 帧数不变、声道数不变、采样率与位深保持。
  if (repaired.frames() != original.frames())
    failures.push_back("帧数不一致: original=" +
                       std::to_string(original.frames()) + " repaired=" +
                       std::to_string(repaired.frames()));
  if (repaired.channels != original.channels)
    failures.push_back("声道数不一致");
  if (repaired.sample_rate != original.sample_rate)
    failures.push_back("采样率不一致");
  if (repaired.bits_per_sample != original.bits_per_sample)
    failures.push_back("位深不一致");

  // 2. 无非法采样：所有样本落在 [-1, 1]，且修复后不得再达满幅削波。
  for (double s : repaired.data) {
    if (!(s >= -1.0 && s <= 1.0)) {
      failures.push_back("存在越界非法采样");
      break;
    }
  }

  // 3. 报告头字段与实际文件一致。
  if (stored.frames != 0 && stored.frames != repaired.frames())
    failures.push_back("报告帧数与修复文件不符");
  if (stored.channels != 0 && stored.channels != repaired.channels)
    failures.push_back("报告声道数与修复文件不符");
  if (stored.repairs.size() != stored.stats.event_count &&
      stored.stats.event_count != 0)
    failures.push_back("报告事件数与事件数组长度不符");

  // 4. 残差统计可复现：用原始/修复文件按报告窗口重新计算全局 MSE。
  const double recomputed =
      RepairPipeline::recompute_residual(original, repaired,
                                         stored.repairs);
  const double reported = stored.stats.residual_energy;
  const double tol = std::max(1e-9, 1e-3 * std::fabs(reported));
  if (std::fabs(recomputed - reported) > tol)
    failures.push_back("残差不可复现: report=" +
                       std::to_string(reported) + " recomputed=" +
                       std::to_string(recomputed));

  // 5. 对修复文件再跑一次检测：满幅削波应已消除（阈值沿用报告值）。
  DetectorConfig dcfg;
  dcfg.threshold = stored.threshold > 0 ? stored.threshold : 0.995;
  ClipDetector detector(dcfg);
  const auto remaining = detector.detect(repaired);
  if (!remaining.empty())
    failures.push_back("修复后仍检测到 " +
                       std::to_string(remaining.size()) + " 个削波事件");

  std::printf("verify_frames=%zu verify_channels=%u residual_report=%.9g "
              "residual_recomputed=%.9g\n",
              repaired.frames(),
              static_cast<unsigned>(repaired.channels), reported,
              recomputed);
  if (failures.empty()) {
    std::cout << "verify=PASS\n";
    return ExitCode::Ok;
  }
  std::cout << "verify=FAIL\n";
  for (const auto& msg : failures) std::cerr << "  - " << msg << "\n";
  return ExitCode::VerifyFailure;
}

}  // namespace popfix
