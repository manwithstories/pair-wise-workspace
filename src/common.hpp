// common.hpp — 共享类型、错误模型与常量
#pragma once
#include <cstdint>
#include <string>
#include <vector>
#include <array>

namespace md {

// 程序退出码（见验收约定）
enum class ExitCode : int {
    Ok = 0,
    BadArgs = 2,
    CorruptInput = 3,
    IncompatibleCheckpoint = 4,
    IoError = 5,
};

// 可追溯错误类型，所有错误均携带 stage 与 seq
enum class ErrType {
    None,
    TruncatedRecord,     // 记录长度不足/文件截断
    BadMagic,            // 不是本工具生成的采集流
    UnsupportedCodec,    // 非 H.264 / PCM / AAC
    MissingKeyframe,     // 缓存窗口内无关键帧
    ShortSegment,        // 片段短于最小时长
    SilenceCrossBoundary,// 静音区间跨越包边界
    UntrustedTime,       // 不可恢复缺口，时间不可信
    ClockDrift,          // RTCP 漂移超限
    DecodeError,         // 解码失败（损坏帧，不计静音）
    DiskWriteFailed,     // 磁盘写入失败
    IoReadFailed,        // 输入读取失败
    QueueCancelled,      // 有界队列被取消
    CheckpointMismatch,  // 检查点与输入/参数不兼容
    SegmentHashMismatch, // 已落盘片段哈希校验失败
};

const char* err_name(ErrType t) noexcept;

enum class CodecId : uint8_t { H264 = 0, PcmS16 = 1, Aac = 2, Unknown = 255 };
const char* codec_name(CodecId c) noexcept;

// RTP 载荷类型（采集流内自定义编号）
constexpr uint8_t PT_H264 = 96;
constexpr uint8_t PT_PCM  = 97;
constexpr uint8_t PT_AAC  = 98;

// 统一整数时间基准：微秒
constexpr int64_t UNIFIED_TICK_US = 1;
constexpr uint64_t SEQ_MOD = 1ULL << 16;     // RTP 16 位序号模数
constexpr uint64_t TS_MOD  = 1ULL << 32;     // RTP 32 位时间戳模数

// 采集流记录类型
enum class RecordKind : uint8_t { Rtp = 1, Rtcp = 2 };

#pragma pack(push, 1)
struct CaptureHeader {
    char     magic[8];      // "MDCAP01\n"
    uint32_t version;       // 采集格式版本
    uint32_t flags;         // bit0: timestamp-wrap 样例
    uint64_t seed;
    uint32_t record_count;
    uint64_t payload_bytes;
};
struct RecordHeader {
    uint8_t  kind;
    uint16_t len;           // 记录体长度（不含本头）
};
struct RtpRecord {
    uint8_t  pt;
    uint16_t seq;
    uint32_t ts;
    uint32_t ssrc;
    uint8_t  marker;
    uint8_t  codec;        // CodecId
    uint16_t clock_rate;   // 低 16 位（采样率/90k 固定）
    uint8_t  payload[1];   // 柔性数组
};
struct RtcpRecord {
    uint32_t ssrc;
    uint32_t rtp_ts;
    uint64_t ntp_us;       // 发送者报告对应的绝对微秒时钟
    uint8_t  payload[1];
};
#pragma pack(pop)

// 归一化后的媒体包（时间轴阶段产物）
struct MediaPacket {
    uint16_t seq = 0;
    uint32_t rtp_ts = 0;
    uint32_t ssrc = 0;
    CodecId codec = CodecId::Unknown;
    uint32_t clock_rate = 90000;
    bool marker = false;
    bool duplicate = false;    // 重复投递（同序号）
    bool untrusted = false;    // 处于时间不可信区间
    int64_t unified_us = 0;    // 统一时间基准上的时间戳
    std::vector<uint8_t> payload;
};

struct RtcpReport {
    uint32_t ssrc = 0;
    uint32_t rtp_ts = 0;
    int64_t ntp_us = 0;
};

// 处理配置（参数版本纳入检查点绑定）
constexpr uint32_t PARAM_VERSION = 1;
struct ProcessConfig {
    std::string input_path;
    std::string checkpoint_path;
    std::string out_path;
    std::string work_dir;       // 片段临时目录（默认 out 同目录）
    uint32_t queue_capacity = 8;
    uint32_t reorder_window = 16;
    uint32_t max_frame_cache = 64;
    int64_t  min_segment_us = 200'000;   // 最小时长 200ms
    int64_t  silence_split_us = 300'000; // 静音超此长才切分
    uint32_t silence_window_ms = 20;
    uint32_t silence_overlap_ms = 10;
    int32_t  silence_threshold_milli = 50; // RMS 阈值（满幅千分比）
    int64_t  drift_limit_us = 50'000;     // RTCP 漂移上限
    bool     resume = false;
    bool     aac = false;
};

// 静音区间（统一时间基准，微秒）
struct SilenceInterval { int64_t begin_us = 0; int64_t end_us = 0; };

// 片段处置结果
enum class Disposition : uint8_t {
    Normal = 0,
    MissingKeyframe,
    ShortSegment,
    SilenceCrossBoundary,
    UntrustedTime,
};
const char* disposition_name(Disposition d) noexcept;

struct SegmentReport {
    uint32_t id = 0;
    std::string sha256;              // 64 位十六进制
    std::string sample_summary;      // 规范化采样摘要（碰撞复核）
    int64_t begin_us = 0;
    int64_t end_us = 0;
    uint32_t packets = 0;
    uint32_t keyframes = 0;
    bool keyframe_indexed = false;   // 是否以关键帧起始
    Disposition disposition = Disposition::Normal;
    bool duplicate_of_other = false;
    uint32_t duplicate_of = 0;
    std::vector<SilenceInterval> silence;
};

struct PipelineError {
    ErrType type = ErrType::None;
    std::string stage;
    uint32_t seq = 0;
    std::string message;
};

struct PipelineResult {
    double roundtrip_rate = 0.0;
    double keyframe_hit_rate = 0.0;
    uint32_t segment_count = 0;
    uint32_t duplicate_count = 0;
    uint32_t peak_queue_items = 0;
    std::string recovered_stage;       // 无恢复时为 "none"
    std::vector<SegmentReport> segments;
    std::vector<PipelineError> errors;
    std::array<uint8_t, 32> input_sha256{};
    uint32_t source_packets = 0;
    uint32_t delivered_packets = 0;
    uint32_t lost_packets = 0;
    uint32_t duplicate_packets = 0;
    uint32_t untrusted_spans = 0;
};

// 阶段名称常量
namespace stage {
constexpr const char* Source   = "source";
constexpr const char* Timeline = "timeline";
constexpr const char* Segment  = "segment";
constexpr const char* Silence  = "silence";
constexpr const char* Fingerprint = "fingerprint";
constexpr const char* Checkpoint = "checkpoint";
constexpr const char* Queue    = "queue";
}

} // namespace md
