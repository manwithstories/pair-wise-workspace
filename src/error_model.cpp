#include "error_model.hpp"

namespace ma {

const char* toString(ErrorCategory c) {
    switch (c) {
    case ErrorCategory::DecodeFailed:  return "decode_failed";
    case ErrorCategory::Truncated:     return "truncated";
    case ErrorCategory::CorruptPacket: return "corrupt_packet";
    case ErrorCategory::Timestamp:     return "timestamp";
    case ErrorCategory::None:          return "none";
    }
    return "unknown";
}

} // namespace ma
