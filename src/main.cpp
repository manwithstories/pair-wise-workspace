// 命令行入口：解析参数并分发到批处理管线，纯终端、无图形依赖。
// 退出码：0 合格 / 2 可恢复错误 / 3 检查点或输入校验失败 /
//         4 指标超限 / 5 构建或参数错误。
#include <algorithm>
#include <cstring>
#include <filesystem>
#include <iostream>
#include <string>
#include <vector>

extern "C" {
#include <libavutil/log.h>
}

#include "pipeline.hpp"
#include "report.hpp"

namespace mb {
namespace {

struct Args {
  std::string command;
  std::string work = "demo-run";
  std::string fixtures_mode = "good";  // good | damaged
  bool resume = false;
  bool json = false;
  bool valid = true;
  std::string invalid_reason;
  int64_t target_bitrate = 4'000'000;
  std::string container = "matroska";
  int64_t chunk_frames = 120;
  int64_t queue_capacity = 8;
  int64_t slow_consumer_ms = 0;
  int emit_frames = 12;
};

bool parse_long(const std::string& tok, const std::string& name,
                std::string* val) {
  std::string prefix = "--" + name + "=";
  if (tok.rfind(prefix, 0) == 0) { *val = tok.substr(prefix.size()); return true; }
  return false;
}

int64_t to_int(const std::string& s, bool* ok) {
  try { size_t used = 0; long long v = std::stoll(s, &used);
    *ok = (used == s.size()); return v; }
  catch (...) { *ok = false; return 0; }
}

Args parse_args(int argc, char** argv) {
  Args a;
  if (argc < 2) { a.valid = false; a.invalid_reason = "missing command"; return a; }
  a.command = argv[1];
  for (int i = 2; i < argc; ++i) {
    std::string t = argv[i];
    std::string val;
    auto next = [&](std::string* out) -> bool {
      if (i + 1 < argc) { *out = argv[++i]; return true; }
      a.valid = false; a.invalid_reason = t + " requires a value"; return false;
    };
    bool okv = true;
    if (parse_long(t, "work", &val)) a.work = val;
    else if (parse_long(t, "fixtures", &val)) a.fixtures_mode = val;
    else if (parse_long(t, "target-bitrate", &val))
      a.target_bitrate = to_int(val, &okv);
    else if (parse_long(t, "container", &val)) a.container = val;
    else if (parse_long(t, "chunk-frames", &val))
      a.chunk_frames = to_int(val, &okv);
    else if (parse_long(t, "queue-capacity", &val))
      a.queue_capacity = to_int(val, &okv);
    else if (parse_long(t, "slow-consumer-ms", &val))
      a.slow_consumer_ms = to_int(val, &okv);
    else if (parse_long(t, "emit-frames", &val))
      a.emit_frames = static_cast<int>(to_int(val, &okv));
    else if (t == "--resume") a.resume = true;
    else if (t == "--json") a.json = true;
    else if (t == "--work" && next(&a.work)) {}
    else if (t == "--fixtures" && next(&a.fixtures_mode)) {}
    else if (t == "--container" && next(&a.container)) {}
    else if (t == "--target-bitrate" && next(&val))
      a.target_bitrate = to_int(val, &okv);
    else if (t == "--chunk-frames" && next(&val))
      a.chunk_frames = to_int(val, &okv);
    else if (t == "--queue-capacity" && next(&val))
      a.queue_capacity = to_int(val, &okv);
    else if (t == "--slow-consumer-ms" && next(&val))
      a.slow_consumer_ms = to_int(val, &okv);
    else if (t == "--emit-frames" && next(&val))
      a.emit_frames = static_cast<int>(to_int(val, &okv));
    else { a.valid = false; a.invalid_reason = "unknown argument: " + t; }
    if (!okv) { a.valid = false; a.invalid_reason = "bad integer in " + t; }
  }
  // 参数有界校验：队列容量硬性上限，码率/块帧数为正。
  if (a.queue_capacity < 1 ||
      a.queue_capacity > static_cast<int64_t>(kMaxQueueCapacity)) {
    a.valid = false;
    a.invalid_reason = "queue capacity out of [1, 256]";
  }
  if (a.chunk_frames < 1 || a.target_bitrate < 1 || a.emit_frames < 0) {
    a.valid = false;
    a.invalid_reason = "bitrate/chunk/emit must be positive";
  }
  if (a.fixtures_mode != "good" && a.fixtures_mode != "damaged") {
    a.valid = false;
    a.invalid_reason = "fixtures must be good or damaged";
  }
  return a;
}

int run_demo(const Args& a) {
  PipelineConfig pc;
  pc.work_dir = a.work;
  pc.damaged = (a.fixtures_mode == "damaged");
  pc.resume = a.resume;
  pc.target_bitrate_bps = a.target_bitrate;
  pc.container = a.container;
  pc.chunk_frames = a.chunk_frames;
  pc.queue_capacity = static_cast<size_t>(a.queue_capacity);
  pc.slow_consumer_ms = a.slow_consumer_ms;
  pc.emit_frames = a.emit_frames;

  // --resume 自演示：全新目录上先建立检查点，再以续跑模式重跑，
  // 使单条命令即可报告 checkpoint_resumed_blocks（中断后续跑语义）。
  bool seeded_now = false;
  ReportInputs seed_stats;
  if (a.resume) {
    auto index = std::filesystem::path(a.work) / "checkpoint" / "index.cpi";
    if (!std::filesystem::exists(index)) {
      PipelineConfig seed = pc;
      seed.resume = false;
      PipelineOutcome seeded = run_pipeline(seed);
      if (!seeded.fatal.empty()) {
        std::cerr << "fatal: " << seeded.fatal << '\n';
        return 5;
      }
      if (static_cast<bool>(seeded.checkpoint_error))
        return decide_exit_code(build_report(seeded.report,
                                             &seeded.checkpoint_error),
                                &seeded.checkpoint_error, false);
      seeded_now = true;
      seed_stats = seeded.report;  // 保留 seed 阶段的队列背压统计
    }
  }

  PipelineOutcome po = run_pipeline(pc);
  if (!po.fatal.empty()) {
    std::cerr << "fatal: " << po.fatal << '\n';
    return 5;
  }
  // 续跑跳过重放已完成块，队列可能不流动；回填 seed 阶段真实背压峰值。
  if (seeded_now) {
    po.report.queue_peak_items = std::max(po.report.queue_peak_items,
                                          seed_stats.queue_peak_items);
    po.report.queue_peak_bytes = std::max(po.report.queue_peak_bytes,
                                          seed_stats.queue_peak_bytes);
    po.report.queue_push_waits = std::max(po.report.queue_push_waits,
                                          seed_stats.queue_push_waits);
    po.report.peak_memory_bytes = std::max(po.report.peak_memory_bytes,
                                           seed_stats.peak_memory_bytes);
  }
  ReportResult rr = build_report(po.report,
      static_cast<bool>(po.checkpoint_error) ? &po.checkpoint_error : nullptr);
  if (a.json) std::cout << rr.json << '\n';
  else {
    std::cout << "media_batch run complete (work=" << a.work << ")\n"
              << "metrics_ok=" << (rr.metrics_ok ? "true" : "false")
              << " exit=" << po.exit_code << "\n";
  }
  return decide_exit_code(rr,
      static_cast<bool>(po.checkpoint_error) ? &po.checkpoint_error : nullptr,
      false);
}

int run(int argc, char** argv) {
  Args a = parse_args(argc, argv);
  if (!a.valid) {
    std::cerr << "argument error: " << a.invalid_reason << '\n';
    return 5;
  }
  if (a.command == "demo") return run_demo(a);
  std::cerr << "unknown command: " << a.command
            << " (supported: demo)\n";
  return 5;
}

}  // namespace
}  // namespace mb

int main(int argc, char** argv) {
  av_log_set_level(AV_LOG_QUIET);  // 最早抑制 FFmpeg 日志，保持终端输出干净
  if (argc >= 2 && std::strcmp(argv[1], "--help") == 0) {
    std::cout
        << "media_batch — 电视台母版批处理工具\n"
        << "用法: media_batch demo [options]\n"
        << "选项: --work DIR --fixtures good|damaged --resume --json\n"
        << "      --target-bitrate N --container FMT --chunk-frames N\n"
        << "      --queue-capacity N(<=256) --slow-consumer-ms N\n"
        << "      --emit-frames N\n";
    return 0;
  }
  if (argc >= 2 && std::strcmp(argv[1], "version") == 0) {
    std::cout << "media_batch 1.0.0\n";
    return 0;
  }
  return mb::run(argc, argv);
}
