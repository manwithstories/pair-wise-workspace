// mediacore/remux_muxer.hpp — FFmpeg 封装输出（无解码/重编码）
//
// 输入 MappedBlock 的 s16 PCM 原样写入容器，codec 选用容器原生的
// PCM 编码标识（WAV -> pcm_s16le，Matroska -> pcm_s16le），
// 样本字节不经过任何 codec 处理，保证样本不变。
// 输出先写 .part 临时文件，close(commit=true) 成功后才原子改名，
// 写入失败时临时文件被清理，不会出现看似成功的清单。
#pragma once

#include <string>
#include <vector>

extern "C" {
#include <libavformat/avformat.h>
}

#include "mediacore/error.hpp"
#include "mediacore/track_mapper.hpp"
#include "mediacore/time.hpp"

namespace mediacore {

enum class Container {
    Wav,
    Matroska,
};

const char* containerName(Container c) noexcept;
const char* containerExtension(Container c) noexcept;
// 按文件后缀猜测容器，无法识别返回错误
Result<Container> containerFromPath(const std::string& path);

class RemuxMuxer {
public:
    RemuxMuxer() = default;
    ~RemuxMuxer();

    RemuxMuxer(const RemuxMuxer&) = delete;
    RemuxMuxer& operator=(const RemuxMuxer&) = delete;

    // 打开输出；tracks 给出每轨道的名称与格式（采样率/声道数）
    Result<void> open(const std::string& finalPath, Container container,
                      const std::vector<OutputTrack>& tracks,
                      int sampleRate);

    // 写入一个已映射块；样本字节直通。返回写出字节计数。
    Result<std::uint64_t> writeBlock(const MappedBlock& block);

    // 提交（原子改名）或放弃（清理临时文件）
    Result<void> commit();
    void abort();

    std::uint64_t bytesWritten() const noexcept { return payloadBytes_; }
    const std::string& finalPath() const noexcept { return finalPath_; }

private:
    AVFormatContext* fmt_ = nullptr;
    std::vector<int> streamIndex_;        // 轨道名顺序 -> AVStream 索引
    std::vector<std::string> trackNames_; // 与 streamIndex_ 平行
    std::vector<SampleTick> streamNextSample_;
    std::string finalPath_;
    std::string tempPath_;
    std::uint64_t payloadBytes_ = 0;
    bool headerWritten_ = false;
    bool committed_ = false;
};

} // namespace mediacore
