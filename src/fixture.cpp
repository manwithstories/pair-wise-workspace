#include "mediacore/fixture.hpp"

#include <cmath>
#include <cstring>
#include <fstream>

#include "mediacore/path_guard.hpp"

namespace mediacore::fixture {

namespace {
constexpr std::uint32_t tag(const char (&s)[5]) {
    return static_cast<std::uint32_t>(s[0]) |
           (static_cast<std::uint32_t>(s[1]) << 8) |
           (static_cast<std::uint32_t>(s[2]) << 16) |
           (static_cast<std::uint32_t>(s[3]) << 24);
}
void putU32(std::vector<std::uint8_t>& v, std::uint32_t x) {
    v.push_back(static_cast<std::uint8_t>(x));
    v.push_back(static_cast<std::uint8_t>(x >> 8));
    v.push_back(static_cast<std::uint8_t>(x >> 16));
    v.push_back(static_cast<std::uint8_t>(x >> 24));
}
void putU16(std::vector<std::uint8_t>& v, std::uint16_t x) {
    v.push_back(static_cast<std::uint8_t>(x));
    v.push_back(static_cast<std::uint8_t>(x >> 8));
}
} // namespace

std::int16_t sampleAt(SampleTick frame, int channel, int channels) {
    if ((frame % 256) == 0) {
        return (channel % 2 == 0) ? 32767 : -32767;
    }
    const double phase = 0.05 * static_cast<double>(frame + channel);
    const double amp = 0.5 + 0.4 * std::sin(static_cast<double>(frame) /
                                             (64.0 * (channel + 1)));
    double v = amp * std::sin(phase);
    if (channels > 1) {
        v *= (channel % 2 == 0 ? 1.0 : -1.0); // 声道间反相，便于校验映射
    }
    auto s = static_cast<std::int32_t>(v * 30000.0);
    if (s > 32767) s = 32767;
    if (s < -32768) s = -32768;
    return static_cast<std::int16_t>(s);
}

std::vector<std::uint8_t> makePcm(const PcmFormat& format,
                                  SampleTick nFrames) {
    std::vector<std::uint8_t> out;
    out.reserve(static_cast<std::size_t>(nFrames) * format.frameBytes());
    for (SampleTick f = 0; f < nFrames; ++f) {
        for (int ch = 0; ch < format.channels; ++ch) {
            putU16(out, static_cast<std::uint16_t>(
                            sampleAt(f, ch, format.channels)));
        }
    }
    return out;
}

void WavBuilder::writeU32(std::uint32_t v) { putU32(bytes, v); }
void WavBuilder::writeU16(std::uint16_t v) { putU16(bytes, v); }
void WavBuilder::writeBytes(const std::vector<std::uint8_t>& data) {
    bytes.insert(bytes.end(), data.begin(), data.end());
    dataPayload += static_cast<std::uint32_t>(data.size());
}
void WavBuilder::writeRaw(const void* p, std::size_t n) {
    const auto* bp = static_cast<const std::uint8_t*>(p);
    bytes.insert(bytes.end(), bp, bp + n);
}

void WavBuilder::writeHeader(std::uint32_t dataSize) {
    dataPayload = 0; // 头部之后开始累计 data 载荷
    writeU32(tag("RIFF"));
    writeU32(36u + dataSize);
    writeU32(tag("WAVE"));
    writeU32(tag("fmt "));
    writeU32(16u);
    writeU16(1u); // PCM
    writeU16(static_cast<std::uint16_t>(format.channels));
    writeU32(static_cast<std::uint32_t>(format.sampleRate));
    const std::uint32_t byteRate = static_cast<std::uint32_t>(
        format.sampleRate * format.frameBytes());
    writeU32(byteRate);
    writeU16(static_cast<std::uint16_t>(format.frameBytes()));
    writeU16(static_cast<std::uint16_t>(format.bitsPerSample));
    writeU32(tag("data"));
    writeU32(dataSize);
}

void WavBuilder::writeCorruptSentinel(std::uint32_t corruptBytes) {
    writeU32(tag("MCOR"));
    writeU32(corruptBytes);
    bytes.insert(bytes.end(), corruptBytes, 0xAA);
}
void WavBuilder::writeGapSentinel(std::uint32_t gapFrames) {
    writeU32(tag("MGAP"));
    writeU32(gapFrames);
}
void WavBuilder::writeDuplicateSentinel(
    std::uint32_t dupFrames, const std::vector<std::uint8_t>& dupPcm) {
    writeU32(tag("MDUP"));
    writeU32(dupFrames);
    // 哨兵载荷属于带外标记，不计入 data 载荷长度
    bytes.insert(bytes.end(), dupPcm.begin(), dupPcm.end());
}

void WavBuilder::finish() {
    // data 长度只统计完整帧载荷（dataPayload 由 writeBytes/帧写入累计），
    // 悬空字节留在 data 区之外，由解复用器作为不完整尾部恢复。
    const std::uint32_t frameBytes =
        static_cast<std::uint32_t>(format.frameBytes());
    const std::uint32_t dataSize =
        frameBytes > 0 ? dataPayload - (dataPayload % frameBytes)
                       : dataPayload;
    const std::uint32_t riffSize = 36u + dataSize;
    std::memcpy(bytes.data() + 4, &riffSize, 4);
    std::memcpy(bytes.data() + 40, &dataSize, 4);
}

bool writeFile(const std::string& path,
               const std::vector<std::uint8_t>& bytes) {
    std::ofstream f(path, std::ios::binary | std::ios::trunc);
    if (!f.is_open()) {
        return false;
    }
    f.write(reinterpret_cast<const char*>(bytes.data()),
            static_cast<std::streamsize>(bytes.size()));
    return static_cast<bool>(f);
}

namespace {
const PcmFormat kFmt{16000, 1, 16, SampleFormat::S16Le};

void appendFrames(WavBuilder& b, SampleTick from, SampleTick count) {
    for (SampleTick f = from; f < from + count; ++f) {
        b.writeU16(static_cast<std::uint16_t>(sampleAt(f, 0, 1)));
        b.dataPayload += 2; // 每帧每声道 2 字节 s16
    }
}
} // namespace

FixtureSet makeAcceptanceFixtures(const std::string& dir) {
    FixtureSet set;
    WavBuilder clean;
    clean.format = kFmt;
    clean.writeHeader(0);
    appendFrames(clean, 0, 2048);
    clean.finish();
    set.cleanWav = dir + "/clean.wav";
    writeFile(set.cleanWav, clean.bytes);

    // 尾部截断：2048 完整帧 + 3 个悬空字节
    WavBuilder trunc;
    trunc.format = kFmt;
    trunc.writeHeader(0);
    appendFrames(trunc, 0, 2048);
    trunc.bytes.insert(trunc.bytes.end(), {0x01, 0x02, 0x03});
    trunc.finish();
    set.truncatedTail = dir + "/truncated_tail.wav";
    writeFile(set.truncatedTail, trunc.bytes);

    // 中段损坏 + 缺口 + 重复，全部可恢复，载荷总尺寸仍为 2048*2
    WavBuilder damaged;
    damaged.format = kFmt;
    damaged.writeHeader(0);
    appendFrames(damaged, 0, 512);
    damaged.writeCorruptSentinel(128);                 // 64 帧损坏
    appendFrames(damaged, 512, 640);                   // 512..1151
    damaged.writeGapSentinel(64);                      // 64 帧缺口
    appendFrames(damaged, 1216, 640);                  // 1216..1855
    auto dupPcm = makePcm(kFmt, 64);
    damaged.writeDuplicateSentinel(64, dupPcm);       // 64 帧重复
    damaged.finish();
    set.midCorrupt = dir + "/mid_corrupt.wav";
    writeFile(set.midCorrupt, damaged.bytes);

    // 截断头部：仅 8 字节，无法识别 RIFF/WAVE
    std::vector<std::uint8_t> bad(8, 0);
    std::memcpy(bad.data(), "RIFF", 4);
    set.badHeader = dir + "/bad_header.wav";
    writeFile(set.badHeader, bad);
    return set;
}

} // namespace mediacore::fixture
