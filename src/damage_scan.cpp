#include "media/damage_scan.hpp"

#include <algorithm>
#include <cstdlib>
#include <limits>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
}

#include "media/fs_util.hpp"

namespace mediaqc {

GapReport check_packet_gaps(const std::vector<PacketInfo>& packets,
                            int sample_rate, int64_t gap_threshold_ns) {
    GapReport out;
    int64_t expect_pts = 0;
    for (const auto& p : packets) {
        const int64_t expected_ns = static_cast<int64_t>(p.frame_samples) * 1'000'000'000LL /
                                    std::max(1, sample_rate);
        if (std::llabs(p.duration_ns - expected_ns) > expected_ns / 4) {
            out.damage.push_back(
                {ErrorCode::BadPacketDuration, p.source_offset, p.pts_samples,
                 p.pts_samples + p.frame_samples,
                 "packet duration " + std::to_string(p.duration_ns) +
                     " ns mismatches " + std::to_string(p.frame_samples) + " frames"});
        }
        if (p.pts_samples != expect_pts) {
            const int64_t gap_ns =
                (p.pts_samples - expect_pts) * 1'000'000'000LL / std::max(1, sample_rate);
            if (gap_ns > gap_threshold_ns) {
                out.damage.push_back(
                    {ErrorCode::KeyframeGap, p.source_offset, expect_pts, p.pts_samples,
                     "packet spacing " + std::to_string(gap_ns) + " ns exceeds threshold"});
            }
        }
        expect_pts = p.pts_samples + p.frame_samples;
    }
    return out;
}

namespace {

// Try decoding one packet through the codec; a rejected packet is a real
// decode error (PCM normally decodes everything that parses).
void decode_check(const std::string& path, std::vector<DamageRecord>& damage) {
    av_log_set_level(AV_LOG_QUIET);
    AVFormatContext* fmt = nullptr;
    if (avformat_open_input(&fmt, path.c_str(), nullptr, nullptr) < 0) return;
    if (avformat_find_stream_info(fmt, nullptr) < 0) {
        avformat_close_input(&fmt);
        return;
    }
    const int idx = av_find_best_stream(fmt, AVMEDIA_TYPE_AUDIO, -1, -1, nullptr, 0);
    if (idx < 0) { avformat_close_input(&fmt); return; }
    const AVCodec* codec = avcodec_find_decoder(fmt->streams[idx]->codecpar->codec_id);
    AVCodecContext* ctx = codec ? avcodec_alloc_context3(codec) : nullptr;
    if (!ctx || avcodec_parameters_to_context(ctx, fmt->streams[idx]->codecpar) < 0 ||
        avcodec_open2(ctx, codec, nullptr) < 0) {
        if (ctx) avcodec_free_context(&ctx);
        avformat_close_input(&fmt);
        return;
    }
    AVPacket* pkt = av_packet_alloc();
    AVFrame* frame = av_frame_alloc();
    while (av_read_frame(fmt, pkt) >= 0) {
        if (pkt->stream_index == idx &&
            (avcodec_send_packet(ctx, pkt) < 0 || avcodec_receive_frame(ctx, frame) < 0)) {
            damage.push_back({ErrorCode::DecodeError, pkt->pos, 0, 0, "codec rejected packet"});
        }
        av_packet_unref(pkt);
        av_frame_unref(frame);
    }
    av_frame_free(&frame);
    av_packet_free(&pkt);
    avcodec_free_context(&ctx);
    avformat_close_input(&fmt);
}

}  // namespace

ScanResult scan_damage(ProbeResult& probe, const ScanConfig& cfg) {
    ScanResult s;
    s.damage = probe.errors;  // header/short-read errors share the same model

    if (!probe.header.riff_ok || !probe.header.data_chunk_found ||
        probe.header.block_align <= 0) {
        s.unrecoverable = true;
        s.first_damage_offset = 0;
        return s;
    }

    // Index recovery: missing sidecar -> sequential scan rebuild.
    if (!probe.index_present) {
        if (!rebuild_index(probe, cfg.frames_per_page)) {
            s.unrecoverable = true;
            s.damage.push_back({ErrorCode::Unrecoverable, probe.header.data_offset, 0, 0,
                                "no decodable boundary; cannot rebuild index"});
            s.first_damage_offset = probe.header.data_offset;
            return s;
        }
        s.index_was_rebuilt = true;
    }

    const auto& h = probe.header;
    const int64_t frame_b = h.block_align;
    const int64_t available = std::max<int64_t>(0, h.available_data_size);

    // Tail truncation: bytes inside the declared chunk that do not form a frame.
    // A shortened file and the partial final frame are the same missing bytes,
    // so they are counted once and the offset points at the first missing byte.
    if (h.declared_data_size > available) {
        const int64_t missing = h.declared_data_size - available;
        s.truncated_bytes = missing;
        s.damage.push_back({ErrorCode::Truncated, h.data_offset + available, 0, 0,
                            "file ends " + std::to_string(missing) +
                                " bytes before declared data size"});
    } else {
        const int64_t tail_rem = available % frame_b;
        if (tail_rem > 0) {
            s.truncated_bytes = tail_rem;
            s.damage.push_back(
                {ErrorCode::Truncated, h.data_offset + available - tail_rem, 0, 0,
                 std::to_string(tail_rem) +
                     " trailing bytes do not form a complete sample frame"});
        }
    }

    // Page CRC validation locates damaged regions precisely (per page).
    const int64_t usable = (available / frame_b) * frame_b;
    for (const auto& page : probe.pages) {
        const int64_t sz = std::min(page.size, h.data_offset + usable - page.offset);
        if (sz <= 0 || sz != page.size) {
            s.damage.push_back({ErrorCode::CrcMismatch, page.offset, page.start_sample,
                                page.end_sample, "index page runs past available data"});
            continue;
        }
        const uint32_t actual = file_range_crc(probe.path, page.offset, page.size);
        if (actual != page.crc32)
            s.damage.push_back({ErrorCode::CrcMismatch, page.offset, page.start_sample,
                                page.end_sample,
                                "crc mismatch: stored=" + std::to_string(page.crc32) +
                                    " actual=" + std::to_string(actual)});
    }

    // Packet stream: gaps, durations and decode errors (stored for the gap check,
    // packets are small fixed-size WAV blocks, not sample data).
    std::vector<PacketInfo> packets;
    stream_packets(probe.path, h, [&](const PacketInfo& p) { packets.push_back(p); },
                   s.packet_count, s.total_frames, s.damage);
    auto gaps = check_packet_gaps(packets, h.sample_rate, cfg.keyframe_gap_ns);
    s.damage.insert(s.damage.end(), gaps.damage.begin(), gaps.damage.end());
    decode_check(probe.path, s.damage);

    for (const auto& d : s.damage) {
        if (d.source_offset >= 0 &&
            (s.first_damage_offset < 0 || d.source_offset < s.first_damage_offset))
            s.first_damage_offset = d.source_offset;
        if (d.end_sample > d.start_sample) {
            if (s.affected_end_sample == 0 || d.start_sample < s.affected_start_sample)
                s.affected_start_sample = d.start_sample;
            s.affected_end_sample = std::max(s.affected_end_sample, d.end_sample);
        }
    }
    std::sort(s.damage.begin(), s.damage.end(),
              [](const DamageRecord& a, const DamageRecord& b) {
                  return a.source_offset != b.source_offset
                             ? a.source_offset < b.source_offset
                             : a.start_sample < b.start_sample;
              });
    return s;
}

}  // namespace mediaqc
