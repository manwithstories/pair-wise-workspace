// SPDX-License-Identifier: MIT
// timecode/jsonl.hpp — stable JSONL serialization for records and summaries.
#pragma once

#include <nlohmann/json.hpp>

#include <string>

#include "timecode/audit.hpp"
#include "timecode/parser.hpp"
#include "timecode/rate_table.hpp"

namespace tc {

using Json = nlohmann::ordered_json;

inline Json rational_json(const Rational& r) {
    return Json{{"num", r.num}, {"den", r.den}};
}

inline Json record_json(const Record& rec, Rate rate) {
    Json j;
    j["line"] = rec.valid ? rec.line : rec.line;
    j["raw"] = rec.raw;
    j["valid"] = rec.valid;
    if (!rec.valid) {
        j["error"] = diag_name(rec.diag);
        j["message"] = rec.message;
        return j;
    }
    j["timecode"] = format_timecode(rec.tc, rate);
    j["format"] = rate_info(rate).canonical;
    j["day_offset"] = rec.tc.day;
    j["source_frame"] = rec.source_frame;
    j["ideal_frame"] = rec.ideal_frame;
    j["drop_compensation"] = rec.drop_compensation;
    j["cumulative_drift_frames"] = rec.cumulative_drift;
    j["real_time_seconds"] = rational_json(rec.real_time_seconds);
    if (rec.diag != Diag::Ok) {
        j["issue"] = diag_name(rec.diag);
        j["message"] = rec.message;
    }
    return j;
}

inline Json summary_json(const AuditSummary& s) {
    Json j;
    j["records"] = s.records;
    j["valid"] = s.valid;
    j["invalid"] = s.invalid;
    j["wraps"] = s.wraps;
    j["duplicates"] = s.duplicates;
    j["missing_frames"] = s.missing;
    j["gap_events"] = s.gap_events;
    j["single_gaps"] = s.single_gaps;
    j["run_gaps"] = s.run_gaps;
    j["out_of_order"] = s.out_of_order;
    j["drop_compensation_total"] = s.total_drop_compensation;
    j["max_abs_drift"] = s.max_abs_drift;
    return j;
}

}  // namespace tc
