#include "timeline.hpp"

#include <algorithm>
#include <climits>

namespace mb {

namespace {
struct Occ {
  int64_t start, end, priority;
  std::string role;  // 轨角色：仅同角色重叠才记为竞争错误
};

// 在已占用（不重叠、按起点排序）区间集合中，从 [lo,hi) 扣除重叠，返回剩余段。
std::vector<std::pair<int64_t, int64_t>> subtract(
    const std::vector<Occ>& occ, int64_t lo, int64_t hi) {
  std::vector<std::pair<int64_t, int64_t>> pieces{{lo, hi}};
  for (const Occ& o : occ) {
    if (o.end <= lo || o.start >= hi) continue;  // 不相交
    std::vector<std::pair<int64_t, int64_t>> next;
    for (auto [a, b] : pieces) {
      if (o.end <= a || o.start >= b) {
        next.emplace_back(a, b);  // 与该占用段不相交，原样保留
      } else {
        if (a < o.start) next.emplace_back(a, o.start);
        if (b > o.end) next.emplace_back(o.end, b);
      }
    }
    pieces.swap(next);
  }
  return pieces;
}

// 合并方式插入占用集合，保持有序且互不重叠。
void insert_occ(std::vector<Occ>& occ, int64_t s, int64_t e, int prio,
                const std::string& role) {
  Occ seg{s, e, prio, role};
  std::vector<Occ> out;
  // 收集与 seg 相邻/相交的段并合并。
  size_t i = 0;
  for (; i < occ.size(); ++i) {
    if (occ[i].end < s) {
      out.push_back(occ[i]);
    } else if (occ[i].start > e) {
      break;
    } else {
      seg.start = std::min(seg.start, occ[i].start);
      seg.end = std::max(seg.end, occ[i].end);
      seg.priority = std::max(seg.priority, occ[i].priority);
    }
  }
  out.push_back(seg);
  for (; i < occ.size(); ++i) out.push_back(occ[i]);
  occ.swap(out);
}

// 查找与 [lo,hi) 相交占用段中的最高优先级。
int overlapper_priority(const std::vector<Occ>& occ, int64_t lo,
                        int64_t hi) {
  int best = INT32_MIN;
  for (const Occ& o : occ) {
    if (o.start < hi && o.end > lo) best = std::max(best, (int)o.priority);
  }
  return best;
}

// 是否存在与 [lo,hi) 相交且同角色的占用段（同类轨竞争才算裁定错误）。
bool has_same_role_overlap(const std::vector<Occ>& occ, int64_t lo,
                           int64_t hi, const std::string& role) {
  for (const Occ& o : occ)
    if (o.start < hi && o.end > lo && o.role == role) return true;
  return false;
}
}  // namespace

Timeline compose_timeline(const std::vector<TrackSpec>& tracksIn,
                          int64_t global_end) {
  Timeline tl;
  if (tracksIn.size() > kMaxTracks) {
    tl.decisions.push_back({"*", kTlTrackLimit, "track count exceeds limit",
                            -1});
    tl.rejected++;
    return tl;
  }
  // 稳定的优先级处理顺序：优先级降序，同优先级保持输入顺序（先到先得）。
  std::vector<size_t> order(tracksIn.size());
  for (size_t i = 0; i < order.size(); ++i) order[i] = i;
  std::stable_sort(order.begin(), order.end(), [&](size_t a, size_t b) {
    return tracksIn[a].priority > tracksIn[b].priority;
  });

  std::vector<Occ> occ;
  for (size_t ti : order) {
    const TrackSpec& tr = tracksIn[ti];
    if (tr.clips.empty()) {
      tl.decisions.push_back({tr.id, kTlEmptyTrack, "track has no clips", -1});
      continue;
    }
    for (const Clip& c : tr.clips) {
      auto reject = [&](const char* code, const std::string& msg) {
        tl.decisions.push_back({tr.id, code, msg, c.source_offset});
        tl.rejected++;
      };
      if (c.start < 0) {
        reject(kTlNegative, "negative clip start");
        continue;
      }
      if (c.end <= c.start) {
        reject(kTlZeroLen, "non-positive clip length");
        continue;
      }
      int64_t lo = c.start, hi = c.end;
      // 轨边界与全局边界：起点越过边界即拒绝；仅尾部越界则裁尾。
      if (lo < tr.boundary_start) {
        reject(kTlOutOfBounds, "clip starts before track boundary");
        continue;
      }
      int64_t hard_end = tr.boundary_end;
      if (global_end > 0) hard_end = hard_end == 0 ? global_end
                                                    : std::min(hard_end,
                                                               global_end);
      if (hard_end > 0 && lo >= hard_end) {
        reject(kTlOutOfBounds, "clip starts at/after hard end boundary");
        continue;
      }
      if (hard_end > 0 && hi > hard_end) hi = hard_end;
      if (hi <= lo) {
        reject(kTlOutOfBounds, "clip empty after boundary clamp");
        continue;
      }
      // 扣除高优先级/先到同优先级已占用区。
      int other_prio = overlapper_priority(occ, lo, hi);
      bool same_role = has_same_role_overlap(occ, lo, hi, tr.role);
      auto pieces = subtract(occ, lo, hi);
      if (pieces.empty()) {
        if (same_role) {
          const char* code = other_prio > tr.priority ? kTlOverlapLow
                                                       : kTlOverlapTie;
          reject(code, "clip fully covered by higher/earlier same-role track");
        }
        // 跨角色（如字幕落在节目轨上）被完整覆盖：静默跳过，不算错误。
        continue;
      }
      bool reduced = pieces.size() > 1 || pieces[0].first != lo ||
                     pieces[0].second != hi || pieces.back().second != hi;
      if (reduced && same_role) {
        const char* code = other_prio > tr.priority ? kTlOverlapLow
                                                     : kTlOverlapTie;
        tl.decisions.push_back({tr.id, code,
                                "same-role clip partially overlapped; "
                                "remainder kept",
                                c.source_offset});
        tl.clipped++;
      }
      for (auto [a, b] : pieces) {
        tl.segments.push_back({tr.id, c.source_ref, a, b, tr.priority,
                               c.source_offset});
        insert_occ(occ, a, b, tr.priority, tr.role);
      }
    }
  }

  std::sort(tl.segments.begin(), tl.segments.end(),
            [](const PlacedSegment& a, const PlacedSegment& b) {
              if (a.start != b.start) return a.start < b.start;
              if (a.priority != b.priority) return a.priority > b.priority;
              return a.track_id < b.track_id;
            });
  if (!tl.segments.empty()) {
    tl.start = tl.segments.front().start;
    tl.end = tl.segments.back().end;
  }
  return tl;
}

}  // namespace mb
