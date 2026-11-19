// capture_gen.cpp — 采集流生成（样例数据随代码生成，不引用外部文件）
#include "packet_source.hpp"
#include "util.hpp"

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/log.h>
#include <libavutil/opt.h>
#include <libavutil/frame.h>
}

#include <algorithm>
#include <cmath>
#include <cstring>
#include <map>
#include <random>

namespace md {
namespace {

constexpr char MAGIC[8] = {'M','D','C','A','P','0','1','\n'};
constexpr uint32_t SSRC_VIDEO = 0x10000001;
constexpr uint32_t SSRC_AUDIO = 0x20000002;
constexpr uint32_t VIDEO_CLOCK = 90000;
constexpr uint32_t AUDIO_CLOCK = 16000;
constexpr int VIDEO_W = 160, VIDEO_H = 90, VIDEO_FPS = 30, GOP = 15;
void wrap_record(std::vector<uint8_t>& out, RecordKind kind, const std::vector<uint8_t>& body) {
    RecordHeader rh{uint8_t(kind), uint16_t(body.size())};
    out.push_back(rh.kind);
    put_u16(out, rh.len);
    out.insert(out.end(), body.begin(), body.end());
}

std::vector<uint8_t> rtp_body(uint8_t pt, uint16_t seq, uint32_t ts,
                              uint32_t ssrc, bool marker, CodecId codec,
                              uint32_t clock_rate,
                              const uint8_t* pl, size_t pl_len) {
    std::vector<uint8_t> b;
    b.push_back(pt);
    put_u16(b, seq);
    put_u32(b, ts);
    put_u32(b, ssrc);
    b.push_back(marker ? 1 : 0);
    b.push_back(uint8_t(codec));
    put_u16(b, uint16_t(clock_rate & 0xFFFF));
    b.insert(b.end(), pl, pl + pl_len);
    return b;
}

std::vector<uint8_t> rtcp_body(uint32_t ssrc, uint32_t rtp_ts, int64_t ntp_us) {
    std::vector<uint8_t> b;
    put_u32(b, ssrc);
    put_u32(b, rtp_ts);
    put_u64(b, uint64_t(ntp_us));
    return b;
}

// 确定性 RNG：同 seed 产出完全一致的采集流
struct Rng {
    std::mt19937_64 e;
    explicit Rng(uint64_t seed) : e(seed) {}
    bool hit(double p) { return std::uniform_real_distribution<>(0, 1)(e) < p; }
    uint32_t below(uint32_t n) { return uint32_t(std::uniform_int_distribution<>(0, int(n) - 1)(e)); }
};

// ---- H.264 合成画面编码：固定 GOP，产出单 NAL 单元包 ----
std::vector<std::vector<uint8_t>> encode_h264(uint32_t frames, Rng& rng) {
    const AVCodec* c = avcodec_find_encoder(AV_CODEC_ID_H264);
    std::vector<std::vector<uint8_t>> out;
    if (!c) return out;
    AVCodecContext* ctx = avcodec_alloc_context3(c);
    ctx->width = VIDEO_W; ctx->height = VIDEO_H;
    ctx->time_base = AVRational{1, VIDEO_FPS};
    ctx->framerate = AVRational{VIDEO_FPS, 1};
    ctx->pix_fmt = AV_PIX_FMT_YUV420P;
    ctx->gop_size = GOP;
    ctx->max_b_frames = 0;
    av_opt_set(ctx->priv_data, "preset", "ultrafast", 0);
    av_opt_set(ctx->priv_data, "tune", "zerolatency", 0);
    if (avcodec_open2(ctx, c, nullptr) < 0) { avcodec_free_context(&ctx); return out; }
    AVFrame* f = av_frame_alloc();
    f->format = ctx->pix_fmt; f->width = VIDEO_W; f->height = VIDEO_H;
    av_frame_get_buffer(f, 0);
    AVPacket* pkt = av_packet_alloc();
    for (uint32_t i = 0; i < frames; ++i) {
        av_frame_make_writable(f);
        // 确定性变化图案：亮度随帧号渐变，保证各帧内容不同
        for (int y = 0; y < VIDEO_H; ++y)
            for (int x = 0; x < VIDEO_W; ++x)
                f->data[0][y * f->linesize[0] + x] = uint8_t((i * 7 + x + y) & 0xFF);
        for (int y = 0; y < VIDEO_H / 2; ++y)
            for (int x = 0; x < VIDEO_W / 2; ++x) {
                f->data[1][y * f->linesize[1] + x] = 128 + int(rng.below(9)) - 4;
                f->data[2][y * f->linesize[2] + x] = 128 + int(rng.below(9)) - 4;
            }
        f->pts = int64_t(i);
        if (avcodec_send_frame(ctx, f) == 0) {
            while (avcodec_receive_packet(ctx, pkt) == 0) {
                out.emplace_back(pkt->data, pkt->data + pkt->size);
                av_packet_unref(pkt);
            }
        }
    }
    avcodec_send_frame(ctx, nullptr);
    while (avcodec_receive_packet(ctx, pkt) == 0) {
        out.emplace_back(pkt->data, pkt->data + pkt->size);
        av_packet_unref(pkt);
    }
    av_packet_free(&pkt);
    av_frame_free(&f);
    avcodec_free_context(&ctx);
    return out;
}

// 7 字节 ADTS 头（AAC-LC, 单声道 16k）
void write_adts(std::vector<uint8_t>& frame, size_t aac_len) {
    size_t len = aac_len + 7;
    uint8_t h[7] = {0xFF, 0xF1, 0x28, 0, 0, 0, 0xFC};
    h[3] = uint8_t((len >> 5) & 0x1F);
    h[4] = uint8_t((len << 3) & 0xFF);
    frame.insert(frame.begin(), h, h + 7);
}

// 合成音频：正弦音 + 确定性静音区间；AAC 时编码为 ADTS 帧
std::vector<std::vector<uint8_t>>
encode_audio(uint32_t duration_ms, bool aac, std::vector<int16_t>& pcm_all) {
    constexpr uint32_t rate = AUDIO_CLOCK;
    const uint32_t total = rate * duration_ms / 1000;
    pcm_all.reserve(total);
    // 每 500ms 安排 120ms 静音（用于静音检测/跨边界场景）
    for (uint32_t n = 0; n < total; ++n) {
        uint32_t phase = n % (rate / 2);            // 500ms 周期
        bool silent = phase >= (rate / 2 - rate * 120 / 1000);
        int16_t s = 0;
        if (!silent) {
            double v = 0.35 * std::sin(2.0 * 3.14159265358979323846 * 440.0 * n / rate);
            s = int16_t(v * 32767.0);
        }
        pcm_all.push_back(s);
    }
    std::vector<std::vector<uint8_t>> packets;
    if (!aac) {
        // PCM：每包 320 采样 = 20ms
        constexpr uint32_t frame_n = 320;
        for (size_t off = 0; off + frame_n <= pcm_all.size(); off += frame_n) {
            const auto* p = reinterpret_cast<const uint8_t*>(&pcm_all[off]);
            packets.emplace_back(p, p + frame_n * sizeof(int16_t));
        }
        return packets;
    }
    const AVCodec* c = avcodec_find_encoder(AV_CODEC_ID_AAC);
    AVCodecContext* ctx = avcodec_alloc_context3(c);
    ctx->sample_rate = rate;
    ctx->sample_fmt = AV_SAMPLE_FMT_FLTP;
    ctx->ch_layout = AV_CHANNEL_LAYOUT_MONO;
    ctx->bit_rate = 24000;
    avcodec_open2(ctx, c, nullptr);
    AVFrame* f = av_frame_alloc();
    f->format = ctx->sample_fmt; f->nb_samples = ctx->frame_size;
    av_channel_layout_copy(&f->ch_layout, &ctx->ch_layout);
    av_frame_get_buffer(f, 0);
    AVPacket* pkt = av_packet_alloc();
    auto drain = [&] {
        while (avcodec_receive_packet(ctx, pkt) == 0) {
            std::vector<uint8_t> fr(pkt->data, pkt->data + pkt->size);
            write_adts(fr, pkt->size);
            packets.push_back(std::move(fr));
            av_packet_unref(pkt);
        }
    };
    for (size_t off = 0; off + ctx->frame_size <= pcm_all.size(); off += ctx->frame_size) {
        av_frame_make_writable(f);
        float* dst = reinterpret_cast<float*>(f->data[0]);
        for (int i = 0; i < ctx->frame_size; ++i)
            dst[i] = float(pcm_all[off + i]) / 32768.0f;
        f->pts = int64_t(off);
        if (avcodec_send_frame(ctx, f) == 0) drain();
    }
    avcodec_send_frame(ctx, nullptr);
    drain();
    av_packet_free(&pkt);
    av_frame_free(&f);
    avcodec_free_context(&ctx);
    return packets;
}

struct TimedRtp {
    uint8_t pt; uint16_t seq; uint32_t ts; uint32_t ssrc;
    bool marker; CodecId codec; uint32_t clock;
    int64_t send_us;           // 64 位单调发送时间（排序键，线上仍用 32 位 ts）
    std::vector<uint8_t> pl;
};

} // namespace

uint32_t generate_capture(const std::string& out_path,
                          const GenerateOptions& opt, PipelineError& err) {
    Rng rng(opt.seed);
    av_log_set_level(AV_LOG_QUIET); // 库日志不进入终端指标输出
    const uint32_t frames = opt.duration_ms * VIDEO_FPS / 1000;
    auto h264 = encode_h264(frames, rng);
    std::vector<int16_t> pcm_all;
    auto audio = encode_audio(opt.duration_ms, opt.aac, pcm_all);

    std::vector<TimedRtp> stream;
    // 视频：seq 从 65530 起触发 16 位序号回绕；ts 起点贴近 32 位边界，
    // 使整段（约 27 万滴答）只跨越一次时间戳回绕，符合真实 ~13h 回绕语义
    uint16_t vseq = 65530;
    uint32_t vts = 0xFFFFE000u;
    uint64_t v_ext = vts;       // 64 位扩展时间戳，保证排序单调
    const int64_t v_t0 = int64_t(v_ext * 1'000'000ull / VIDEO_CLOCK);
    for (size_t i = 0; i < h264.size(); ++i) {
        int64_t send_us = int64_t(v_ext * 1'000'000ull / VIDEO_CLOCK) - v_t0;
        stream.push_back({PT_H264, vseq++, vts, SSRC_VIDEO, true,
                          CodecId::H264, VIDEO_CLOCK, send_us,
                          std::move(h264[i])});
        vts += VIDEO_CLOCK / VIDEO_FPS;
        v_ext += VIDEO_CLOCK / VIDEO_FPS;
    }
    // 音频
    uint16_t aseq = 30000;
    uint32_t ats = 0x1000;
    uint64_t a_ext = ats;
    const uint32_t a_step = AUDIO_CLOCK / 50; // 20ms
    const int64_t a_t0 = int64_t(a_ext * 1'000'000ull / AUDIO_CLOCK);
    for (size_t i = 0; i < audio.size(); ++i) {
        int64_t send_us = int64_t(a_ext * 1'000'000ull / AUDIO_CLOCK) - a_t0;
        stream.push_back({opt.aac ? PT_AAC : PT_PCM, aseq++, ats, SSRC_AUDIO,
                          i + 1 == audio.size(),
                          opt.aac ? CodecId::Aac : CodecId::PcmS16,
                          AUDIO_CLOCK, send_us, std::move(audio[i])});
        ats += a_step;
        a_ext += a_step;
    }
    // 末尾重复素材（跨机位同内容）：复制前 8 个视频包，新 SSRC
    if (opt.duplicate_tail_ms > 0) {
        uint32_t n = std::min<uint32_t>(8, uint32_t(h264.size()));
        uint16_t dseq = 40000; uint32_t dts = 0x500000;
        uint64_t d_ext = dts;
        const int64_t d_t0 = int64_t(d_ext * 1'000'000ull / VIDEO_CLOCK);
        for (uint32_t i = 0; i < n; ++i) {
            int64_t send_us = int64_t(d_ext * 1'000'000ull / VIDEO_CLOCK) - d_t0;
            stream.push_back({PT_H264, dseq++, dts, SSRC_VIDEO + 1, true,
                              CodecId::H264, VIDEO_CLOCK, send_us, h264[i]});
            dts += VIDEO_CLOCK / VIDEO_FPS;
            d_ext += VIDEO_CLOCK / VIDEO_FPS;
        }
    }
    // 按 64 位单调媒体时间排序形成发送顺序（同刻视频优先），稳定排序
    std::stable_sort(stream.begin(), stream.end(),
                     [](const TimedRtp& a, const TimedRtp& b) {
                         if (a.send_us != b.send_us) return a.send_us < b.send_us;
                         return a.ssrc < b.ssrc;
                     });

    // 固化丢包、乱序、重复；回绕由小起始序号/时间戳自然产生
    std::vector<TimedRtp> sent;
    sent.reserve(stream.size() * 2);
    for (size_t i = 0; i < stream.size(); ++i) {
        if (rng.hit(opt.loss)) continue;            // 丢包：不写入文件
        // 乱序：与下一条交换投递顺序（下一条无论是否丢都补投，保证可复现）
        if (rng.hit(opt.reorder) && i + 1 < stream.size()) {
            if (!rng.hit(opt.loss)) sent.push_back(stream[i + 1]);
            sent.push_back(stream[i]);
            if (rng.hit(opt.duplicate)) sent.push_back(stream[i]);
            ++i;
            continue;
        }
        sent.push_back(stream[i]);
        if (rng.hit(opt.duplicate)) sent.push_back(stream[i]); // 重复投递
    }

    // 组装记录（含周期性 RTCP）
    std::vector<uint8_t> records;
    uint32_t record_count = 0;
    uint64_t payload_bytes = 0;
    auto push_rtp = [&](const TimedRtp& t) {
        auto body = rtp_body(t.pt, t.seq, t.ts, t.ssrc, t.marker,
                             t.codec, t.clock, t.pl.data(), t.pl.size());
        wrap_record(records, RecordKind::Rtp, body);
        payload_bytes += t.pl.size();
        ++record_count;
    };
    // 每 SSRC 独立的 RTCP 计数：首个 RTP 之后立即发一条 SR 建立时钟锚点，
    // 此后每 50 个 RTP 发一条；符合真实发送者报告语义并保证统一基准一致
    std::map<uint32_t, uint32_t> rtp_seen_per_ssrc;
    std::map<uint32_t, uint32_t> rtcp_counter;
    for (size_t i = 0; i < sent.size(); ++i) {
        const TimedRtp& t = sent[i];
        uint32_t seen = ++rtp_seen_per_ssrc[t.ssrc];
        if (seen == 1 || (seen > 1 && seen % 50 == 0)) {
            // RTCP 发送者报告：ntp_us 取该包的 64 位扩展媒体时间，
            // rtp_ts 仍为线上 32 位（可能回绕），由时间轴阶段重新扩展
            auto rb = rtcp_body(t.ssrc, t.ts, t.send_us);
            wrap_record(records, RecordKind::Rtcp, rb);
            ++record_count;
            ++rtcp_counter[t.ssrc];
        }
        push_rtp(t);
    }

    uint32_t flags = 0;
    if (opt.wrap_mode != 0) flags |= 1u;
    CaptureHeader hdr{};
    std::memcpy(hdr.magic, MAGIC, 8);
    hdr.version = 1; hdr.flags = flags; hdr.seed = opt.seed;
    hdr.record_count = record_count; hdr.payload_bytes = payload_bytes;
    std::vector<uint8_t> file;
    for (size_t i = 0; i < sizeof(hdr); ++i) file.push_back(reinterpret_cast<uint8_t*>(&hdr)[i]);
    file.insert(file.end(), records.begin(), records.end());
    if (opt.truncated && file.size() > 64)
        file.resize(file.size() - 17); // 截断最后一条记录的一部分

    std::string io_err;
    if (!atomic_write(out_path, file, io_err)) {
        err = {ErrType::DiskWriteFailed, stage::Source, 0, io_err};
        return 0;
    }
    return record_count;
}

} // namespace md
