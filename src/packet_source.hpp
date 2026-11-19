// packet_source.hpp — 采集流生成与读取（样例数据随代码生成，不引用外部文件）
#pragma once
#include "common.hpp"
#include <string>
#include <vector>
#include <functional>

namespace md {

struct GenerateOptions {
    uint64_t seed = 41;
    double loss = 0.02;        // 丢包率
    double reorder = 0.08;     // 乱序率
    double duplicate = 0.02;   // 重复投递率
    int32_t wrap_mode = 0;     // 0=无回绕, 1=序号回绕, 2=时间戳回绕, 3=两者
    bool truncated = false;    // 截断最后一条记录
    bool aac = false;          // AAC 音频，否则 PCM s16le
    uint32_t duration_ms = 3000;
    uint32_t duplicate_tail_ms = 0; // 末尾重复片段（模拟跨机位重复素材）
};

// 生成采集流并原子写入 out_path；返回写出的记录数
uint32_t generate_capture(const std::string& out_path,
                          const GenerateOptions& opt,
                          PipelineError& err);

// 输入摘要：整文件 sha256 + 头部字段
struct InputSummary {
    std::array<uint8_t, 32> sha256{};
    uint64_t seed = 0;
    uint32_t version = 0;
    uint32_t flags = 0;
    uint32_t record_count = 0;
    uint64_t payload_bytes = 0;
};

// 拉取式包源：顺序读取采集流（乱序/丢包已固化在文件中）
class PacketSource {
public:
    bool open(const std::string& path, PipelineError& err);
    // 取下一条 RTP；文件结束返回 false。RTCP 存入内部队列。
    bool next_rtp(MediaPacket& out, PipelineError& err);
    const std::vector<RtcpReport>& rtcp_reports() const { return rtcp_; }
    const InputSummary& summary() const { return summary_; }
    uint32_t total_records() const { return records_seen_; }

private:
    std::vector<uint8_t> bytes_;
    size_t pos_ = 0;
    std::vector<RtcpReport> rtcp_;
    InputSummary summary_;
    uint32_t records_seen_ = 0;
};

} // namespace md
