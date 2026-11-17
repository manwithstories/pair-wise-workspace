// SPDX-License-Identifier: MIT
#include "timecode/audit.hpp"

#include <algorithm>
#include <fmt/format.h>

#include "timecode/converter.hpp"
#include "timecode/dropframe.hpp"
#include "timecode/parser.hpp"
#include "timecode/rate_table.hpp"

namespace tc {

namespace {

// Day frames of the rate as a positive magnitude (all rates are positive).
std::int64_t day_span(Rate rate) {
    return rate_info(rate).frames_per_day;
}

void push_event(std::vector<AuditEvent>& events, std::size_t line, Diag d,
                std::string msg, const std::string& raw) {
    events.push_back(AuditEvent{line, d, std::move(msg), raw});
}

}  // namespace

AuditOutput audit_sequence(const std::vector<AuditInput>& lines, Rate rate,
                           WrapPolicy policy) {
    AuditOutput out;
    AuditSummary& sum = out.summary;
    sum.rate = rate;
    sum.policy = policy;
    sum.records = static_cast<std::int64_t>(lines.size());

    std::vector<Record> valid;
    std::vector<Record> invalid_records;
    std::int64_t prev_frame = 0;
    std::int64_t expected_frame = 0;  // ideal continuous position
    std::int64_t day_base = 0;        // accumulated day-offset from wraps
    bool have_prev = false;
    const std::int64_t span = day_span(rate);

    for (std::size_t i = 0; i < lines.size(); ++i) {
        const std::size_t line_no = i + 1;
        const std::string& text = lines[i].line;
        ParseResult pr = parse_timecode(text, rate);

        Record rec;
        rec.line = line_no;
        rec.raw = text;
        rec.real_time_seconds = Rational(0, 1);

        if (!pr.ok) {
            rec.valid = false;
            rec.diag = pr.diag;
            rec.message = pr.message;
            ++sum.invalid;
            push_event(out.events, line_no, pr.diag, pr.message, text);
            invalid_records.push_back(std::move(rec));
            continue;  // parse failures never break later-record processing
        }

        rec.valid = true;
        rec.tc = pr.tc;
        rec.source_frame = to_frame_number(pr.tc, rate);
        rec.drop_compensation = drop_compensation_at(pr.tc, rate);
        sum.total_drop_compensation += rec.drop_compensation;

        // Resolve a backward jump against the previous valid frame. A proven
        // wrap raises the persistent day base so every later record stays on
        // the new day instead of re-triggering a wrap on each line.
        std::int64_t unwrapped = rec.source_frame + day_base;
        if (have_prev && unwrapped < prev_frame) {
            const std::int64_t backward = prev_frame - unwrapped;
            // Provable wrap: the jump back crosses roughly one full labeled
            // day (tolerance: at least half a day span).
            const bool provable = backward >= span / 2;
            if (policy == WrapPolicy::Strict) {
                rec.diag = Diag::OutOfOrder;
                rec.message = "backward timecode under strict policy";
                push_event(out.events, line_no, Diag::OutOfOrder, rec.message, text);
                ++sum.out_of_order;
            } else if (policy == WrapPolicy::Wrap ||
                       (policy == WrapPolicy::Repair && provable)) {
                day_base += span;
                unwrapped += span;
                ++sum.wraps;
            } else {  // repair but not provable
                rec.diag = Diag::UnprovenWrap;
                rec.message = "backward jump is not a provable day wrap";
                push_event(out.events, line_no, Diag::UnprovenWrap, rec.message, text);
                ++sum.out_of_order;
            }
        }

        // Explicit per-line stride wins; frame-dense (1) otherwise. Per-second
        // sampling carries step=30 so legal sample spacing is not a "gap".
        const std::int64_t step = lines[i].step_frames > 0 ? lines[i].step_frames : 1;

        if (have_prev) {
            const std::int64_t delta = unwrapped - prev_frame;
            if (delta == 0) {
                rec.diag = Diag::DuplicateFrame;
                rec.message = "duplicate frame number";
                push_event(out.events, line_no, Diag::DuplicateFrame, rec.message, text);
                ++sum.duplicates;
            } else if (delta < 0) {
                // Already diagnosed above as out-of-order; keep it visible.
                if (rec.diag == Diag::Ok) {
                    rec.diag = Diag::OutOfOrder;
                    rec.message = "out-of-order frame";
                    ++sum.out_of_order;
                }
            } else {
                // Explicit per-line stride wins; frame-dense (1) otherwise.
                // Per-second sampling carries step=30 so legal sample spacing
                // is not mistaken for a gap.
                if (delta > step) {
                    const std::int64_t missing = delta - step;
                    sum.missing += missing;
                    ++sum.gap_events;
                    if (missing == 1) {
                        ++sum.single_gaps;
                    } else {
                        ++sum.run_gaps;
                    }
                    push_event(out.events, line_no, Diag::MissingFrame,
                               fmt::format("gap of {} frame(s) before this record", missing),
                               text);
                }
            }
        }

        // Ideal continuous frame: anchored at the first valid sample, then
        // advanced by the declared stride. A missing frame makes the real
        // position run ahead of ideal; a duplicate repeats the same position
        // and does not advance the ideal. Drift is real-minus-ideal in frames.
        if (!have_prev) {
            expected_frame = unwrapped;
        } else if (rec.diag != Diag::DuplicateFrame) {
            expected_frame += step;
        }
        rec.ideal_frame = expected_frame;
        rec.cumulative_drift = unwrapped - rec.ideal_frame;
        const std::int64_t abs_drift =
            rec.cumulative_drift < 0 ? -rec.cumulative_drift : rec.cumulative_drift;
        sum.max_abs_drift = std::max(sum.max_abs_drift, abs_drift);

        // Exact real elapsed time on the unwrapped timeline.
        const RateInfo info = rate_info(rate);
        rec.real_time_seconds =
            Rational(unwrapped * info.frames_per_second.den, info.frames_per_second.num);

        prev_frame = unwrapped;
        have_prev = true;
        ++sum.valid;
        valid.push_back(std::move(rec));
    }

    out.records = stable_sorted_records(std::move(valid));
    for (auto& bad : invalid_records) out.records.push_back(std::move(bad));
    return out;
}

std::vector<Record> stable_sorted_records(std::vector<Record> records) {
    // Valid records first, ordered by frame with stable tie handling; parse
    // failures (source_frame == 0 default) are appended by the caller, so here
    // we only stable-sort the valid slice the auditor hands us.
    std::stable_sort(records.begin(), records.end(),
                     [](const Record& a, const Record& b) {
                         if (a.source_frame != b.source_frame)
                             return a.source_frame < b.source_frame;
                         return a.line < b.line;
                     });
    return records;
}

}  // namespace tc
