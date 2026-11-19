#include "media/stream_pipeline.hpp"
#include <thread>
#include <utility>

namespace media {

PipelineStats runPipeline(PacketQueue& q,
                          const std::vector<MediaPacket>& source,
                          const std::function<TimeError(MediaPacket&)>& consume) {
  PipelineStats st;
  std::thread producer([&] {
    for (const auto& pkt : source) {
      if (q.push(pkt) == PushResult::Cancelled) {
        if (st.error == TimeError::None) st.error = TimeError::QueueCancelled;
        return;
      }
      ++st.produced;
    }
    q.close();
  });

  while (true) {
    auto [res, maybePkt] = q.pop();
    if (res == PopResult::Cancelled) {
      st.error = q.cancelReason();
      break;
    }
    if (res == PopResult::Closed) break;
    MediaPacket pkt = std::move(*maybePkt);
    TimeError e = consume(pkt);
    ++st.consumed;
    if (isFatal(e)) {
      st.error = e;
      st.firstBadPacket = pkt.globalIndex;
      q.cancel(e); // 释放可能阻塞在 push 上的生产者
      break;
    }
  }
  producer.join();
  st.peak = q.peak();
  return st;
}
} // namespace media
