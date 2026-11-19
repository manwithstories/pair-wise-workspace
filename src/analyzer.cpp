#include "analyzer.hpp"
#include "damage_locator.hpp"
#include "fixture_gen.hpp"
#include "gop_analyzer.hpp"
#include "media_clock.hpp"
#include "probe.hpp"
#include "sync_recovery.hpp"

#include <algorithm>
#include <chrono>
#include <thread>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
}

namespace ma {
namespace {

double nowMs() {
    using namespace std::chrono;
    return duration_cast<duration<double, std::milli>>(
        steady_clock::now().time_since_epoch()).count();
}

// 队列负载：按包实际字节计账（保证内存上限真正约束队列数据量）。
struct QueuedPacket {
    AVPacket* pkt = nullptr;
    int64_t sequence = 0;
    double enqueueMs = 0.0;
};

} // namespace

bool analyzeFile(const AnalyzeOptions& opt, AuditResult& out,
                 std::string& err) {
    av_log_set_level(AV_LOG_QUIET); // 解码错误由本工具分类输出，抑制库噪音
    out = AuditResult{};
    out.file = opt.input;

    ProbeInfo probe;
    if (!probeOpen(opt.input, probe, err)) {
        out.ok = false;
        out.fatalError = err;
        return true; // 输入问题 → 退出码 2，由调用方区分
    }
    out.containerFormat = probe.formatName;
    out.durationMs = probe.durationMs;
    out.fileSizeBytes = probe.fileSize;
    out.hasAudio = probe.audioIndex >= 0;
    for (const auto& ps : probe.streams)
        out.streams.push_back(summarizeStream(ps, probe.durationMs));

    // 视频解码器（用于损坏包检测）；音频保持容器级解析，避免无谓占用。
    const ProbeStream* vps = nullptr;
    const ProbeStream* aps = nullptr;
    for (const auto& ps : probe.streams) {
        if (ps.index == probe.videoIndex) vps = &ps;
        if (ps.index == probe.audioIndex) aps = &ps;
    }
    AVCodecContext* vdec = nullptr;
    if (vps) {
        vdec = avcodec_alloc_context3(vps->decoder);
        avcodec_parameters_to_context(vdec, vps->codecpar);
        if (avcodec_open2(vdec, vps->decoder, nullptr) < 0) {
            avcodec_free_context(&vdec);
            err = "视频解码器打开失败";
            return false;
        }
    }

    StreamClock vClock(1LL << 33), aClock(1LL << 33);
    StreamClock vPtsClock(1LL << 33, 3.0, /*wrapOnly=*/true);
    GopAnalyzer gop(vps ? vps->timeBase.num : 1,
                    vps ? vps->timeBase.den : 25);
    DamageLocator damage(8);
    SyncRecovery sync;
    PipelineMetrics metrics;
    PipelineErrorState perr;

    // 队列字节预算取内存上限的 1/4（其余留给解码器/帧/表结构）。
    const size_t queueBytes =
        std::max<size_t>(64 * 1024,
                         opt.memoryMiB * 1024 * 1024 / 4);
    // 条目上限随内存预算缩放：按平均 8KiB/包估算，并与显式配置取小，
    // 确保小内存时条目约束也收紧，而非固定 256 先于字节约束触发。
    const size_t budgetItems =
        std::max<size_t>(8, queueBytes / (8 * 1024));
    const size_t itemLimit =
        std::min<size_t>(opt.queueItemLimit, budgetItems);
    BoundedQueue<QueuedPacket> queue(queueBytes, itemLimit);

    int64_t seq = 0;
    int64_t vFrameNo = 0, aFrameNo = 0, aSampleCursor = 0;
    bool vBaseSet = false, aBaseSet = false;
    int64_t vBaseDts = 0, aBaseDts = 0;
    // 视频典型帧间隔（EWMA），用于在公共理想时间轴上重建视频理想时刻。
    double vIntervalEwma = 0.0, prevVideoMs = 0.0;
    double firstPacketMs = 0.0, lastResultMs = 0.0;
    std::atomic<bool> truncated{false};
    std::atomic<int64_t> truncSeq{-1};
    std::atomic<int64_t> truncPos{-1};
    const double deadlineMs = opt.timeoutMs > 0
        ? nowMs() + opt.timeoutMs : 0.0;

    // ---- 生产者：解复用，读包错误/截断仅做原子记录，由消费者统一记账 ----
    std::thread producer([&] {
        AVPacket* raw = av_packet_alloc();
        bool first = true;
        while (!perr.failed()) {
            if (deadlineMs > 0.0 && nowMs() > deadlineMs) {
                perr.fail("分析超时");
                break;
            }
            int rc = av_read_frame(probe.fmt, raw);
            if (rc < 0) {
                if (rc != AVERROR_EOF && rc != AVERROR(EAGAIN)) {
                    truncated.store(true);
                    truncSeq.store(seq);
                    truncPos.store(raw->pos);
                }
                break;
            }
            if (first) { firstPacketMs = nowMs(); first = false; }
            QueuedPacket qp{raw, seq, nowMs()};
            const size_t bytes = sizeof(QueuedPacket) +
                                 static_cast<size_t>(raw->size);
            if (!queue.push(qp, bytes, perr.token(),
                            &metrics.backpressureEvents)) {
                av_packet_free(&raw); // 取消路径：释放未入队包，避免泄漏
                break;
            }
            raw = av_packet_alloc();
            ++seq;
        }
        av_packet_free(&raw);
        queue.close();
    });

    // ---- 消费者：解码、时钟展开、GOP、定位、同步、延迟采样 ----
    AVFrame* frame = av_frame_alloc();
    while (true) {
        if (deadlineMs > 0.0 && nowMs() > deadlineMs) perr.fail("分析超时");
        auto q = queue.pop(perr.token());
        if (!q) break;
        QueuedPacket qp = std::move(*q);
        AVPacket* pkt = qp.pkt;
        const bool isVideo = pkt->stream_index == probe.videoIndex;
        const bool isAudio = pkt->stream_index == probe.audioIndex;
        const AVRational tb = probe.fmt->streams[pkt->stream_index]->time_base;
        const int64_t pktSeq = qp.sequence;
        // DTS 时钟负责解码顺序排序；PTS 独立时钟负责呈现时间回绕检测。
        const int64_t rawDts =
            pkt->dts == AV_NOPTS_VALUE
                ? (pkt->pts == AV_NOPTS_VALUE ? 0 : pkt->pts) : pkt->dts;
        const int64_t rawPts =
            pkt->pts == AV_NOPTS_VALUE ? rawDts : pkt->pts;

        bool decodeOk = true;
        if (isVideo && vdec) {
            int rc = avcodec_send_packet(vdec, pkt);
            if (rc < 0) {
                decodeOk = false;
            } else while (true) {
                rc = avcodec_receive_frame(vdec, frame);
                if (rc == AVERROR(EAGAIN) || rc == AVERROR_EOF) break;
                if (rc < 0) { decodeOk = false; break; }
                av_frame_unref(frame);
            }
        }

        if (isVideo) {
            if (!vBaseSet) { vBaseSet = true; vBaseDts = rawDts; }
            // DTS 时钟喂原始 DTS 检测解码序异常。
            const int64_t expanded = vClock.feed(rawDts);
            // PTS 时钟独立喂原始 PTS，捕获 ctts 注入的呈现时间回绕。
            vPtsClock.feed(rawPts);
            const int64_t normDts = rawDts - vBaseDts;
            // 毫秒减去首包原始基准，得到从 0 起的呈现时间。
            const double actualMs = StreamClock::toMs(
                expanded - vClock.events().front().raw, tb.num, tb.den);
            if (vFrameNo > 0) {
                const double gap = actualMs - prevVideoMs;
                if (gap > 0.0)
                    vIntervalEwma = vIntervalEwma == 0.0
                        ? gap
                        : 0.9 * vIntervalEwma + 0.1 * gap;
            }
            prevVideoMs = actualMs;
            // 视频理想时间直接采用实际 DTS 毫秒：VFR 的帧间隔变化属于
            // 正常节奏而非同步错误，不应进入音画偏移。
            const double idealMs = actualMs;
            sync.addVideo(idealMs, actualMs);
            PacketInfo pi;
            pi.streamIndex = pkt->stream_index;
            pi.pts = normDts;
            pi.pos = pkt->pos;
            pi.keyframe = (pkt->flags & AV_PKT_FLAG_KEY) != 0;
            gop.feed(pi);
            damage.onPacket(decodeOk, pktSeq, pkt->pos, actualMs);
            ++vFrameNo;
        } else if (isAudio && aps) {
            if (!aBaseSet) { aBaseSet = true; aBaseDts = rawDts; }
            const int64_t expanded = aClock.feed(rawDts);
            const double actualMs = StreamClock::toMs(
                expanded - aClock.events().front().raw, tb.num, tb.den);
            // 理想时间按名义采样率从 0 累加（与实际用同一参考零点），
            // 漂移文件中 actual 步长略大 → 两者斜率差即 drift。
            // 理想时间按"名义"每包 1024 样本从 0 累加，不使用实际 duration
            // （漂移文件的实际 duration 已含漂移，会抵消斜率差）。
            constexpr int64_t nominalFrameSamples = 1024;
            const double idealMs = 1000.0 *
                static_cast<double>(aSampleCursor) /
                static_cast<double>(aps->codecpar->sample_rate);
            aSampleCursor += nominalFrameSamples;
            sync.addAudio(idealMs, actualMs);
            damage.onPacket(true, pktSeq, pkt->pos, actualMs);
            ++aFrameNo;
        }
        metrics.latencyMs.push_back(nowMs() - qp.enqueueMs);
        lastResultMs = nowMs();
        av_packet_free(&pkt);
    }
    av_frame_free(&frame);
    producer.join();
    if (truncated.load()) {
        damage.onError({ErrorCategory::Truncated, truncSeq.load(),
                        truncPos.load(), 0.0, "av_read_frame 提前失败"});
    }
    if (vdec) avcodec_free_context(&vdec);

    // ---- 时间戳异常落表（保留原始值供复核）----
    for (const auto& e : vClock.events())
        if (e.kind != TsKind::Normal)
            out.tsAnomalies.push_back(
                {probe.videoIndex, e.kind, e.raw, e.expanded, -1});
    // PTS 呈现时间回绕/倒退（DTS 正常但 ctts 异常的场景）。
    for (const auto& e : vPtsClock.events())
        if (e.kind != TsKind::Normal)
            out.tsAnomalies.push_back(
                {probe.videoIndex, e.kind, e.raw, e.expanded, -1});
    for (const auto& e : aClock.events())
        if (e.kind != TsKind::Normal)
            out.tsAnomalies.push_back(
                {probe.audioIndex, e.kind, e.raw, e.expanded, -1});

    damage.finish(truncated.load());
    out.damageRanges = damage.ranges();
    out.recovery = damage.recovery();

    const GopReport gr = gop.report();
    out.gopIntervalMeanMs = gr.intervalMeanMs;
    out.gopIntervalP95Ms = gr.intervalP95Ms;
    out.gopAnomalies = gr.anomalies;
    if (vps && !gr.frameIntervalsMs.empty()) {
        IntervalStats is;
        auto samples = gr.frameIntervalsMs;
        is.finalize(samples);
        auto& vs = *std::find_if(out.streams.begin(), out.streams.end(),
            [&](const StreamSummary& s) { return s.index == vps->index; });
        vs.frameIntervalMinMs = is.minMs;
        vs.frameIntervalMaxMs = is.maxMs;
        vs.frameIntervalMeanMs = is.meanMs;
        vs.frameIntervalP95Ms = is.p95Ms;
        vs.packetCount = vFrameNo;
        vs.keyframeCount = gr.keyframeCount;
    }
    if (aps) {
        auto& as = *std::find_if(out.streams.begin(), out.streams.end(),
            [&](const StreamSummary& s) { return s.index == aps->index; });
        as.packetCount = aFrameNo;
    }

    if (out.hasAudio) {
        const SyncReport sr = sync.estimate();
        out.avSyncErrorBeforeMs = sr.errorBeforeP95Ms;
        out.avSyncErrorAfterMs = sr.errorAfterP95Ms;
        out.driftPpm = sr.driftPpm;
    }
    // 验收口径：端到端延迟覆盖首包读取至末包结果落盘。
    out.pipelineLatencyP95Ms =
        lastResultMs > 0 ? (lastResultMs - firstPacketMs) : 0.0;
    (void)metrics.latencyP95Ms(); // 队列等待 P95 保留在内部指标中
    metrics.peakRssMiB = currentRssMiB();
    out.peakMemoryMiB = metrics.peakRssMiB;
    out.backpressureEvents = metrics.backpressureEvents;
    out.cancelled = perr.failed();
    out.timeout = perr.failed() && perr.message() == "分析超时";
    if (out.timeout) out.fatalError = "分析超时";
    out.ok = true;
    return true;
}

} // namespace ma
