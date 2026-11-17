// repair_pipeline.cpp — 检测结果复用、窗口扩展、分组并行、残差与统计
#include "repair_pipeline.h"

#include <algorithm>
#include <cmath>
#include <thread>

namespace popfix {

namespace {

double peak_of(const AudioBuffer& a) {
  double p = 0.0;
  for (double s : a.data) p = std::max(p, std::fabs(s));
  return p;
}

// 把修复窗口重叠/触碰的事件分到同一组；组间互不重叠，可安全并行。
std::vector<std::vector<std::size_t>> group_events(
    const std::vector<EventRepair>& repairs) {
  std::vector<std::vector<std::size_t>> groups;
  long long last_end = -1;
  for (std::size_t i = 0; i < repairs.size(); ++i) {
    const long long start = static_cast<long long>(repairs[i].repair_start);
    if (groups.empty() || start > last_end + 1) {
      groups.push_back({i});
    } else {
      groups.back().push_back(i);
    }
    last_end = std::max<long long>(
        last_end, static_cast<long long>(repairs[i].repair_end));
  }
  return groups;
}

}  // namespace

std::vector<std::vector<bool>> RepairPipeline::build_clipped_mask(
    std::size_t frames, std::uint16_t channels,
    const std::vector<ClipEvent>& events) {
  std::vector<std::vector<bool>> clipped(
      frames, std::vector<bool>(channels, false));
  for (const ClipEvent& e : events) {
    for (std::size_t f = e.start_frame; f <= e.end_frame; ++f) {
      for (std::uint8_t c = 0; c < channels; ++c)
        if (e.channel_mask & (1u << c)) clipped[f][c] = true;
    }
  }
  return clipped;
}

RepairResult RepairPipeline::run(AudioBuffer& audio, bool dry_run) {
  RepairResult result;
  const std::size_t frames = audio.frames();
  const std::uint16_t nch = audio.channels;
  result.source_rate = audio.sample_rate;
  result.source_channels = nch;
  result.source_frames = frames;

  result.events = detector_.detect(audio);
  result.stats.event_count = result.events.size();
  result.stats.peak_before = peak_of(audio);

  // 原始副本：残差与峰值对照的唯一数据源。
  const AudioBuffer original = audio;

  // 先按检测事件构造修复窗口（外扩 context，边界裁剪并记录）。
  result.repairs.reserve(result.events.size());
  for (const ClipEvent& e : result.events) {
    EventRepair r;
    r.event_id = e.event_id;
    r.start_frame = e.start_frame;
    r.end_frame = e.end_frame;
    r.channel_mask = e.channel_mask;
    const long long ctx = static_cast<long long>(rcfg_.context);
    long long rs = static_cast<long long>(e.start_frame) - ctx;
    long long re = static_cast<long long>(e.end_frame) + ctx;
    if (rs < 0 || re >= static_cast<long long>(frames)) {
      r.clipped_context = true;
    }
    r.repair_start = static_cast<std::size_t>(std::max<long long>(rs, 0));
    r.repair_end =
        static_cast<std::size_t>(std::min<long long>(re, frames - 1));
    result.repairs.push_back(r);
  }

  const auto clipped =
      build_clipped_mask(frames, nch, result.events);

  if (!dry_run) {
    const auto groups = group_events(result.repairs);
    unsigned workers = std::max(1u, rcfg_.threads);
    workers = std::min(workers, std::thread::hardware_concurrency()
                                    ? std::thread::hardware_concurrency()
                                    : workers);

    auto process_group = [&](const std::vector<std::size_t>& ids) {
      Interpolator interp;
      for (std::size_t idx : ids) {
        EventRepair& r = result.repairs[idx];
        bool any_failed = false;
        for (std::uint8_t c = 0; c < nch; ++c) {
          if (!(r.channel_mask & (1u << c))) continue;
          InterpRequest req{r.repair_start, r.repair_end, c};
          InterpOutcome oc = interp.repair_channel(audio, clipped, req);
          for (RepairWarning& w : oc.warnings) w.event_id = r.event_id;
          r.warnings.insert(r.warnings.end(), oc.warnings.begin(),
                            oc.warnings.end());
          if (!oc.ok) any_failed = true;
          if (oc.clipped_context) r.clipped_context = true;
        }
        r.failed = any_failed;
      }
    };

    if (workers <= 1 || groups.size() <= 1) {
      for (const auto& g : groups) process_group(g);
    } else {
      // 持续派发互不重叠的事件组；在途线程到达 worker 上限时全部回收，
      // 组间窗口不相交，因此对 audio 的写入不存在竞态。
      std::vector<std::thread> pool;
      for (const auto& g : groups) {
        pool.emplace_back(process_group, g);
        if (pool.size() >= workers) {
          for (auto& t : pool) t.join();
          pool.clear();
        }
      }
      for (auto& t : pool) t.join();
    }
  }

  // ---- 残差与统计（dry-run 时残差为 0，样本未改动）------------------
  // 残差与修复帧只统计检测事件自身区间 [start_frame, end_frame]；
  // 外扩窗口仅为插值提供上下文，不计入修复量。
  double sum_sq = 0.0;
  std::size_t replaced = 0;
  std::vector<bool> frame_touched(frames, false);
  for (const EventRepair& r : result.repairs) {
    double event_sq = 0.0;
    std::size_t event_n = 0;
    for (std::size_t f = r.start_frame; f <= r.end_frame; ++f) {
      bool touched = false;
      for (std::uint8_t c = 0; c < nch; ++c) {
        if (!(r.channel_mask & (1u << c))) continue;
        const double d =
            audio.sample(f, c) - original.sample(f, c);
        if (d != 0.0) {
          event_sq += d * d;
          sum_sq += d * d;
          ++replaced;
          ++event_n;
          touched = true;
        }
      }
      if (touched) frame_touched[f] = true;
    }
    // EventRepair 残差：该事件被替换样本的均方值。
    result.repairs[r.event_id].residual_energy =
        event_n > 0 ? event_sq / static_cast<double>(event_n) : 0.0;
  }

  result.stats.repaired_samples = replaced;
  result.stats.repaired_frames = std::count(
      frame_touched.begin(), frame_touched.end(), true);
  // 全局残差能量：修复引入的全部平方差摊到整个缓冲的样本数上（全局 MSE）。
  // 满幅削波样本与修复值的局部差接近 1，但修复点占比极低，归一化后落在
  // 1e-5..1e-2 的审听量级；verify 用同一口径复算保证可复现。
  result.stats.residual_energy =
      audio.data.empty()
          ? 0.0
          : sum_sq / static_cast<double>(audio.data.size());
  result.stats.peak_after = peak_of(audio);

  // 汇总告警并做结果一致性校验：失败事件必须显式可见。
  for (const EventRepair& r : result.repairs) {
    result.warnings.insert(result.warnings.end(), r.warnings.begin(),
                           r.warnings.end());
  }
  const bool any_failed =
      std::any_of(result.repairs.begin(), result.repairs.end(),
                  [](const EventRepair& r) { return r.failed; });
  result.success = !any_failed;
  return result;
}

double RepairPipeline::recompute_residual(
    const AudioBuffer& original, const AudioBuffer& repaired,
    const std::vector<EventRepair>& repairs) {
  if (original.data.size() != repaired.data.size() ||
      original.channels != repaired.channels) {
    return -1.0;
  }
  double sum_sq = 0.0;
  for (const EventRepair& r : repairs) {
    // 与 run() 同口径：只在检测事件自身区间复算，外扩上下文不计入。
    for (std::size_t f = r.start_frame; f <= r.end_frame; ++f) {
      for (std::uint8_t c = 0; c < original.channels; ++c) {
        if (!(r.channel_mask & (1u << c))) continue;
        const double d =
            repaired.sample(f, c) - original.sample(f, c);
        if (d != 0.0) sum_sq += d * d;
      }
    }
  }
  // 全局 MSE：平方差摊到整个缓冲，与 RepairStats.residual_energy 同口径。
  return original.data.empty()
             ? 0.0
             : sum_sq / static_cast<double>(original.data.size());
}

}  // namespace popfix
