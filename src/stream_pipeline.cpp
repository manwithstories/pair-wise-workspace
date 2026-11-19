#include "media/stream_pipeline.hpp"
// 队列本体为头文件模板（BoundedQueue）。本文件预留流水线编排辅助，
// 完整实现随第四批提交。
namespace media {
struct PipelineAnchor { int placeholder = 0; };
} // namespace media
