// 媒体交付流水线 CLI。
// 退出码：0 成功；1 参数错误；2 处理失败（致命故障）。
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <string>
#include "media/generator.hpp"
#include "media/pipeline.hpp"
#include "media/quality_report.hpp"

using namespace media;

namespace {
struct Args {
  int seconds = 12;
  double driftPpm = 180.0;
  bool wrapPts = false;
  bool vfr = false;
  int corruptEvery = 0;
  int queuePackets = 64;
  int64_t targetBitrate = 1'800'000;
  int truncateAt = 0;
  std::string jsonPath;
  std::string outDir = "demo_out";
};

void usage() {
  std::cout <<
      "usage: media-deliver demo [options]\n"
      "  --seconds N           合成素材时长秒数（默认 12）\n"
      "  --drift-ppm PPM       注入线性时钟漂移（默认 180）\n"
      "  --wrap-pts            注入 33 位 PTS 回绕\n"
      "  --vfr                 注入可变帧间隔\n"
      "  --corrupt-every N     每 N 个包注入一个坏包（0 关闭）\n"
      "  --queue-packets N     有界队列容量（默认 64）\n"
      "  --target-bitrate BPS  目标视频码率（默认 1800000）\n"
      "  --truncate-at N       在第 N 个包处模拟截断（致命）\n"
      "  --json PATH           报告写入 JSON 文件\n"
      "  --out DIR             工作目录（默认 demo_out）\n";
}

bool parseInt(const char* s, long long& out) {
  try {
    size_t pos = 0;
    out = std::stoll(s, &pos);
    return pos == std::strlen(s);
  } catch (...) {
    return false;
  }
}
} // namespace

int main(int argc, char** argv) {
  if (argc < 2) {
    usage();
    return 1;
  }
  if (std::strcmp(argv[1], "--help") == 0 ||
      std::strcmp(argv[1], "-h") == 0) {
    usage();
    return 0;
  }
  if (std::strcmp(argv[1], "demo") != 0) {
    std::cerr << "error: unknown command '" << argv[1] << "'\n";
    return 1;
  }

  Args a;
  for (int i = 2; i < argc; ++i) {
    std::string k = argv[i];
    auto need = [&](std::string& v) -> bool {
      if (i + 1 >= argc) return false;
      v = argv[++i];
      return true;
    };
    long long n;
    if (k == "--seconds") {
      std::string v;
      if (!need(v) || !parseInt(v.c_str(), n) || n <= 0)
        return std::cerr << "error: invalid --seconds\n", 1;
      a.seconds = int(n);
    } else if (k == "--drift-ppm") {
      std::string v;
      if (!need(v)) return std::cerr << "error: invalid --drift-ppm\n", 1;
      try { a.driftPpm = std::stod(v); }
      catch (...) { return std::cerr << "error: invalid --drift-ppm\n", 1; }
    } else if (k == "--wrap-pts") a.wrapPts = true;
    else if (k == "--vfr") a.vfr = true;
    else if (k == "--corrupt-every") {
      std::string v;
      if (!need(v) || !parseInt(v.c_str(), n) || n < 0)
        return std::cerr << "error: invalid --corrupt-every\n", 1;
      a.corruptEvery = int(n);
    } else if (k == "--queue-packets") {
      std::string v;
      if (!need(v) || !parseInt(v.c_str(), n) || n <= 0)
        return std::cerr << "error: invalid --queue-packets\n", 1;
      a.queuePackets = int(n);
    } else if (k == "--target-bitrate") {
      std::string v;
      if (!need(v) || !parseInt(v.c_str(), n) || n <= 0)
        return std::cerr << "error: invalid --target-bitrate\n", 1;
      a.targetBitrate = n;
    } else if (k == "--truncate-at") {
      std::string v;
      if (!need(v) || !parseInt(v.c_str(), n) || n <= 0)
        return std::cerr << "error: invalid --truncate-at\n", 1;
      a.truncateAt = int(n);
    } else if (k == "--json") {
      if (!need(a.jsonPath))
        return std::cerr << "error: --json requires a path\n", 1;
    } else if (k == "--out") {
      if (!need(a.outDir))
        return std::cerr << "error: --out requires a dir\n", 1;
    } else {
      std::cerr << "error: unknown option '" << k << "'\n";
      return 1;
    }
  }

  // 1) 合成素材（截断演示同样需要完整源，截断发生在解封装阶段）。
  GenConfig gc;
  gc.outDir = a.outDir;
  gc.seconds = a.seconds;
  gc.driftPpm = a.driftPpm;
  gc.wrapPts = a.wrapPts;
  gc.vfr = a.vfr;
  gc.corruptEvery = a.corruptEvery;
  gc.targetBitrate = a.targetBitrate;
  GenResult gr;
  if (auto e = generateSynthetic(gc, gr); !e.empty()) {
    std::cerr << "error: generation failed: " << e << '\n';
    return 2;
  }

  // 2) 运行交付流水线。
  PipelineConfig pc;
  pc.workDir = a.outDir;
  pc.manifestPath = gr.manifestPath;
  pc.queuePackets = a.queuePackets;
  pc.truncateAt = a.truncateAt;
  pc.targetBitrateBps = a.targetBitrate;
  pc.jsonPath = a.jsonPath;
  pc.indexPath = a.outDir + "/keyframes.idx";
  pc.diagPath = a.outDir + "/diagnostics.jsonl";

  PipelineReport rep;
  TimeError fatal = runPipeline(pc, rep);
  if (isFatal(fatal)) {
    std::cerr << "fatal: " << rep.fatalDetail << '\n';
    std::cerr << "first_bad_packet=" << rep.firstBadPacket << '\n';
    return 2;
  }

  // 3) 聚合指标并输出。
  Manifest m;
  std::map<std::string, Sidecar> cars;
  std::string err;
  loadManifestAndSidecars(a.outDir, gr.manifestPath, m, cars, err);
  uint64_t bytes = mediaBytesOnDisk(a.outDir, m);
  int64_t durationUs = int64_t(a.seconds) * 1'000'000LL;
  QualityStats s = aggregateStats(rep, rep.queuePeak, a.targetBitrate,
                                  durationUs, bytes);

  std::cout << "packets_in=" << s.packetsIn << '\n'
            << "packets_repaired=" << s.packetsRepaired << '\n'
            << "wraps=" << s.wraps << '\n'
            << "drift_ppm=" << s.driftPpm << '\n'
            << "queue_peak=" << s.queuePeak << '\n'
            << "target_bitrate_bps=" << s.targetBitrateBps << '\n'
            << "actual_bitrate_bps=" << s.actualBitrateBps << '\n'
            << "bitrate_error_pct=" << s.bitrateErrorPct << '\n'
            << "psnr_db=" << s.psnrDb << '\n'
            << "manifest_alignment_ms=" << s.alignmentErrorMs << '\n'
            << "seek_error_ms=" << s.seekErrorMs << '\n';

  if (!a.jsonPath.empty()) {
    std::ofstream jf(a.jsonPath);
    if (!jf) {
      std::cerr << "error: cannot write json report\n";
      return 2;
    }
    jf << renderReportJson(s);
  }
  return 0;
}
