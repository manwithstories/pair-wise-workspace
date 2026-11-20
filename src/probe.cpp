#include "media/probe.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <fstream>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
#include <libavutil/error.h>
}

#include "media/fs_util.hpp"
#include "media/json.hpp"

namespace mediaqc {
namespace {

uint32_t le32(const unsigned char* p) {
    return static_cast<uint32_t>(p[0]) | (static_cast<uint32_t>(p[1]) << 8) |
           (static_cast<uint32_t>(p[2]) << 16) | (static_cast<uint32_t>(p[3]) << 24);
}
uint16_t le16(const unsigned char* p) {
    return static_cast<uint16_t>(p[0] | (p[1] << 8));
}

// FourCC as stored on disk: first character in the least-significant byte.
uint32_t fourcc(char a, char b, char c, char d) {
    return static_cast<uint32_t>(static_cast<unsigned char>(a)) |
           (static_cast<uint32_t>(static_cast<unsigned char>(b)) << 8) |
           (static_cast<uint32_t>(static_cast<unsigned char>(c)) << 16) |
           (static_cast<uint32_t>(static_cast<unsigned char>(d)) << 24);
}

bool read_exact(std::ifstream& f, unsigned char* buf, size_t n, int64_t off,
                std::vector<DamageRecord>& errors, const std::string& why) {
    f.read(reinterpret_cast<char*>(buf), static_cast<std::streamsize>(n));
    if (static_cast<size_t>(f.gcount()) != n) {
        errors.push_back({ErrorCode::ShortRead, off, 0, 0, why});
        return false;
    }
    return true;
}

}  // namespace

ProbeResult probe_wav_header(const std::string& path) {
    ProbeResult r;
    r.path = path;
    const auto st = fs::stat(path);
    r.file_size = st.size;
    std::ifstream f(path, std::ios::binary);
    if (!f) {
        r.errors.push_back({ErrorCode::ShortRead, 0, 0, 0, "cannot open file"});
        return r;
    }

    unsigned char riff[12];
    if (!read_exact(f, riff, 12, 0, r.errors, "short read in RIFF header")) return r;
    if (std::memcmp(riff, "RIFF", 4) != 0 || std::memcmp(riff + 8, "WAVE", 4) != 0) {
        r.errors.push_back({ErrorCode::HeaderOutOfRange, 0, 0, 0, "not a RIFF/WAVE file"});
        return r;
    }
    const uint32_t declared_riff = le32(riff + 4);
    if (static_cast<int64_t>(declared_riff) + 8 > st.size)
        r.errors.push_back({ErrorCode::ShortRead, 8, 0, 0,
                            "RIFF size " + std::to_string(declared_riff) + " exceeds file"});
    r.header.riff_ok = true;

    unsigned char ch[8];
    while (true) {
        f.read(reinterpret_cast<char*>(ch), 8);
        if (f.gcount() == 0 && f.eof()) break;  // clean end after last chunk
        if (f.gcount() != 8) {
            r.errors.push_back({ErrorCode::ShortRead, static_cast<int64_t>(f.tellg()), 0, 0,
                                "partial chunk header at end of file"});
            break;
        }
        const int64_t chunk_off = static_cast<int64_t>(f.tellg()) - 8;
        const uint32_t id = le32(ch);
        uint64_t csize = le32(ch + 4);
        const int64_t body_off = chunk_off + 8;
        if (static_cast<int64_t>(body_off + csize) > st.size) {
            r.errors.push_back({ErrorCode::ShortRead, body_off, 0, 0,
                                "chunk '" + std::string(reinterpret_cast<char*>(ch), 4) +
                                "' extends past end of file"});
        }
        if (id == fourcc('f', 'm', 't', ' ') && csize >= 16) {
            std::vector<unsigned char> fmt(csize);
            if (!read_exact(f, fmt.data(), fmt.size(), body_off, r.errors, "short read in fmt chunk"))
                return r;
            r.header.audio_format = le16(fmt.data());
            r.header.channels = le16(fmt.data() + 2);
            r.header.sample_rate = static_cast<int>(le32(fmt.data() + 4));
            r.header.byte_rate = static_cast<int64_t>(le32(fmt.data() + 8));
            r.header.block_align = le16(fmt.data() + 12);
            r.header.bits_per_sample = le16(fmt.data() + 14);
            if (r.header.channels < 1 || r.header.channels > 64 ||
                r.header.sample_rate < 1 || r.header.sample_rate > 768000 ||
                r.header.bits_per_sample < 1 || r.header.bits_per_sample > 1024 ||
                r.header.block_align != (r.header.channels * r.header.bits_per_sample) / 8)
                r.errors.push_back({ErrorCode::HeaderOutOfRange, body_off, 0, 0,
                                    "fmt field out of range or block_align mismatch"});
        } else if (id == fourcc('d', 'a', 't', 'a')) {
            r.header.data_chunk_found = true;
            r.header.data_offset = body_off;
            r.header.declared_data_size = static_cast<int64_t>(csize);
            r.header.available_data_size = std::min<int64_t>(static_cast<int64_t>(csize),
                                                             st.size - body_off);
            r.header.actual_data_size = r.header.available_data_size;
        }
        // Word-aligned chunks; seek to the absolute next chunk boundary so
        // the position is correct whether or not the body was just read.
        f.clear();
        f.seekg(static_cast<std::streamoff>(body_off + csize + (csize & 1)),
                std::ios::beg);
        if (!f) { r.errors.push_back({ErrorCode::ShortRead, body_off, 0, 0, "seek past EOF"}); break; }
    }
    if (!r.header.data_chunk_found)
        r.errors.push_back({ErrorCode::HeaderOutOfRange, std::min<int64_t>(st.size, 12),
                            0, 0, "WAVE file has no data chunk"});
    return r;
}

void load_index(ProbeResult& r) {
    const std::string ipath = r.path + ".idx.json";
    const auto st = fs::stat(ipath);
    if (!st.exists) {
        r.errors.push_back({ErrorCode::MissingIndex, r.file_size, 0, 0,
                            "index sidecar missing: " + ipath});
        return;
    }
    json::Value doc;
    try {
        doc = json::parse(fs::read_file(ipath));
    } catch (const std::exception& e) {
        r.errors.push_back({ErrorCode::MissingIndex, 0, 0, 0, std::string("index unreadable: ") + e.what()});
        return;
    }
    const auto* jpages = doc.find("pages");
    if (!jpages || !jpages->is(json::Type::Array)) {
        r.errors.push_back({ErrorCode::MissingIndex, 0, 0, 0, "index has no pages array"});
        return;
    }
    int expected = 0;
    int64_t expect_offset = r.header.data_offset;
    for (const auto& jp : jpages->as_array()) {
        IndexPage p;
        p.page_no = static_cast<int>(jp.find("page_no")->as_int());
        p.offset = jp.find("offset")->as_int();
        p.size = jp.find("size")->as_int();
        p.start_sample = jp.find("start_sample")->as_int();
        p.end_sample = jp.find("end_sample")->as_int();
        p.crc32 = static_cast<uint32_t>(jp.find("crc32")->as_int());
        if (p.page_no != expected || p.offset != expect_offset) {
            r.errors.push_back({ErrorCode::IndexGap, p.offset, p.start_sample, p.end_sample,
                                "index pages non-contiguous at page " + std::to_string(p.page_no)});
            return;
        }
        r.pages.push_back(p);
        expect_offset += p.size;
        ++expected;
    }
    r.index_present = true;
}

bool rebuild_index(ProbeResult& r, int frames_per_page) {
    const auto& h = r.header;
    if (h.data_offset < 0 || h.block_align <= 0 || h.channels <= 0) return false;
    const int64_t frame_bytes = h.block_align;
    const int64_t usable = (r.header.actual_data_size / frame_bytes) * frame_bytes;
    if (usable <= 0) return false;  // no decodable boundary at all
    const int64_t page_bytes = static_cast<int64_t>(frames_per_page) * frame_bytes;
    int64_t off = h.data_offset;
    int64_t sample = 0;
    int no = 0;
    while (off < h.data_offset + usable) {
        const int64_t sz = std::min(page_bytes, h.data_offset + usable - off);
        const int64_t frames = sz / frame_bytes;
        r.pages.push_back({no, off, sz, sample, sample + frames,
                           file_range_crc(r.path, off, sz)});
        off += sz;
        sample += frames;
        ++no;
    }
    r.index_rebuilt = true;
    r.index_present = true;
    return true;
}

bool stream_packets(const std::string& path, const WavHeader& h,
                    const PacketCallback& cb, int64_t& packet_count,
                    int64_t& total_frames, std::vector<DamageRecord>& errors) {
    av_log_set_level(AV_LOG_QUIET);  // keep terminal output to our own metrics
    AVFormatContext* fmt = nullptr;
    if (avformat_open_input(&fmt, path.c_str(), nullptr, nullptr) < 0) {
        errors.push_back({ErrorCode::HeaderOutOfRange, 0, 0, 0, "avformat cannot open input"});
        return false;
    }
    if (avformat_find_stream_info(fmt, nullptr) < 0) {
        errors.push_back({ErrorCode::HeaderOutOfRange, 0, 0, 0, "cannot find stream info"});
        avformat_close_input(&fmt);
        return false;
    }
    int stream_index = av_find_best_stream(fmt, AVMEDIA_TYPE_AUDIO, -1, -1, nullptr, 0);
    if (stream_index < 0) {
        errors.push_back({ErrorCode::HeaderOutOfRange, 0, 0, 0, "no audio stream"});
        avformat_close_input(&fmt);
        return false;
    }
    AVStream* st = fmt->streams[stream_index];
    AVPacket* pkt = av_packet_alloc();
    const int frame_b = h.block_align > 0 ? h.block_align
                    : static_cast<int>(st->codecpar->block_align);
    while (true) {
        const int rc = av_read_frame(fmt, pkt);
        if (rc == AVERROR_EOF) break;
        if (rc < 0) {
            char errbuf[AV_ERROR_MAX_STRING_SIZE];
            errors.push_back({ErrorCode::DecodeError, pkt->pos, 0, 0,
                              std::string("av_read_frame failed: ") +
                                  av_make_error_string(errbuf, sizeof(errbuf), rc)});
            break;
        }
        if (pkt->stream_index != stream_index || frame_b <= 0) {
            av_packet_unref(pkt);
            continue;
        }
        const int frames = pkt->size / frame_b;
        const int64_t pts_samples = pkt->pts != AV_NOPTS_VALUE
            ? av_rescale_q(pkt->pts, st->time_base, AVRational{1, st->codecpar->sample_rate})
            : total_frames;
        PacketInfo info;
        info.source_offset = pkt->pos;
        info.pts_samples = pts_samples;
        info.frame_samples = frames;
        info.duration_ns = av_rescale_q(pkt->duration > 0 ? pkt->duration : frames,
                                        st->time_base, AVRational{1, 1'000'000'000});
        info.keyframe = (pkt->flags & AV_PKT_FLAG_KEY) != 0;
        cb(info);
        ++packet_count;
        total_frames += frames;
        av_packet_unref(pkt);
    }
    av_packet_free(&pkt);
    avformat_close_input(&fmt);
    return true;
}

uint32_t file_range_crc(const std::string& path, int64_t offset, int64_t size) {
    std::ifstream f(path, std::ios::binary);
    if (!f) throw std::runtime_error("cannot open for crc: " + path);
    f.seekg(static_cast<std::streamoff>(offset));
    std::vector<unsigned char> buf(1 << 20);
    uint32_t crc = 0;
    int64_t left = size;
    while (left > 0) {
        const auto n = static_cast<std::streamsize>(std::min<int64_t>(left, buf.size()));
        f.read(reinterpret_cast<char*>(buf.data()), n);
        const auto got = f.gcount();
        if (got <= 0) break;
        crc = fs::crc32_ieee(buf.data(), static_cast<size_t>(got), crc);
        left -= got;
    }
    return crc;
}

}  // namespace mediaqc
