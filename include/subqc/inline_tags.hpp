#pragma once

#include <string>

#include "subqc/model.hpp"

namespace subqc {

InlineAnalysis analyze_inline_tags(const std::string &text, const std::string &base_style,
                                   const StyleCatalog &catalog, const std::string &file, int line);

}  // namespace subqc
