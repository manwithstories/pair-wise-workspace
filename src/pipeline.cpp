#include "pipeline.hpp"
// 批次 8 实现
namespace md {
ExitCode run_pipeline(const ProcessConfig&, const PipelineHooks&,
                      PipelineResult& result) {
    result.recovered_stage = "none";
    return ExitCode::Ok;
}
} // namespace md
