#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <stdexcept>
#include <string>
#include <vector>

namespace h264 {

struct ScanWarning {
    std::uint64_t offset = 0;
    std::string message;
};

struct NalUnit {
    std::uint64_t offset = 0;
    std::uint8_t nal_ref_idc = 0;
    std::uint8_t nal_unit_type = 0;
    std::vector<std::uint8_t> rbsp;
};

struct ScanResult {
    std::uint64_t bytesRead = 0;
    std::size_t nalCount = 0;
    std::vector<ScanWarning> warnings;
};

class ScanError : public std::runtime_error {
public:
    ScanError(const std::string& message, std::uint64_t offset);
    std::uint64_t offset() const noexcept { return offset_; }

private:
    std::uint64_t offset_;
};

using NalCallback = std::function<void(NalUnit&&)>;
ScanResult scanAnnexB(const std::string& path, const NalCallback& callback);

bool isSupportedNalType(std::uint8_t type);

}  // namespace h264
