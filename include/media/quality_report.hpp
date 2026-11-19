#pragma once
// 质量统计：码率偏差、PSNR、帧数、清单对齐误差、机器可读诊断与 JSON 报告。
#include <cstdint>
#include <fstream>
#include <memory>
#include <string>
#include <vector>
#include "media/manifest.hpp"
#include "media/packet.hpp"
#include "media/time_model.hpp"

namespace media {

struct QualityStats {
  uint64_t packetsIn = 0;
  uint64_t packetsRepaired = 0;
  uint64_t wraps = 0;
  uint64_t queuePeak = 0;
  uint64_t framesDecoded = 0;
  uint64_t framesCorrupt = 0;
  int64_t durationUs = 0;
  uint64_t payloadBytes = 0;
  int64_t targetBitrateBps = 0;
  int64_t actualBitrateBps = 0;
  double bitrateErrorPct = 0;
  double psnrDb = 0;
  double alignmentErrorMs = 0; // 最大边界误差
  double seekErrorMs = 0;      // 平均落点误差
  double driftPpm = 0;
  bool lowConfidence = false;
};

// 累加解码后帧的 MSE（Y 平面），最终汇总为 PSNR（分贝）。
class PsnrAccumulator {
public:
  void addFrame(double mse);
  double psnrDb() const; // 无有效帧返回 0
  uint64_t frames() const { return frames_; }

private:
  double sumPsnr_ = 0;
  uint64_t frames_ = 0;
};

double computeBitrateErrorPct(int64_t target, int64_t actual);

// 机器可读诊断（每行一个 JSON 对象，JSONL），可恢复故障逐条写入。
class DiagnosticLog {
public:
  explicit DiagnosticLog(std::string path) : path_(std::move(path)) {}
  TimeError open();
  TimeError append(const TimeEvent& ev);
  TimeError close();

private:
  std::string path_;
  bool opened_ = false;
  std::unique_ptr<std::ofstream> file_;
};

std::string renderReportJson(const QualityStats& s);
} // namespace media
