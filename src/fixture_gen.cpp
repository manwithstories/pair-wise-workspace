#include "fixture_gen.hpp"

#include <cmath>
#include <cstdarg>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <vector>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
}

namespace ma {
namespace {

// 确定性伪随机（仅用于故障注入选位，画面内容本身无随机性）。
uint64_t splitmix64(uint64_t& state) {
    state += 0x9e3779b97f4a7c15ULL;
    uint64_t z = state;
    z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ULL;
    z = (z ^ (z >> 27)) * 0x94d049bb133111ebULL;
    return z ^ (z >> 31);
}

void setErr(std::string& err, const char* fmt, ...) {
    char buf[256];
    va_list ap;
    va_start(ap, fmt);
    std::vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    err = buf;
}

constexpr double kPi = 3.14159265358979323846;

// VFR 帧间隔模式（相对名义步长），均值为 1.0 保持名义帧率。
double vfrFactor(int frameNo) {
    static const double pat[3] = {1.0, 1.5, 0.5};
    return pat[frameNo % 3];
}

// 编码并复用一个帧（frame 可为 nullptr 表示 flush）。
bool encodeWrite(AVFormatContext* fmt, AVCodecContext* enc, AVStream* st,
                 AVFrame* frame, AVPacket* pkt, std::string& err) {
    int rc = avcodec_send_frame(enc, frame);
    if (rc < 0) { setErr(err, "send_frame 失败 %d", rc); return false; }
    while (true) {
        rc = avcodec_receive_packet(enc, pkt);
        if (rc == AVERROR(EAGAIN) || rc == AVERROR_EOF) return true;
        if (rc < 0) { setErr(err, "receive_packet 失败 %d", rc); return false; }
        av_packet_rescale_ts(pkt, enc->time_base, st->time_base);
        pkt->stream_index = st->index;
        rc = av_interleaved_write_frame(fmt, pkt); // 内部 unref，pkt 复用
        if (rc < 0) { setErr(err, "write_frame 失败 %d", rc); return false; }
    }
}

} // namespace

bool generateFixture(const FixtureOptions& o, std::string& err) {
    av_log_set_level(AV_LOG_ERROR); // 抑制 libx264/AAC 统计噪音
    // 12800 = 25fps*512：CFR 每帧 512 ticks，VFR 按因子改变步长。
    const int tickRate = o.videoFps * o.gopFrames;
    const AVRational vtb{1, tickRate};
    const AVRational atb{1, o.sampleRate};
    AVFormatContext* fmt = nullptr;
    int rc = avformat_alloc_output_context2(&fmt, nullptr, "mp4", o.path.c_str());
    if (rc < 0 || !fmt) { setErr(err, "无法创建 mp4 输出上下文"); return false; }

    const AVCodec* venc = avcodec_find_encoder(AV_CODEC_ID_H264);
    const AVCodec* aenc = avcodec_find_encoder(AV_CODEC_ID_AAC);
    if (!venc || !aenc) { setErr(err, "缺少 H.264/AAC 编码器"); return false; }

    AVStream* vst = avformat_new_stream(fmt, nullptr);
    AVStream* ast = avformat_new_stream(fmt, nullptr);
    AVCodecContext* vc = avcodec_alloc_context3(venc);
    AVCodecContext* ac = avcodec_alloc_context3(aenc);
    vc->width = o.width; vc->height = o.height;
    vc->pix_fmt = AV_PIX_FMT_YUV420P;
    vc->time_base = vtb;
    vc->framerate = AVRational{o.videoFps, 1};
    vc->gop_size = o.gopFrames;
    vc->max_b_frames = 2;
    av_opt_set(vc->priv_data, "preset", "veryfast", 0);
    av_opt_set(vc->priv_data, "tune", "fastdecode", 0);
    ac->sample_rate = o.sampleRate;
    ac->sample_fmt = AV_SAMPLE_FMT_FLTP;
    ac->ch_layout = AV_CHANNEL_LAYOUT_MONO;
    ac->bit_rate = 64000;
    ac->time_base = atb;

    if (avcodec_open2(vc, venc, nullptr) < 0 ||
        avcodec_open2(ac, aenc, nullptr) < 0) {
        setErr(err, "编码器打开失败"); return false;
    }
    avcodec_parameters_from_context(vst->codecpar, vc);
    avcodec_parameters_from_context(ast->codecpar, ac);
    vst->time_base = vtb; ast->time_base = atb;

    AVDictionary* opts = nullptr;
    if (o.faststart) av_dict_set(&opts, "movflags", "faststart", 0);
    if (!(fmt->oformat->flags & AVFMT_NOFILE)) {
        rc = avio_open(&fmt->pb, o.path.c_str(), AVIO_FLAG_WRITE);
        if (rc < 0) { setErr(err, "无法打开输出文件"); return false; }
    }
    if (avformat_write_header(fmt, &opts) < 0) {
        setErr(err, "写文件头失败"); return false;
    }
    av_dict_free(&opts);

    AVFrame* vf = av_frame_alloc();
    vf->format = vc->pix_fmt; vf->width = o.width; vf->height = o.height;
    av_frame_get_buffer(vf, 0);
    const int totalFrames = o.durationSec * o.videoFps;
    int64_t vpts = 0;
    AVPacket* pkt = av_packet_alloc();

    // ---- 视频：确定性 YUV 图案；VFR 时 PTS 步长按模式变化（均值保持 25fps）----
    for (int n = 0; n < totalFrames; ++n) {
        av_frame_make_writable(vf);
        const int64_t step = o.vfr
            ? static_cast<int64_t>(std::lround(o.gopFrames * vfrFactor(n)))
            : o.gopFrames;
        for (int y = 0; y < o.height; ++y)
            for (int x = 0; x < o.width; ++x)
                vf->data[0][y * vf->linesize[0] + x] =
                    static_cast<uint8_t>((x + y + n * 7) & 0xFF);
        for (int y = 0; y < o.height / 2; ++y)
            for (int x = 0; x < o.width / 2; ++x) {
                vf->data[1][y * vf->linesize[1] + x] =
                    static_cast<uint8_t>(128 + (n * 3 & 31));
                vf->data[2][y * vf->linesize[2] + x] =
                    static_cast<uint8_t>(128 - (n * 3 & 31));
            }
        vf->pts = vpts;
        vpts += step;
        if (!encodeWrite(fmt, vc, vst, vf, pkt, err)) return false;
    }

    // ---- 音频：正弦波；漂移通过逐步加大的实际时间戳步长注入 ----
    AVFrame* af = av_frame_alloc();
    af->format = ac->sample_fmt; af->nb_samples = 1024;
    av_channel_layout_copy(&af->ch_layout, &ac->ch_layout);
    av_frame_get_buffer(af, 0);
    int64_t sampleCursor = 0; // 理想采样位置，保证波形连续
    double driftCursor = 0.0; // 漂移时间戳 double 累加，避免整数取整吞掉小漂移
    const int audioFrames = o.durationSec * o.sampleRate / 1024;
    for (int n = 0; n < audioFrames; ++n) {
        av_frame_make_writable(af);
        float* dst = reinterpret_cast<float*>(af->data[0]);
        for (int s = 0; s < 1024; ++s) {
            const double ph = 2.0 * kPi * 440.0 *
                              (sampleCursor + s) / o.sampleRate;
            dst[s] = static_cast<float>(0.25 * std::sin(ph));
        }
        sampleCursor += 1024;
        af->pts = static_cast<int64_t>(std::llround(driftCursor));
        driftCursor += 1024.0 * (1.0 + o.audioDriftPpm / 1e6);
        if (!encodeWrite(fmt, ac, ast, af, pkt, err)) return false;
    }

    av_frame_free(&vf); av_frame_free(&af);
    encodeWrite(fmt, vc, vst, nullptr, pkt, err);
    encodeWrite(fmt, ac, ast, nullptr, pkt, err);
    av_packet_free(&pkt);
    av_write_trailer(fmt);
    if (!(fmt->oformat->flags & AVFMT_NOFILE)) avio_closep(&fmt->pb);
    avcodec_free_context(&vc); avcodec_free_context(&ac);
    avformat_free_context(fmt);
    return true;
}

namespace {

bool readAll(const std::string& path, std::vector<char>& buf,
             std::string& err) {
    std::ifstream f(path, std::ios::binary | std::ios::ate);
    if (!f) { setErr(err, "无法读取夹具文件: %s", path.c_str()); return false; }
    const auto sz = f.tellg();
    buf.resize(static_cast<size_t>(sz));
    f.seekg(0);
    f.read(buf.data(), sz);
    return bool(f);
}

bool writeAll(const std::string& path, const char* data, size_t n,
              std::string& err) {
    std::ofstream f(path, std::ios::binary | std::ios::trunc);
    if (!f) { setErr(err, "无法写回夹具文件: %s", path.c_str()); return false; }
    f.write(data, static_cast<std::streamsize>(n));
    return bool(f);
}

// 在顶层盒中查找指定 box（mdat 等一层结构）。
size_t findBox(const std::vector<char>& d, const char* tag,
               size_t from = 0) {
    for (size_t p = from; p + 8 <= d.size();) {
        uint32_t size = (static_cast<uint8_t>(d[p]) << 24) |
                        (static_cast<uint8_t>(d[p + 1]) << 16) |
                        (static_cast<uint8_t>(d[p + 2]) << 8) |
                        static_cast<uint8_t>(d[p + 3]);
        if (size < 8 || p + size > d.size()) break;
        if (std::memcmp(d.data() + p + 4, tag, 4) == 0) return p;
        p += size;
    }
    return std::string::npos;
}

// 容器盒：其载荷为子盒序列（ISO BMFF 容器级 box）。
bool isContainerBox(const char* tag) {
    static const char* containers[] = {
        "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf", "mvex"};
    for (const char* c : containers)
        if (std::memcmp(tag, c, 4) == 0) return true;
    return false;
}

// 深度优先查找任意层级的盒（如 stbl 内的 ctts）。
size_t findBoxDeep(const std::vector<char>& d, const char* tag,
                   size_t from, size_t end) {
    for (size_t p = from; p + 8 <= end;) {
        uint32_t size = (static_cast<uint8_t>(d[p]) << 24) |
                        (static_cast<uint8_t>(d[p + 1]) << 16) |
                        (static_cast<uint8_t>(d[p + 2]) << 8) |
                        static_cast<uint8_t>(d[p + 3]);
        if (size < 8 || p + size > end) break;
        const char* cur = d.data() + p + 4;
        if (std::memcmp(cur, tag, 4) == 0) return p;
        if (isContainerBox(cur)) {
            const size_t found = findBoxDeep(d, tag, p + 8, p + size);
            if (found != std::string::npos) return found;
        }
        p += size;
    }
    return std::string::npos;
}

uint32_t be32(const std::vector<char>& d, size_t at) {
    return (static_cast<uint8_t>(d[at]) << 24) |
           (static_cast<uint8_t>(d[at + 1]) << 16) |
           (static_cast<uint8_t>(d[at + 2]) << 8) |
           static_cast<uint8_t>(d[at + 3]);
}

// 解析视频 sample table，返回每个视频样本在文件中的精确字节偏移。
// 依据 stco（chunk 偏移）+ stsc（样本/块映射）+ stsz（样本大小）。
std::vector<size_t> videoSampleOffsets(const std::vector<char>& d) {
    std::vector<size_t> offsets;
    const size_t stbl = findBoxDeep(d, "stbl", 0, d.size());
    if (stbl == std::string::npos) return offsets;
    const size_t stblEnd = stbl + be32(d, stbl);
    const size_t stco = findBoxDeep(d, "stco", stbl, stblEnd);
    const size_t stsz = findBoxDeep(d, "stsz", stbl, stblEnd);
    const size_t stsc = findBoxDeep(d, "stsc", stbl, stblEnd);
    if (stco == std::string::npos || stsz == std::string::npos ||
        stsc == std::string::npos)
        return offsets;

    // 盒内布局：size(4) tag(4) version/flags(4) 之后为载荷。
    const uint32_t nChunks = be32(d, stco + 12);
    std::vector<size_t> chunkOff(nChunks);
    for (uint32_t i = 0; i < nChunks; ++i)
        chunkOff[i] = be32(d, stco + 16 + 4 * i);

    // stsz: +12 sample_size(uniform)，+16 sample_count，变长表从 +20 起。
    const uint32_t uniformSize = be32(d, stsz + 12);
    const uint32_t nSamples = be32(d, stsz + 16);
    std::vector<uint32_t> sizes(nSamples);
    if (uniformSize == 0) {
        for (uint32_t i = 0; i < nSamples; ++i)
            sizes[i] = be32(d, stsz + 20 + 4 * i);
    } else {
        for (auto& s : sizes) s = uniformSize;
    }

    // stsc: +12 entry_count；每项 12 字节位于 +16 起：
    // first_chunk(4) samples_per_chunk(4) sample_desc_index(4)。
    const uint32_t nEntries = be32(d, stsc + 12);
    std::vector<uint32_t> samplesPerChunk(nChunks);
    uint32_t curSpc = 0;
    size_t ent = 0;
    for (uint32_t c = 0; c < nChunks; ++c) {
        while (ent < nEntries &&
               be32(d, stsc + 16 + ent * 12) <= c + 1) {
            curSpc = be32(d, stsc + 20 + ent * 12);
            ++ent;
        }
        samplesPerChunk[c] = curSpc;
    }

    size_t sampleIdx = 0;
    for (uint32_t c = 0; c < nChunks && sampleIdx < nSamples; ++c) {
        size_t cursor = chunkOff[c];
        for (uint32_t s = 0;
             s < samplesPerChunk[c] && sampleIdx < nSamples; ++s) {
            offsets.push_back(cursor);
            cursor += sizes[sampleIdx++];
        }
    }
    return offsets;
}

} // namespace

bool injectTimestampWrap(const std::string& path, int frameIndex,
                         std::string& err) {
    std::vector<char> d;
    if (!readAll(path, d, err)) return false;
    const size_t box = findBoxDeep(d, "ctts", 0, d.size());
    if (box == std::string::npos) {
        setErr(err, "未找到 ctts 盒（需要含 B 帧的视频流）");
        return false;
    }
    // 布局：size(4) tag(4) version/flags(4) entry_count(4)，条目表从 +16 起。
    const uint32_t entries = be32(d, box + 12);
    const size_t table = box + 16;
    int accum = 0;
    for (uint32_t e = 0; e < entries; ++e) {
        const uint32_t count = be32(d, table + 8 * e);
        if (frameIndex < accum + static_cast<int>(count)) {
            const size_t off = table + 8 * e + 4; // composition offset 字段
            d[off] = static_cast<char>(0x80);     // v0 有符号解释为 -2^31
            for (int k = 1; k < 4; ++k) d[off + k] = 0;
            return writeAll(path, d.data(), d.size(), err);
        }
        accum += static_cast<int>(count);
    }
    setErr(err, "帧序号 %d 超出 ctts 条目范围", frameIndex);
    return false;
}

bool injectCorruptPacket(const std::string& path, uint64_t seed,
                         int64_t& posOut, int& corruptPackets,
                         std::string& err) {
    std::vector<char> d;
    if (!readAll(path, d, err)) return false;

    // 经视频 sample table 取得每个视频样本的精确文件偏移，
    // 避免在音视频交错的 mdat 中按字节猜测 NAL。
    const std::vector<size_t> samples = videoSampleOffsets(d);
    if (samples.size() < 4) {
        setErr(err, "视频样本数不足，无法注入损坏包");
        return false;
    }
    auto readNalLen = [&](size_t at) -> uint32_t {
        if (at + 4 > d.size()) return 0;
        return (static_cast<uint8_t>(d[at]) << 24) |
               (static_cast<uint8_t>(d[at + 1]) << 16) |
               (static_cast<uint8_t>(d[at + 2]) << 8) |
               static_cast<uint8_t>(d[at + 3]);
    };

    // 选第 2 个非关键（P-slice, NAL type 1）视频样本：保留全部 IDR 与
    // 首个 P 帧，使损坏位置靠前，保证恢复率 ≥90%。
    size_t target = std::string::npos;
    int pFrames = 0;
    for (size_t off : samples) {
        if (off + 5 > d.size()) continue;
        const uint8_t nalType = static_cast<uint8_t>(d[off + 4]) & 0x1F;
        if (nalType != 1) continue;
        if (++pFrames != 2) continue;
        target = off;
        break;
    }
    if (target == std::string::npos) {
        setErr(err, "未找到可注入的非关键帧视频样本");
        return false;
    }
    const uint32_t nalen = readNalLen(target);
    if (nalen < 16 || target + 4 + nalen > d.size()) {
        setErr(err, "目标 NAL 长度异常");
        return false;
    }
    // 确定性损坏：翻转 NAL header（置 forbidden_zero_bit、破坏类型字段）
    // 并翻转片身前 24 字节，稳定触发解码器报错而非被容错跳过。
    uint64_t st = seed;
    corruptPackets = 1;
    d[target + 4] = static_cast<char>(
        static_cast<uint8_t>(d[target + 4]) | 0x80); // forbidden bit
    for (int k = 0; k < 24; ++k) {
        const size_t at = target + 5 +
            (splitmix64(st) % static_cast<uint64_t>(nalen - 8));
        d[at] = static_cast<char>(static_cast<uint8_t>(d[at]) ^ 0xFF);
    }
    posOut = static_cast<int64_t>(target);
    return writeAll(path, d.data(), d.size(), err);
}

bool truncateFile(const std::string& path, double keepRatio,
                  int64_t& newSize, std::string& err) {
    std::vector<char> d;
    if (!readAll(path, d, err)) return false;
    if (keepRatio <= 0.0 || keepRatio >= 1.0) {
        setErr(err, "keepRatio 必须位于 (0,1)"); return false;
    }
    const size_t n = static_cast<size_t>(d.size() * keepRatio);
    newSize = static_cast<int64_t>(n);
    return writeAll(path, d.data(), n, err);
}

} // namespace ma
