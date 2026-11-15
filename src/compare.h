#pragma once

#include <string>

namespace fmp4 {

struct CompareOutput {
    std::string json;
    bool passed = false;
};

CompareOutput compareValidationJson(const std::string& before_json,
                                    const std::string& after_json);

}  // namespace fmp4

