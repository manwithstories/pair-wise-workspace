// capture_read.cpp — 流式读取采集流（常量内存，截断/不支持编码可追溯）
#include "packet_source.hpp"
#include "util.hpp"
#include "sha256.hpp"

#include <cstring>
#include <fstream>

namespace md {
namespace {

constexpr char MAGIC[8] = {'M','D','C','A','P','0','1','\n'};

// 流式哈希：64KB 缓冲，峰值内存与输入时长无关
std::array<uint8_t, 32> hash_file(const std::string& path, bool& ok) {
    std::ifstream f(path, std::ios::binary);
    Sha256 h;
    std::vector<uint8_t> buf(65536);
    ok = bool(f);
    while (f) {
        f.read(reinterpret_cast<char*>(buf.data()),
               static_cast<std::streamsize>(buf.size()));
        std::streamsize n = f.gcount();
        if (n > 0) h.update(buf.data(), size_t(n));
    }
    return h.finish();
}

} // namespace

bool PacketSource::open(const std::string& path, PipelineError& err) {
    bool ok = false;
    summary_.sha256 = hash_file(path, ok);
    if (!ok) {
        err = {ErrType::IoReadFailed, stage::Source, 0, "cannot open input: " + path};
        return false;
    }
    auto in = std::make_unique<std::ifstream>(path, std::ios::binary);
    CaptureHeader hdr{};
    in->read(reinterpret_cast<char*>(&hdr), sizeof(hdr));
    if (in->gcount() != std::streamsize(sizeof(hdr)) ||
        std::memcmp(hdr.magic, MAGIC, 8) != 0) {
        err = {ErrType::BadMagic, stage::Source, 0, "bad magic or short header"};
        return false;
    }
    summary_.seed = hdr.seed;
    summary_.version = hdr.version;
    summary_.flags = hdr.flags;
    summary_.record_count = hdr.record_count;
    summary_.payload_bytes = hdr.payload_bytes;
    pos_ = sizeof(hdr);
    records_seen_ = 0;
    stream_ = std::move(in);
    return true;
}

bool PacketSource::next_rtp(MediaPacket& out, PipelineError& err) {
    while (true) {
        uint8_t rh[3];
        stream_->read(reinterpret_cast<char*>(rh), 3);
        std::streamsize got = stream_->gcount();
        if (got == 0) return false;                       // 干净结束
        if (got != 3) {
            err = {ErrType::TruncatedRecord, stage::Source, records_seen_,
                   "record header truncated"};
            return false;
        }
        uint16_t len = get_u16(rh + 1);
        std::vector<uint8_t> body(len);
        if (len > 0) {
            stream_->read(reinterpret_cast<char*>(body.data()), len);
            if (stream_->gcount() != std::streamsize(len)) {
                err = {ErrType::TruncatedRecord, stage::Source, records_seen_,
                       "record body truncated"};
                return false;
            }
        }
        pos_ += 3 + len;
        ++records_seen_;
        if (rh[0] == uint8_t(RecordKind::Rtcp)) {
            if (len < 16) {
                err = {ErrType::TruncatedRecord, stage::Source, records_seen_,
                       "rtcp record truncated"};
                return false;
            }
            RtcpReport rr;
            rr.ssrc = get_u32(body.data());
            rr.rtp_ts = get_u32(body.data() + 4);
            rr.ntp_us = int64_t(get_u64(body.data() + 8));
            rtcp_.push_back(rr);
            continue;
        }
        if (rh[0] != uint8_t(RecordKind::Rtp)) {
            err = {ErrType::BadMagic, stage::Source, records_seen_,
                   "unknown record kind"};
            return false;
        }
        if (len < 15) {
            err = {ErrType::TruncatedRecord, stage::Source, records_seen_,
                   "rtp record truncated"};
            return false;
        }
        CodecId codec = CodecId(body[12]);
        if (codec != CodecId::H264 && codec != CodecId::PcmS16 &&
            codec != CodecId::Aac) {
            err = {ErrType::UnsupportedCodec, stage::Source, records_seen_,
                   std::string("unsupported codec id ") +
                       std::to_string(unsigned(body[12]))};
            return false;
        }
        out = MediaPacket{};
        out.rtp_ts = get_u32(body.data() + 3);
        out.seq = uint16_t(get_u16(body.data() + 1));
        out.ssrc = get_u32(body.data() + 7);
        out.marker = body[11] != 0;
        out.codec = codec;
        out.clock_rate = uint32_t(get_u16(body.data() + 13));
        if (out.clock_rate == 0) out.clock_rate = 90000;
        out.payload.assign(body.begin() + 15, body.end());
        return true;
    }
}

} // namespace md
