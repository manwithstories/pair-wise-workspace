#pragma once

#include <cstdint>
#include <stdexcept>
#include <string>

namespace fmp4 {

enum class ExitCode : int {
    Ok = 0,
    ValidationFailed = 1,
    UsageOrIo = 2,
    ParseFailed = 3,
};

class Error : public std::runtime_error {
public:
    Error(ExitCode code, std::string message, uint64_t offset = 0)
        : std::runtime_error(std::move(message)), code_(code), offset_(offset) {}

    ExitCode code() const noexcept { return code_; }
    uint64_t offset() const noexcept { return offset_; }

private:
    ExitCode code_;
    uint64_t offset_;
};

inline Error parseError(const std::string& message, uint64_t offset) {
    return Error(ExitCode::ParseFailed,
                 message + " at byte offset " + std::to_string(offset), offset);
}

inline Error ioError(const std::string& message) {
    return Error(ExitCode::UsageOrIo, message);
}

}  // namespace fmp4

