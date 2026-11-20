#include "mediacore/wav_demuxer.hpp"

// 批次骨架：完整 RIFF 解析/哨兵/扫描逻辑在下一批落地
namespace mediacore {

WavDemuxer::~WavDemuxer() { close(); }

void WavDemuxer::close() {
    if (file_.is_open()) file_.close();
    eof_ = true;
}

Result<void> WavDemuxer::open(const std::string& path,
                              const DemuxConfig& config) {
    config_ = config;
    file_.open(path, std::ios::binary);
    if (!file_.is_open()) {
        return MediaError{MediaErrorCode::InvalidArgument,
                          "cannot open input: " + path, false};
    }
    return Result<void>{};
}

bool WavDemuxer::nextBlock(AudioBlock&, MediaError&) { return false; }

} // namespace mediacore
