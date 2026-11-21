#include "media/wav_reader.hpp"

#include <cstring>
#include <fstream>

#include "media/file_util.hpp"

namespace media {
namespace {

uint16_t le16(const unsigned char* p) {
  return static_cast<uint16_t>(p[0] | (p[1] << 8));
}
uint32_t le32(const unsigned char* p) {
  return static_cast<uint32_t>(p[0]) | (p[1] << 8) |
         (static_cast<uint32_t>(p[2]) << 16) |
         (static_cast<uint32_t>(p[3]) << 24);
}
uint64_t le64(const unsigned char* p) {
  uint64_t v = 0;
  for (int i = 7; i >= 0; --i) v = (v << 8) | p[i];
  return v;
}

}  // namespace

WavReader::WavReader(std::string path, CorruptionPolicy policy,
                     size_t blockFrames)
    : path_(std::move(path)), policy_(policy), blockFrames_(blockFrames) {}

MediaError WavReader::makeError(ErrorCode code, int64_t byteOff,
                                int64_t sample, const std::string& msg,
                                bool recoverable) const {
  return MediaError{code, Stage::Read, path_, byteOff, sample, msg,
                    recoverable};
}

bool WavReader::handleCorruption(MediaError* err,
                                 const MediaError& candidate) {
  recoverable_.push_back(candidate);
  if (policy_ == CorruptionPolicy::Abort) {
    *err = candidate;
    err->recoverable = false;
    return false;
  }
  return true;
}

bool WavReader::readExact(void* dst, size_t n, MediaError* err) {
  in_.read(static_cast<char*>(dst), static_cast<std::streamsize>(n));
  if (static_cast<size_t>(in_.gcount()) != n) {
    if (err)
      *err = makeError(ErrorCode::TruncatedChunk, -1, frameCursor_,
                       "unexpected end of file", true);
    return false;
  }
  return true;
}

void WavReader::mapEncoding() {
  auto& f = summary_.format;
  switch (f.bitsPerSample) {
    case 8: f.encoding = SampleEncoding::U8; break;
    case 16: f.encoding = SampleEncoding::S16; break;
    case 24: f.encoding = SampleEncoding::S24; break;
    case 32:
      f.encoding = f.formatTag == kWavTagIeeeFloat ? SampleEncoding::F32
                                                   : SampleEncoding::S32;
      break;
    case 64: f.encoding = SampleEncoding::F64; break;
    default: break;
  }
}

bool WavReader::parseFmt(uint64_t chunkSize, MediaError* err) {
  std::vector<unsigned char> buf(chunkSize);
  if (!readExact(buf.data(), chunkSize, err)) return false;
  if (chunkSize < 16) {
    *err = makeError(ErrorCode::BadChunkLength, 20, -1,
                     "fmt chunk shorter than 16 bytes", false);
    return false;
  }
  auto& f = summary_.format;
  f.formatTag = le16(buf.data());
  f.channels = le16(buf.data() + 2);
  f.sampleRate = le32(buf.data() + 4);
  f.bitsPerSample = le16(buf.data() + 14);

  if (f.formatTag == kWavTagExtensible && chunkSize >= 40) {
    f.channelMask = le32(buf.data() + 24);
    uint16_t subTag = le16(buf.data() + 24 + 20);  // SubFormat 首 2 字节
    f.formatTag = subTag;
  }
  if (f.formatTag != kWavTagPcm && f.formatTag != kWavTagIeeeFloat) {
    *err = makeError(ErrorCode::UnsupportedFormat, 20, -1,
                     "only PCM/IEEE-float WAV is supported", false);
    return false;
  }
  if (f.channels == 0 || f.channels > 32 || f.sampleRate == 0 ||
      f.bitsPerSample == 0 || (f.bitsPerSample % 8) != 0) {
    *err = makeError(ErrorCode::UnsupportedFormat, 20, -1,
                     "invalid channel count, sample rate or bit depth", false);
    return false;
  }
  mapEncoding();
  if (f.encoding == SampleEncoding::S16 && f.bitsPerSample != 16) {
    *err = makeError(ErrorCode::UnsupportedFormat, 20, -1,
                     "unsupported bit depth", false);
    return false;
  }
  fmtSeen_ = true;
  return true;
}

void WavReader::decodeFrame(const unsigned char* raw, int32_t* out) const {
  const auto& f = summary_.format;
  int bytes = f.bitsPerSample / 8;
  for (int ch = 0; ch < f.channels; ++ch) {
    const unsigned char* p = raw + ch * bytes;
    int32_t v = 0;
    switch (f.encoding) {
      case SampleEncoding::U8:
        v = (static_cast<int32_t>(p[0]) - 128) << 24;
        break;
      case SampleEncoding::S16:
        v = static_cast<int32_t>(static_cast<int16_t>(le16(p))) << 16;
        break;
      case SampleEncoding::S24: {
        int32_t s = p[0] | (p[1] << 8) | (p[2] << 16);
        if (s & 0x800000) s |= 0xFF000000;  // 符号扩展
        v = s << 8;
        break;
      }
      case SampleEncoding::S32:
        v = static_cast<int32_t>(le32(p));
        break;
      case SampleEncoding::F32: {
        uint32_t bits = le32(p);
        float fv;
        std::memcpy(&fv, &bits, sizeof(fv));
        double d = static_cast<double>(fv);
        if (d > 1.0) d = 1.0;
        if (d < -1.0) d = -1.0;
        v = static_cast<int32_t>(d * 2147483648.0);
        break;
      }
      case SampleEncoding::F64: {
        uint64_t bits = le64(p);
        double d;
        std::memcpy(&d, &bits, sizeof(d));
        if (d > 1.0) d = 1.0;
        if (d < -1.0) d = -1.0;
        v = static_cast<int32_t>(d * 2147483648.0);
        break;
      }
    }
    out[ch] = v;
  }
}

bool WavReader::readDataChunk(uint64_t chunkSize, const FrameCallback& cb,
                              MediaError* err) {
  const auto& f = summary_.format;
  const int frameSize = f.frameSize();
  std::vector<unsigned char> raw(static_cast<size_t>(blockFrames_) *
                                 frameSize);
  sampleBuffer_.resize(static_cast<size_t>(blockFrames_) * f.channels);
  uint64_t remaining = chunkSize;
  uint64_t chunkStart = in_.tellg();

  while (remaining >= static_cast<uint64_t>(frameSize)) {
    size_t wantFrames = static_cast<size_t>(
        std::min<uint64_t>(blockFrames_, remaining / frameSize));
    size_t wantBytes = wantFrames * frameSize;
    if (!readExact(raw.data(), wantBytes, err)) {
      // 文件在帧中途截断：记录可恢复错误，已解析帧保留。
      MediaError e = makeError(
          ErrorCode::TruncatedChunk,
          static_cast<int64_t>(chunkStart + chunkSize - remaining),
          frameCursor_, "data chunk truncated mid-frame", true);
      if (!handleCorruption(err, e)) return false;
      return true;
    }
    for (size_t i = 0; i < wantFrames; ++i)
      decodeFrame(raw.data() + i * frameSize,
                  sampleBuffer_.data() + i * f.channels);
    AudioBlock block{sampleBuffer_.data(), wantFrames, f.channels,
                     frameCursor_};
    if (cb && !cb(block)) return true;
    frameCursor_ += static_cast<SampleTick>(wantFrames);
    remaining -= wantBytes;
  }

  // 非整帧尾块：读出残余字节并报告原始字节偏移。
  if (remaining > 0) {
    std::vector<unsigned char> tail(remaining);
    int64_t tailOff = static_cast<int64_t>(chunkStart + chunkSize - remaining);
    if (!readExact(tail.data(), remaining, err)) {
      MediaError e = makeError(ErrorCode::TruncatedChunk, tailOff,
                               frameCursor_, "tail bytes missing", true);
      if (!handleCorruption(err, e)) return false;
    } else {
      MediaError e = makeError(ErrorCode::NonIntegerFrameTail, tailOff,
                               frameCursor_,
                               std::to_string(remaining) +
                                   " trailing bytes do not form a whole frame",
                               true);
      if (!handleCorruption(err, e)) return false;
      summary_.tailBytes += remaining;
    }
  }
  summary_.totalFrames = frameCursor_;
  return true;
}

MediaError WavReader::read(const FrameCallback& onFrame) {
  in_.open(path_, std::ios::binary);
  if (!in_)
    return makeError(ErrorCode::IoFailure, -1, -1, "cannot open file", false);
  in_.seekg(0, std::ios::end);
  summary_.fileSize = static_cast<uint64_t>(in_.tellg());
  in_.seekg(0, std::ios::beg);
  summary_.path = path_;

  char riff[12];
  MediaError err;
  if (!readExact(riff, 12, &err)) return err;
  if (std::memcmp(riff, "RIFF", 4) != 0 ||
      std::memcmp(riff + 8, "WAVE", 4) != 0)
    return makeError(ErrorCode::BadRiffHeader, 0, -1,
                     "missing RIFF/WAVE signature", false);

  while (true) {
    char head[8];
    if (!in_.read(head, 8)) {
      if (in_.eof() && in_.gcount() == 0) break;  // 正常结束
      MediaError e = makeError(ErrorCode::TruncatedChunk,
                               static_cast<int64_t>(in_.tellg()),
                               frameCursor_, "chunk header truncated", true);
      if (!handleCorruption(&err, e)) return err;
      break;
    }
    uint32_t sz32 = le32(reinterpret_cast<unsigned char*>(head) + 4);
    uint64_t size = sz32;
    uint64_t dataPos = static_cast<uint64_t>(in_.tellg());

    if (std::memcmp(head, "fmt ", 4) == 0) {
      if (!parseFmt(size, &err)) return err;
    } else if (std::memcmp(head, "data", 4) == 0) {
      if (!fmtSeen_)
        return makeError(ErrorCode::BadChunkLength, dataPos, -1,
                         "data chunk precedes fmt chunk", false);
      summary_.dataBytes += size;
      if (!readDataChunk(size, onFrame, &err)) return err;
    } else {
      // 未知块：按声明长度安全跳过。
      if (!in_.seekg(static_cast<std::streamoff>(size), std::ios::cur)) {
        MediaError e = makeError(ErrorCode::TruncatedChunk,
                                 static_cast<int64_t>(dataPos), frameCursor_,
                                 "skipping unknown chunk ran past EOF", true);
        if (!handleCorruption(&err, e)) return err;
        break;
      }
    }
    if (size & 1ULL) {  // 奇数长度块的填充字节
      char pad;
      if (!in_.get(pad)) break;
    }
  }

  if (!fmtSeen_)
    return makeError(ErrorCode::BadRiffHeader, 0, -1, "no fmt chunk found",
                     false);
  summary_.totalFrames = frameCursor_;
  // 摘要覆盖整个文件字节，分块哈希，避免大文件整体入内存。
  summary_.digest = fsutil::fnvFile(path_);
  return MediaError{};
}

}  // namespace media
