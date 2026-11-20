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
    // 标准 RIFF 块目录遍历
    std::streamoff end = 12 + riffSize_;
    char hdr[8];
    while (file_.tellg() < end) {
        const auto pos = file_.tellg();
        if (!readExact(hdr, 8, err)) {
            return false;
        }
        const std::uint32_t id = readU32(hdr);
        const std::uint32_t sz = readU32(hdr + 4);
        if (id == tag("data")) {
            dataStart_ = static_cast<ByteOffset>(pos) + 8;
            dataLength_ = static_cast<ByteOffset>(sz);
            return true;
        }
        if (!file_.seekg((sz + 1u) & ~1u, std::ios::cur)) {
            break;
        }
    }

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

    // 先遍历块目录定位 fmt 与 data
    bool haveFmt = false;
    std::streamoff end = 12 + riffSize_;
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
            format_.sampleRate =
                static_cast<int>(readU32(fmtBuf + 4));
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
            // RIFF 块按字对齐：跳过剩余 fmt 扩展字节及可能的 1 字节填充
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

bool WavDemuxer::readSentinel(AudioBlock& out, bool& consumed,
                              MediaError& err) {
    char body[8];
    if (!readExact(body, 8, err)) {
        return false;
    }
    const std::uint32_t id = readU32(body);
    const std::uint32_t arg = readU32(body + 4);
    cursor_ += 8;
    out = AudioBlock{};
    out.format = format_;
    out.startSample = nextSample_;
    consumed = true;

    if (id == tag("MCOR")) {
        const ByteOffset skip = static_cast<ByteOffset>(arg);
        std::vector<std::uint8_t> scratch(
            static_cast<std::size_t>(std::min<ByteOffset>(skip, 1 << 16)));
        ByteOffset left = skip;
        while (left > 0) {
            const auto n = static_cast<std::streamsize>(
                std::min<ByteOffset>(left, scratch.size()));
            file_.read(reinterpret_cast<char*>(scratch.data()), n);
            left -= file_.gcount();
            if (file_.gcount() != n) {
                break;
            }
        }
        cursor_ += skip;
        out.kind = BlockKind::Corrupt;
        auto e = MediaError{MediaErrorCode::CorruptStream,
                            "corrupt region of " + std::to_string(skip) +
                                " bytes at sample " +
                                std::to_string(nextSample_),
                            true};
        stats_.recoverableErrors.push_back(e);
        return true;
    }
    if (id == tag("MGAP")) {
        nextSample_ += static_cast<SampleTick>(arg);
        out.kind = BlockKind::Gap;
        stats_.recoverableErrors.push_back(
            {MediaErrorCode::TimestampGap,
             "gap of " + std::to_string(arg) + " frames at sample " +
                 std::to_string(out.startSample),
             true});
        return true;
    }
    // MDUP：随后的 PCM 与此前区间重叠，读出来但标记 Duplicate 由下游丢弃
    const SampleTick dupFrames = static_cast<SampleTick>(arg);
    const std::size_t bytes =
        static_cast<std::size_t>(dupFrames) * format_.frameBytes();
    out.interleavedPcm.resize(bytes);
    if (bytes > 0 && !readExact(
            reinterpret_cast<char*>(out.interleavedPcm.data()), bytes, err)) {
        return false;
    }
    cursor_ += static_cast<ByteOffset>(bytes);
    out.kind = BlockKind::Duplicate;
    stats_.recoverableErrors.push_back(
        {MediaErrorCode::DuplicateBlock,
         std::to_string(dupFrames) + " duplicated frames at sample " +
             std::to_string(nextSample_),
         true});
    return true;
}

bool WavDemuxer::nextBlock(AudioBlock& out, MediaError& err) {
    if (eof_) {
        return false;
    }
    // 哨兵优先：块边界处探测 4 字节标记
    char probe[4];
    file_.read(probe, 4);
    if (file_.gcount() == 0) {
        eof_ = true;
        return false;
    }
    if (file_.gcount() < 4) {
        // 连 4 字节都不足：属于不完整尾部
        stats_.tailBytesDropped += static_cast<std::uint64_t>(file_.gcount());
        eof_ = true;
        if (file_.gcount() > 0) {
            stats_.recoverableErrors.push_back(
                {MediaErrorCode::IncompleteTail,
                 std::to_string(file_.gcount()) +
                     " trailing bytes do not form a complete frame",
                 true});
        }
        return false;
    }
    const std::uint32_t id = readU32(probe);
    if (id == tag("MCOR") || id == tag("MGAP") || id == tag("MDUP")) {
        file_.seekg(-4, std::ios::cur);
        bool consumed = false;
        if (!readSentinel(out, consumed, err)) {
            return false;
        }
        stats_.blocksEmitted++;
        return true;
    }

    // 普通 PCM 块：先放入已读到的 4 字节，再补齐 blockFrames 帧
    const int frame = format_.frameBytes();
    std::size_t cap =
        static_cast<std::size_t>(config_.blockFrames) * frame;
    // 声明 dataLength 时不得越界读取
    if (dataLength_ > 0 && cursor_ + static_cast<ByteOffset>(cap) >
                               dataLength_) {
        const ByteOffset remain = dataLength_ - cursor_;
        cap = static_cast<std::size_t>(remain);
    }
    AudioBlock blk;
    blk.format = format_;
    blk.startSample = nextSample_;
    blk.interleavedPcm.resize(cap);
    std::memcpy(blk.interleavedPcm.data(), probe, 4);
    std::size_t filled = 4;
    bool hitEof = false;
    while (filled < cap) {
        file_.read(reinterpret_cast<char*>(blk.interleavedPcm.data()) + filled,
                   static_cast<std::streamsize>(cap - filled));
        const auto got = file_.gcount();
        filled += static_cast<std::size_t>(got);
        if (got == 0) {
            hitEof = true;
            break;
        }
    }
    blk.interleavedPcm.resize(filled);
    const std::size_t dropped = truncateToFrameBoundary(blk.interleavedPcm,
                                                         format_);
    if (dropped > 0) {
        stats_.tailBytesDropped += dropped;
        blk.kind = BlockKind::IncompleteTail;
        stats_.recoverableErrors.push_back(
            {MediaErrorCode::IncompleteTail,
             std::to_string(dropped) +
                 " trailing bytes truncated to frame boundary",
             true});
    }
    cursor_ += static_cast<ByteOffset>(filled);
    if (blk.frameCount() == 0) {
        eof_ = true;
        return false;
    }
    nextSample_ = blk.endSample();
    stats_.blocksEmitted++;
    out = std::move(blk);
    if (hitEof) {
        eof_ = true;
    }
    return true;
}

} // namespace mediacore
