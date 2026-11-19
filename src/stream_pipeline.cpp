#include "stream_pipeline.hpp"
#include "media_clock.hpp"

#include <algorithm>

#if defined(__APPLE__)
#include <mach/mach.h>
#endif

namespace ma {

double PipelineMetrics::latencyP95Ms() const {
    auto v = latencyMs;
    return percentileMs(v, 95.0);
}

double currentRssMiB() {
#if defined(__APPLE__)
    mach_task_basic_info_data_t info{};
    mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
    if (task_info(mach_task_self(), MACH_TASK_BASIC_INFO,
                  reinterpret_cast<task_info_t>(&info), &count) == KERN_SUCCESS) {
        return static_cast<double>(info.resident_size) / (1024.0 * 1024.0);
    }
#endif
    return 0.0;
}

} // namespace ma
