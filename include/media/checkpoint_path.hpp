// Internal checkpoint path safety helper shared by the implementation TUs.
#pragma once

#include <string>

namespace media {

// Reject path traversal and NUL bytes; only plain directory paths pass.
bool checkpoint_path_safe(const std::string& p);

} // namespace media
