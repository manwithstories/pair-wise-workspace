#include "segmenter.hpp"
// 批次 4 实现
namespace md {
void Segmenter::push_video(const MediaPacket&,
                           const std::vector<SilenceInterval>&,
                           std::function<void(std::vector<MediaPacket>, Disposition)>) {}
void Segmenter::finish(const std::vector<SilenceInterval>&,
                       std::function<void(std::vector<MediaPacket>, Disposition)>) {}
bool Segmenter::is_keyframe(const MediaPacket&) const { return false; }
bool Segmenter::silence_long_enough(int64_t, int64_t) const { return false; }
} // namespace md
