// SPDX-License-Identifier: MIT
// timecode/audit.hpp — sequence-level timecode auditing.
//
// The auditor never edits parse errors away. It walks records in input order,
// tracking frame continuity; output is then stably sorted for reporting.
// `repair` may only re-base provable day wraps and fill nothing else.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "timecode/types.hpp"

namespace tc {

struct AuditSummary {
    std::int64_t records = 0;
    std::int64_t valid = 0;
    std::int64_t invalid = 0;
    std::int64_t wraps = 0;
    std::int64_t duplicates = 0;
    std::int64_t missing = 0;          // missing frames across all gaps
    std::int64_t gap_events = 0;       // distinct gap events (single + runs)
    std::int64_t single_gaps = 0;      // one-frame gaps
    std::int64_t run_gaps = 0;         // longer consecutive gaps
    std::int64_t out_of_order = 0;
    std::int64_t total_drop_compensation = 0;
    std::int64_t max_abs_drift = 0;
    Rate rate = Rate::F2997_DF;
    WrapPolicy policy = WrapPolicy::Strict;
    bool clean() const {
        return invalid == 0 && duplicates == 0 && missing == 0 &&
               out_of_order == 0;
    }
};

struct AuditOutput {
    std::vector<Record> records;
    AuditSummary summary;
    std::vector<AuditEvent> events;
};

// One raw input line plus the rate it must parse against. `step_frames` is
// the expected frame stride between adjacent valid samples (0 = infer from
// the first positive delta, i.e. frame-dense input by default).
struct AuditInput {
    std::string line;
    std::int64_t step_frames = 0;
};

AuditOutput audit_sequence(const std::vector<AuditInput>& lines, Rate rate,
                           WrapPolicy policy = WrapPolicy::Strict);

// Stable output ordering: valid records by unwrapped frame, parse failures
// last (preserving their original line order among themselves).
std::vector<Record> stable_sorted_records(std::vector<Record> records);

}  // namespace tc
