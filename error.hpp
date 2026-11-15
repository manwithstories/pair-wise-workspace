#pragma once

#include <cstdint>
#include <stdexcept>
#include <string>

namespace bake {

enum class ExitCode : int {
    ok = 0,
    mismatch = 1,
    invalid_input = 2,
    write_failed = 3,
    bad_arguments = 4,
    invariant = 5,
};

class BakeError : public std::runtime_error {
public:
    ExitCode code;
    int64_t frame;
    int row;
    int column;

    explicit BakeError(ExitCode code_value, const std::string& message,
                       int64_t frame_value = -1, int row_value = -1,
                       int column_value = -1)
        : std::runtime_error(message), code(code_value), frame(frame_value),
          row(row_value), column(column_value) {}
};

inline std::string locationSuffix(int64_t frame, int row = -1, int column = -1) {
    std::string result;
    if (frame >= 0) {
        result += " frame=" + std::to_string(frame);
    }
    if (row >= 0) {
        result += " row=" + std::to_string(row);
    }
    if (column >= 0) {
        result += " col=" + std::to_string(column);
    }
    return result;
}

inline int64_t checkedAdd(int64_t a, int64_t b, const std::string& what) {
    if ((b > 0 && a > INT64_MAX - b) || (b < 0 && a < INT64_MIN - b)) {
        throw BakeError(ExitCode::invalid_input, what + ": int64 overflow");
    }
    return a + b;
}

inline int64_t checkedMul(int64_t a, int64_t b, const std::string& what) {
    if (a == 0 || b == 0) {
        return 0;
    }
    if (a == -1 && b == INT64_MIN) {
        throw BakeError(ExitCode::invalid_input, what + ": int64 overflow");
    }
    if (b == -1 && a == INT64_MIN) {
        throw BakeError(ExitCode::invalid_input, what + ": int64 overflow");
    }
    const int64_t result = a * b;
    if (result / b != a) {
        throw BakeError(ExitCode::invalid_input, what + ": int64 overflow");
    }
    return result;
}

}  // namespace bake
