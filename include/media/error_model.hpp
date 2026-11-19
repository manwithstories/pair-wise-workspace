// 错误模型：重复、乱序、丢失、损坏与不确定性的统一分类。
// 该模型由缓冲、恢复、同步与清单模块共用，所有不可换算/不可恢复项
// 都必须带上源轨道与错误类型记录，保证结果可解释。
#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace media {

enum class ErrorKind {
    Duplicate,             // 序号或时间戳完全重复的包
    OutOfOrder,            // 乱序到达但落在重排窗口内
    LateOutsideWindow,     // 迟到且落在窗口外，不得回退已提交检查点
    Lost,                  // 序号缺口判定为丢失
    Recovered,             // 经前向冗余恢复
    Unrecoverable,         // 冗余不足，形成明确缺口
    CorruptRecord,         // 字幕/索引记录损坏
    TruncatedRecord,       // 记录被截断
    TimestampWrap,         // 时间戳日界回绕（信息性）
    DuplicateTimestamp,    // 重复时间戳（信息性，需保留先后序）
    UnconvertibleTimecode, // 帧率/时钟制式不支持，无法换算
    CheckpointCorrupt,     // 检查点校验失败
    WriteFailure,          // 磁盘写入失败
    Uncertain,             // 无法归类的其他不确定性
};

const char* to_string(ErrorKind kind);

struct ErrorRecord {
    ErrorKind kind = ErrorKind::Uncertain;
    std::string source_track; // 来源轨道，如 "audio/eng"、"subtitle/srt"
    int64_t sequence = -1;    // 相关包序号或记录序号，未知为 -1
    int64_t line = -1;        // 文本记录行号（字幕解析用），未知为 -1
    int64_t offset = -1;      // 字节偏移（字幕解析用），未知为 -1
    std::string detail;
};

// 计数器与明细记录；明细有界，防止突发错误撑爆内存。
class ErrorModel {
  public:
    static constexpr size_t kMaxRecords = 4096;

    void record(ErrorRecord rec);
    void count(ErrorKind kind) { record({kind, "", -1, -1, -1, ""}); }

    uint64_t count_of(ErrorKind kind) const;
    const std::vector<ErrorRecord>& records() const { return records_; }
    bool truncated() const { return dropped_ > 0; }
    uint64_t dropped_records() const { return dropped_; }

  private:
    uint64_t counts_[static_cast<size_t>(ErrorKind::Uncertain) + 1] = {};
    std::vector<ErrorRecord> records_;
    uint64_t dropped_ = 0;
};

} // namespace media
