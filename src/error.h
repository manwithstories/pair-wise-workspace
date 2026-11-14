#pragma once

#include <stdexcept>
#include <string>

namespace mp4 {

enum class ExitCode : int {
    Ok = 0,
    SegmentIssue = 2,
    InitIncompatible = 3,
    FormatError = 4,
    WriteError = 5,
};

class Error : public std::runtime_error {
public:
    Error(ExitCode code, const std::string& message)
        : std::runtime_error(message), code_(code) {}
    ExitCode code() const noexcept { return code_; }

private:
    ExitCode code_;
};

[[noreturn]] inline void fail(ExitCode code, const std::string& message) {
    throw Error(code, message);
}

}  // namespace mp4
