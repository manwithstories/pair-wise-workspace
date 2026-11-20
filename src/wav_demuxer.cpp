#include "mediacore/wav_demuxer.hpp"

#include <array>
#include <cstring>
#include <limits>

namespace mediacore {

namespace {

std::uint32_t readU32(const char* p) {
    std::uint32_t v;
    std::memcpy(&v, p, 4);
    return v; // 宿主均为 little-endian（macOS arm64/x86_64）
}
std::uint16_t readU16(const char* p) {
    std::uint16_t v;
    std::memcpy(&v, p, 2);
    return v;
}
constexpr std::uint32_t tag(const char (&s)[5]) {
    return static_cast<std::uint32_t>(s[0]) |
           (static_cast<std::uint32_t>(s[1]) << 8) |
           (static_cast<std::uint32_t>(s[2]) << 16) |
           (static_cast<std::uint32_t>(s[3]) << 24);
}

} // namespace

WavDemuxer::~WavDemuxer() { close(); }

void WavDemuxer::close() {
    if (file_.is_open()) {
        file_.close();
    }
    eof_ = true;
}

Result<void> WavDemuxer::open(const std::string& path,
                              const DemuxConfig& config) {
    path_ = path;
    config_ = config;
    stats_ = DemuxStats{};
    file_.open(path, std::ios::binary);
    if (!file_.is_open()) {
        return MediaError{MediaErrorCode::InvalidArgument,
                          "cannot open input: " + path, false};
    }
    MediaError err;
    if (!parseHeader(err)) {
        close();
        return err;
    }
    totalFrames_ = format_.framesInBytes(dataLength_);
    return Result<void>{};
}

bool WavDemuxer::readExact(char* dst, std::size_t n, MediaError& err) {
    file_.read(dst, static_cast<std::streamsize>(n));
    const auto got = file_.gcount();
    if (static_cast<std::size_t>(got) != n) {
        err = {MediaErrorCode::TruncatedHeader,
               "unexpected EOF while reading header of " + path_, false};
        return false;
    }
    return true;
}

bool WavDemuxer::locateDataChunk(MediaError& err) {
    // 索引缺失/不完整：顺序扫描重建（受 scanLimitBytes 约束）
    stats_.indexRebuilt = true;
    stats_.recoverableErrors.push_back(
        {MediaErrorCode::MissingIndex,
         "RIFF chunk index missing/incomplete, sequential scan fallback",
         true});
    file_.clear();
    file_.seekg(12, std::ios::beg);
    std::array<char, 4> probe{};
    std::int64_t scanned = 0;
    while (scanned + 4 <= config_.scanLimitBytes) {
        const auto here = file_.tellg();
        file_.read(probe.data(), 4);
        if (file_.gcount() != 4) {
            break; // 文件结束仍未找到
        }
        scanned += 4;
        if (readU32(probe.data()) == tag("data")) {
            char lenBuf[4];
            if (!readExact(lenBuf, 4, err)) {
                return false;
            }
            scanned += 4;
            dataStart_ = static_cast<ByteOffset>(here) + 8;
            dataLength_ = static_cast<ByteOffset>(readU32(lenBuf));
            stats_.bytesScanned = static_cast<std::uint64_t>(scanned);
            return true;
        }
        file_.seekg(-3, std::ios::cur); // 重叠 3 字节继续扫描
    }
    err = {MediaErrorCode::ScanLimitExceeded,
           "sequential index scan exceeded limit (" +
               std::to_string(config_.scanLimitBytes) + " bytes) for " + path_,
           false};
    return false;
}

bool WavDemuxer::parseHeader(MediaError& err) {
    char riff[12];
    if (!readExact(riff, 12, err)) {
        return false;
    }
    if (readU32(riff) != tag("RIFF") || readU32(riff + 8) != tag("WAVE")) {
        err = {MediaErrorCode::TruncatedHeader,
               "not a RIFF/WAVE container: " + path_, false};
        return false;
    }
    riffSize_ = readU32(riff + 4);

    bool haveFmt = false;
    const std::streamoff end =
        12 + static_cast<std::streamoff>(riffSize_);
    char hdr[8];
    while (file_.tellg() < end) {
        const auto pos = file_.tellg();
        if (!readExact(hdr, 8, err)) {
            return false;
        }
        const std::uint32_t id = readU32(hdr);
        const std::uint32_t sz = readU32(hdr + 4);
        if (id == tag("fmt ")) {
            if (sz < 16) {
                err = {MediaErrorCode::UnsupportedFormat,
                       "fmt chunk too small: " + path_, false};
                return false;
            }
            char fmtBuf[16];
            if (!readExact(fmtBuf, 16, err)) {
                return false;
            }
            const std::uint16_t audioFmt = readU16(fmtBuf);
            if (audioFmt != 1) {
                err = {MediaErrorCode::UnsupportedFormat,
                       "only uncompressed PCM supported, format tag=" +
                           std::to_string(audioFmt),
                       false};
                return false;
            }
            format_.channels = readU16(fmtBuf + 2);
            format_.sampleRate = static_cast<int>(readU32(fmtBuf + 4));
            format_.bitsPerSample = readU16(fmtBuf + 14);
            if (format_.bitsPerSample != 16) {
                err = {MediaErrorCode::UnsupportedFormat,
                       "only 16-bit PCM supported, got " +
                           std::to_string(format_.bitsPerSample),
                       false};
                return false;
            }
            if (!format_.valid()) {
                err = {MediaErrorCode::UnsupportedFormat,
                       "invalid PCM format in " + path_, false};
                return false;
            }
            haveFmt = true;
            const std::uint32_t rest = sz > 16 ? sz - 16 : 0;
            const std::streamoff skip =
                static_cast<std::streamoff>((rest + 1u) & ~1u);
            if (!file_.seekg(skip, std::ios::cur)) {
                err = {MediaErrorCode::TruncatedHeader,
                       "cannot skip fmt extension: " + path_, false};
                return false;
            }
        } else if (id == tag("data")) {
            dataStart_ = static_cast<ByteOffset>(pos) + 8;
            dataLength_ = static_cast<ByteOffset>(sz);
            if (!haveFmt) {
                err = {MediaErrorCode::TruncatedHeader,
                       "data chunk precedes fmt chunk: " + path_, false};
                return false;
            }
            break;
        } else {
            if (!file_.seekg((sz + 1u) & ~1u, std::ios::cur)) {
                err = {MediaErrorCode::TruncatedHeader,
                       "cannot skip chunk: " + path_, false};
                return false;
            }
        }
    }

    if (!haveFmt) {
        err = {MediaErrorCode::TruncatedHeader,
               "fmt chunk missing: " + path_, false};
        return false;
    }
    if (dataStart_ == 0) {
        if (!locateDataChunk(err)) {
            return false; // 扫描超限/未找到 -> 可诊断不可恢复错误
        }
    }
    file_.seekg(dataStart_, std::ios::beg);
    return true;
}

// 块级读取（哨兵/尾部截断）在下一批实现
bool WavDemuxer::nextBlock(AudioBlock&, MediaError&) { return false; }

} // namespace mediacore
