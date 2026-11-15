#pragma once

#include "common.h"
#include "gop_index.h"
#include "param_sets.h"
#include "stats.h"

#include <string>
#include <vector>

namespace h264 {

struct AuditReport {
    std::string input_path;
    SpsInfo sps;
    PpsInfo pps;
    StatsSummary stats;
    std::vector<GopInfo> gops;
    std::vector<Warning> warnings;
};

void print_terminal(const AuditReport& report, bool show_summary, bool show_gops);
bool write_json(const std::string& path, const AuditReport& report, std::string& error);

}  // namespace h264
