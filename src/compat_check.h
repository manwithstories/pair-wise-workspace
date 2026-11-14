#pragma once

#include "init_parse.h"
#include "json.h"
#include "segment_index.h"

#include <string>
#include <vector>

namespace mp4 {

struct CompatCheck {
    std::string name;
    bool ok = false;
    std::string detail;
};

struct ValidationResult {
    bool init_compatible = false;
    bool segment_ok = false;
    std::string verdict;
    std::vector<CompatCheck> checks;
};

ValidationResult validate_init(const SegmentIndex& index, const InitInfo& candidate);
Json validation_to_json(const ValidationResult& result, const SegmentIndex& index);

}  // namespace mp4
