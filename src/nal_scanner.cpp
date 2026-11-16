#include "nal_scanner.h"

#include <array>
#include <fstream>
#include <sstream>

namespace h264 {

namespace {

constexpr std::size_t kBufferSize = 1024 * 1024;
constexpr std::size_t kMaxNalSize = 32 * 1024 * 1024;

std::string atOffset(const std::string& message, std::uint64_t offset) {
    std::ostringstream out;
    out << message << " (byte offset " << offset << ")";
    return out.str();
}

}  // namespace

ScanError::ScanError(const std::string& message, std::uint64_t offset)
    : std::runtime_error(atOffset(message, offset)), offset_(offset) {}

bool isSupportedNalType(std::uint8_t type) {
    switch (type) {
        case 1:   // non-IDR slice
        case 5:   // IDR slice
        case 6:   // SEI
        case 7:   // SPS
        case 8:   // PPS
        case 9:   // AUD
        case 10:  // end of sequence
        case 11:  // end of stream
        case 12:  // filler data
            return true;
        default:
            return false;
    }
}

ScanResult scanAnnexB(const std::string& path, const NalCallback& callback) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        throw ScanError("cannot open input file", 0);
    }

    ScanResult result;
    std::vector<std::uint8_t> raw;
    raw.reserve(64 * 1024);
    std::array<char, kBufferSize> buffer{};
    std::uint64_t absolute = 0;
    std::uint64_t zeroRun = 0;
    std::uint64_t nalStart = 0;
    bool inNal = false;
    bool sawStartCode = false;

    auto finishNal = [&]() {
        std::size_t payloadSize = raw.size();
        while (payloadSize > 0 && raw[payloadSize - 1] == 0) {
            --payloadSize;
        }
        if (payloadSize == 0) {
            result.warnings.push_back({nalStart, "residual NAL without header"});
            raw.clear();
            return;
        }
        const std::uint8_t header = raw[0];
        const std::uint8_t type = header & 0x1f;
        if (payloadSize == 1) {
            result.warnings.push_back({nalStart, "residual NAL without RBSP payload"});
            raw.clear();
            return;
        }

        std::size_t write = 0;
        unsigned zeroCount = 0;
        for (std::size_t read = 1; read < payloadSize; ++read) {
            const std::uint8_t byte = raw[read];
            if (zeroCount >= 2 && byte == 0x03) {
                zeroCount = 0;
                continue;
            }
            raw[write++] = byte;
            if (byte == 0) {
                ++zeroCount;
            } else {
                zeroCount = 0;
            }
        }
        raw.resize(write);
        NalUnit nal;
        nal.offset = nalStart;
        nal.nal_ref_idc = (header >> 5) & 0x03;
        nal.nal_unit_type = type;
        nal.rbsp = std::move(raw);
        ++result.nalCount;
        callback(std::move(nal));
        raw.clear();
        raw.reserve(64 * 1024);
    };

    while (input) {
        input.read(buffer.data(), static_cast<std::streamsize>(buffer.size()));
        const std::streamsize got = input.gcount();
        for (std::streamsize i = 0; i < got; ++i) {
            const std::uint8_t byte = static_cast<std::uint8_t>(buffer[static_cast<std::size_t>(i)]);
            if (!inNal) {
                if (byte == 0) {
                    ++zeroRun;
                } else if (byte == 1 && zeroRun >= 2) {
                    nalStart = absolute - (zeroRun >= 3 ? 3 : 2);
                    inNal = true;
                    sawStartCode = true;
                    zeroRun = 0;
                    raw.clear();
                } else {
                    zeroRun = 0;
                }
            } else if (byte == 1 && raw.size() >= 2 &&
                       raw[raw.size() - 1] == 0 && raw[raw.size() - 2] == 0) {
                const std::size_t prefix = raw.size() >= 3 && raw[raw.size() - 3] == 0 ? 3 : 2;
                const std::uint64_t nextStart = absolute - prefix;
                raw.resize(raw.size() - prefix);
                finishNal();
                nalStart = nextStart;
                inNal = true;
            } else {
                if (raw.size() >= kMaxNalSize) {
                    throw ScanError("NAL unit exceeds 32 MiB parser limit", nalStart);
                }
                raw.push_back(byte);
            }
            ++absolute;
        }
        if (input.bad()) {
            throw ScanError("input read failed", absolute);
        }
    }

    if (inNal) {
        finishNal();
    }
    result.bytesRead = absolute;
    if (!sawStartCode) {
        throw ScanError("no Annex B start code", 0);
    }
    if (result.nalCount == 0) {
        throw ScanError("no complete NAL unit", 0);
    }
    return result;
}

}  // namespace h264
