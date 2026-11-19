#include "gap_estimator.hpp"

#include <algorithm>
#include <cstdint>

namespace mfs {

void estimate_lost_ranges(const std::vector<SequenceGap>& gaps,
                          const std::vector<RepairedFrame>& frames,
                          const GapEstimateOptions& options,
                          std::vector<LostRange>& out) {
  for (const SequenceGap& gap : gaps) {
    if (gap.last_seq < gap.first_seq) {
      continue;
    }
    const int64_t count = static_cast<int64_t>(gap.last_seq - gap.first_seq) + 1;
    const int64_t nominal = options.nominal_interval_us;
    int64_t start = 0;
    int64_t end = 0;

    const RepairedFrame* before = nullptr;
    const RepairedFrame* after = nullptr;
    for (const RepairedFrame& frame : frames) {
      if (frame.seq < gap.first_seq) {
        before = &frame;
      } else if (frame.seq > gap.last_seq) {
        after = &frame;
        break;
      }
    }

    if (before && after) {
      // Interpolate across the hole using sequence distance, which also
      // handles frames that were never captured.
      const int64_t span_seq =
          static_cast<int64_t>(after->seq - before->seq);
      const int64_t offset_seq =
          static_cast<int64_t>(gap.first_seq - before->seq);
      const int64_t span_ts = after->ts_us - before->ts_us;
      start = before->ts_us + (span_seq > 0 ? span_ts * offset_seq / span_seq
                                            : nominal);
      end = before->ts_us +
            (span_seq > 0
                 ? span_ts * (static_cast<int64_t>(gap.last_seq - before->seq) + 1) /
                       span_seq
                 : nominal * count);
    } else if (before) {
      start = before->ts_us + nominal;
      end = start + nominal * count;
    } else if (after) {
      start = after->ts_us - nominal * count;
      end = after->ts_us;
    } else {
      start = 0;
      end = nominal * count;
    }
    if (end <= start) {
      end = start + nominal * count;
    }
    auto frames_lost = static_cast<uint32_t>(std::min<int64_t>(count, UINT32_MAX));
    out.push_back(LostRange{gap.first_seq, gap.last_seq, start, end, frames_lost});
  }
}

}  // namespace mfs
