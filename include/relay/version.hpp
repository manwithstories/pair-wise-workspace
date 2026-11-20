#pragma once

namespace relay {

// Analysis format version. Checkpoints bind to this version; a mismatch
// refuses resume so stale state is never replayed against new logic.
inline constexpr unsigned kAnalysisVersion = 1;

const char* version_string();

}  // namespace relay
