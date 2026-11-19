#include "probe.hpp"

#include <limits>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/avutil.h>
}

namespace ma {

void IntervalStats::finalize(std::vector<double>& samples) {
    count = static_cast<int>(samples.size());
    if (samples.empty()) return;
    minMs = std::numeric_limits<double>::infinity();
    maxMs = 0.0;
    double sum = 0.0;
    for (double v : samples) {
        minMs = std::min(minMs, v);
        maxMs = std::max(maxMs, v);
        sum += v;
    }
    meanMs = sum / static_cast<double>(samples.size());
    p95Ms = percentileMs(samples, 95.0);
}

ProbeInfo::~ProbeInfo() {
    if (fmt) avformat_close_input(&fmt);
}

bool probeOpen(const std::string& path, ProbeInfo& out, std::string& err) {
    AVFormatContext* fmt = nullptr;
    // 自定义 IO 由上层保证路径为本地 MP4/MOV；只读打开。
    int rc = avformat_open_input(&fmt, path.c_str(), nullptr, nullptr);
    if (rc < 0) {
        char buf[AV_ERROR_MAX_STRING_SIZE] = {0};
        av_strerror(rc, buf, sizeof(buf));
        err = "无法打开输入: " + std::string(buf);
        return false;
    }
    out.fmt = fmt;

    rc = avformat_find_stream_info(fmt, nullptr);
    if (rc < 0) {
        char buf[AV_ERROR_MAX_STRING_SIZE] = {0};
        av_strerror(rc, buf, sizeof(buf));
        err = "流信息探测失败: " + std::string(buf);
        return false;
    }

    out.formatName = fmt->iformat ? fmt->iformat->name : "unknown";
    out.formatLongName = fmt->iformat && fmt->iformat->long_name
                             ? fmt->iformat->long_name : "";
    if (fmt->duration > 0 && fmt->duration != AV_NOPTS_VALUE)
        out.durationMs = fmt->duration * 1000.0 / AV_TIME_BASE;
    out.fileSize = fmt->pb ? avio_size(fmt->pb) : 0;

    for (unsigned i = 0; i < fmt->nb_streams; ++i) {
        AVStream* st = fmt->streams[i];
        AVCodecParameters* par = st->codecpar;
        ProbeStream ps;
        ps.index = static_cast<int>(i);
        ps.codecpar = par;
        ps.timeBase = st->time_base;
        ps.isVideo = par->codec_type == AVMEDIA_TYPE_VIDEO;
        ps.decoder = avcodec_find_decoder(par->codec_id);
        const AVCodecDescriptor* desc = avcodec_descriptor_get(par->codec_id);
        ps.codecName = desc ? desc->name : "unknown";
        if (ps.decoder) {
            const char* prof = avcodec_profile_name(
                par->codec_id, par->profile);
            if (prof) ps.profileName = prof;
        }
        if (ps.isVideo && out.videoIndex < 0)
            out.videoIndex = ps.index;
        if (par->codec_type == AVMEDIA_TYPE_AUDIO && out.audioIndex < 0)
            out.audioIndex = ps.index;
        out.streams.push_back(ps);
    }
    return true;
}

StreamSummary summarizeStream(const ProbeStream& ps, double durationMs) {
    StreamSummary s;
    s.index = ps.index;
    s.isVideo = ps.isVideo;
    s.codec = ps.codecName;
    s.profile = ps.profileName;
    s.timeBaseNum = ps.timeBase.num;
    s.timeBaseDen = ps.timeBase.den;
    s.durationMs = durationMs;
    if (ps.codecpar) {
        if (ps.isVideo) {
            s.width = ps.codecpar->width;
            s.height = ps.codecpar->height;
        }
        if (ps.codecpar->bit_rate > 0)
            s.bitrateKbps = ps.codecpar->bit_rate / 1000.0;
    }
    return s;
}

} // namespace ma
