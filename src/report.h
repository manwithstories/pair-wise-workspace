#pragma once

#include "box_tree.h"
#include "fragment_index.h"
#include "validate.h"

#include <string>

namespace fmp4 {

std::string validationJson(const ValidationResult& result);
std::string validationText(const ValidationResult& result);
std::string probeJson(const InitSegment& init, const FragmentIndex& index);
std::string probeText(const InitSegment& init, const FragmentIndex& index);

}  // namespace fmp4

