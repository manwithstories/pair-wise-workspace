// mediacore/error.hpp — 跨模块错误模型
//
// 所有模块（解复用、映射、转封装、清单、质量统计）共用同一错误码表与
// Result<T> 返回约定。错误分为：
//   - 可恢复 (recoverable=true)：跳过受影响区间后仍可产出有效输出；
//   - 不可恢复：当前文件必须拒绝，批次流程可继续后续文件。
#pragma once

#include <stdexcept>
#include <string>
#include <utility>
#include <variant>

namespace mediacore {

enum class MediaErrorCode {
    Ok = 0,
    InvalidArgument,     // 参数 / 配置非法（路径、映射规则等）
    PathTraversal,       // 路径越出允许的工作目录
    UnsupportedFormat,   // 不支持的采样格式 / 封装 / 声道布局
    TruncatedHeader,     // 头部截断，无法识别输入
    CorruptStream,       // 中段数据损坏（可恢复，记录损坏区间）
    IncompleteTail,      // 尾部不完整样本（可恢复，丢弃半样本）
    MissingIndex,        // 索引缺失，已顺序扫描重建（信息性）
    ScanLimitExceeded,   // 顺序扫描超过配置上限，无法重建索引
    DuplicateBlock,      // 块重复（可恢复，跳过重复）
    TimestampGap,        // 时间戳缺口（可恢复，记录跳过区间）
    MappingOutOfRange,   // 映射引用不存在的输入声道
    MappingDuplicate,    // 同一输入声道被重复占用
    MappingIncompatible, // 输出轨道与声道布局不兼容
    WriteFailed,         // 输出写入失败
    Cancelled,           // 上游取消
};

struct MediaError {
    MediaErrorCode code = MediaErrorCode::Ok;
    std::string message;
    bool recoverable = false;

    MediaError() = default;
    MediaError(MediaErrorCode c, std::string msg, bool canRecover)
        : code(c), message(std::move(msg)), recoverable(canRecover) {}

    bool ok() const noexcept { return code == MediaErrorCode::Ok; }
    explicit operator bool() const noexcept { return ok(); }
};

// 错误码的稳定短名（清单与终端输出使用，保证可脚本解析）
const char* errorCodeName(MediaErrorCode code) noexcept;

// 供需要抛异常的场景（如线程内、构造期）使用
class MediaException : public std::runtime_error {
public:
    explicit MediaException(MediaError err)
        : std::runtime_error(err.message), error_(std::move(err)) {}
    MediaException(MediaErrorCode code, std::string msg, bool recoverable)
        : std::runtime_error(msg),
          error_(code, std::move(msg), recoverable) {}

    const MediaError& error() const noexcept { return error_; }

private:
    MediaError error_;
};

// 轻量 Result，避免异常跨线程边界
template <typename T>
class Result {
public:
    Result(T value) : storage_(std::move(value)) {}
    Result(MediaError error) : storage_(std::move(error)) {}

    bool ok() const noexcept {
        return std::holds_alternative<T>(storage_);
    }
    explicit operator bool() const noexcept { return ok(); }

    const T& value() const& { return std::get<T>(storage_); }
    T& value() & { return std::get<T>(storage_); }
    T valueOr(T fallback) const {
        return ok() ? std::get<T>(storage_) : std::move(fallback);
    }
    const MediaError& error() const& { return std::get<MediaError>(storage_); }

private:
    std::variant<T, MediaError> storage_;
};

} // namespace mediacore
