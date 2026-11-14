#pragma once

#include <string>
#include <vector>

#include "nlohmann/json.hpp"
#include "subqc/model.hpp"

namespace subqc {

nlohmann::json build_report(const std::vector<AnalyzedFile> &files, const RenderOptions &options,
                            long long elapsed_ms, const std::vector<Diagnostic> &extra_diagnostics);
std::string text_report(const nlohmann::json &report);
int report_exit_code(const nlohmann::json &report);

}  // namespace subqc
