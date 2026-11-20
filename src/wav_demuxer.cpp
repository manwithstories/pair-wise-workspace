#include "mediacore/wav_demuxer.hpp"

#include <algorithm>
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
    sentinelBytes_ += 8; // 哨兵标记头（4 字节 tag + 4 字节参数）
    out = AudioBlock{};
    out.format = format_;
    out.startSample = currentSample_; // 快照哨兵所在边界（推进在交付点）
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
        sentinelBytes_ += skip; // 损坏填充字节也是带外内容
        // 损坏按帧换算占用采样刻度，推进时间线使跳过区间非空
        const SampleTick lostFrames =
            format_.frameBytes() > 0
                ? static_cast<SampleTick>(skip / format_.frameBytes())
                : 0;
        out.corruptFrames = lostFrames;
        // 仅快照帧数；currentSample_ 的推进统一在哨兵块交付点
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
        out.gapFrames = static_cast<SampleTick>(arg);
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
    sentinelBytes_ += static_cast<ByteOffset>(bytes); // 重复 PCM 带外
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
    const int frame = format_.frameBytes();

    // 哨兵块统一交付：按当前逻辑时间线打戳并推进，保证顺序与刻度一致
    auto emitSentinel = [&](AudioBlock sb) -> bool {
        sb.startSample = currentSample_;
        if (sb.kind == BlockKind::Corrupt) {
            currentSample_ += sb.corruptFrames;
        } else if (sb.kind == BlockKind::Gap) {
            currentSample_ += sb.gapFrames;
        }
        nextSample_ = currentSample_;
        stats_.blocksEmitted++;
        out = std::move(sb);
        return true;
    };

    // 上一次填充途中暂存的哨兵优先交付（位于任何新数据之前）
    if (hasPendingSentinel_) {
        AudioBlock sb = std::move(pendingSentinel_);
        hasPendingSentinel_ = false;
        return emitSentinel(std::move(sb));
    }

    // 以实际文件位置为权威计算 data 区剩余字节（哨兵字节不属于样本）
    auto dataRemaining = [&]() -> ByteOffset {
        if (dataLength_ <= 0) {
            return -1;
        }
        const ByteOffset pos = static_cast<ByteOffset>(file_.tellg());
        return dataStart_ + dataLength_ + sentinelBytes_ - pos;
    };

    // 帧边界物理探测哨兵：1=哨兵(已写入 out) -1=EOF/错误 0=普通 PCM
    auto probeSentinel = [&]() -> int {
        // 区内剩余不足 4 字节时不可能容纳哨兵标记，直接放行给整帧
        // 读取，避免短读在流上残留 failbit 破坏后续读取
        const ByteOffset avail = dataRemaining();
        if (avail >= 0 && avail < 4) {
            return 0;
        }
        char probe[4];
        const auto here = file_.tellg();
        file_.clear(); // 清除历史 EOF/failbit，保证本次读取与回退可靠
        file_.read(probe, 4);
        const auto got = file_.gcount();
        if (got == 0) {
            file_.clear(); // 清除 EOF/failbit，恢复 seekg 能力
            eof_ = true;
            return -1;
        }
        if (got < 4) {
            file_.clear();
            file_.seekg(here, std::ios::beg);
            if (dataRemaining() > 0) {
                return 0; // 区内末帧 PCM 的一部分
            }
            stats_.tailBytesDropped +=
                static_cast<std::uint64_t>(got);
            eof_ = true;
            stats_.recoverableErrors.push_back(
                {MediaErrorCode::IncompleteTail,
                 std::to_string(got) +
                     " trailing bytes do not form a complete frame",
                 true});
            return -1;
        }
        const std::uint32_t id = readU32(probe);
        if (id == tag("MCOR") || id == tag("MGAP") || id == tag("MDUP")) {
            file_.seekg(here, std::ios::beg);
            bool consumed = false;
            if (!readSentinel(out, consumed, err)) {
                return -1;
            }
            return 1;
        }
        file_.seekg(here, std::ios::beg);
        return 0;
    };

    // 入口哨兵：直接交付（时间线由 emitSentinel 推进）
    if (int r = probeSentinel(); r != 0) {
        if (r > 0) {
            AudioBlock sb = std::move(out);
            return emitSentinel(std::move(sb));
        }
        return false;
    }
    if (dataRemaining() == 0) {
        eof_ = true;
        return false;
    }

    // 普通 PCM 块：用逻辑帧目标数驱动，帧边界允许哨兵插入。
    // frameGoal 只由声明 data 长度与 blockFrames 决定，与探测回退无关，
    // 避免基于物理游标反复重算导致的末块少帧问题。
    SampleTick frameGoal = config_.blockFrames;
    const ByteOffset remain0 = dataRemaining();
    if (remain0 >= 0) {
        frameGoal = std::min<SampleTick>(
            frameGoal, remain0 / frame);
    }
    std::size_t cap =
        static_cast<std::size_t>(frameGoal) * frame;

    AudioBlock blk;
    blk.format = format_;
    const SampleTick blockStart = currentSample_; // 填充前快照
    blk.startSample = blockStart;
    blk.interleavedPcm.reserve(cap);

    char frameBuf[64];
    bool hitEof = false;
    SampleTick framesRead = 0;
    while (framesRead < frameGoal && !hitEof) {
        const int r = probeSentinel();
        if (r > 0) {
            hasPendingSentinel_ = true;
            pendingSentinel_ = std::move(out); // 哨兵留待下一次先交付
            break;
        }
        if (r < 0) {
            hitEof = true;
            break;
        }
        file_.clear();
        file_.read(frameBuf, frame);
        if (file_.gcount() != frame) {
            hitEof = true;
            break;
        }
        blk.interleavedPcm.insert(blk.interleavedPcm.end(), frameBuf,
                                  frameBuf + frame);
        ++framesRead;
    }
    // 块结束后：若已读到声明 data 边界，先基于物理文件长度检测
    // 采集端追加的悬空字节（在任何进一步读取之前，避免游标错位）
    const ByteOffset leftNow = dataRemaining();
    if (leftNow <= 0 && dataLength_ > 0 && !hasPendingSentinel_) {
        const ByteOffset declaredEnd =
            dataStart_ + dataLength_ + sentinelBytes_;
        file_.clear();
        file_.seekg(0, std::ios::end);
        const ByteOffset physicalEnd =
            static_cast<ByteOffset>(file_.tellg());
        const ByteOffset stray = physicalEnd - declaredEnd;
        if (stray > 0) {
            stats_.tailBytesDropped +=
                static_cast<std::uint64_t>(stray);
            stats_.recoverableErrors.push_back(
                {MediaErrorCode::IncompleteTail,
                 std::to_string(stray) +
                     " trailing bytes beyond declared data chunk",
                 true});
        }
        file_.seekg(physicalEnd, std::ios::beg);
        hitEof = true;
    } else if (!hitEof && !hasPendingSentinel_) {
        // 块结束后统一补探测：哨兵可能紧随块边界
        if (probeSentinel() > 0) {
            hasPendingSentinel_ = true;
            pendingSentinel_ = std::move(out);
        }
    }

    const std::size_t filled = blk.interleavedPcm.size();
    const std::size_t dropped =
        truncateToFrameBoundary(blk.interleavedPcm, format_);
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
        // 无样本：若暂存了哨兵则下一次交付，否则结束
        if (!hasPendingSentinel_) {
            eof_ = true;
        }
        return false;
    }

    // 普通块用填充前快照打戳（currentSample_ 已被本块推进，不能直接用）
    blk.startSample = blockStart;
    currentSample_ = blockStart + blk.frameCount();
    nextSample_ = currentSample_;
    stats_.blocksEmitted++;
    out = std::move(blk);

    // 尾部恢复：区外悬空字节已在上方处理；这里处理区内残余与 EOF
    const ByteOffset leftInData = dataRemaining();
    if (leftInData > 0 && leftInData < frame) {
        stats_.tailBytesDropped +=
            static_cast<std::uint64_t>(leftInData);
        stats_.recoverableErrors.push_back(
            {MediaErrorCode::IncompleteTail,
             std::to_string(leftInData) +
                 " sub-frame bytes inside data chunk dropped",
             true});
        file_.seekg(dataStart_ + dataLength_ + sentinelBytes_,
                    std::ios::beg);
        if (!hasPendingSentinel_) {
            eof_ = true;
        }
    } else if ((leftInData <= 0 || hitEof) && !hasPendingSentinel_) {
        eof_ = true;
    }
    return true;
}

} // namespace mediacore
