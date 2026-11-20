#include "mediacore/quality.hpp"

#include <cmath>
#include <sstream>

namespace mediacore {

std::string QualityStats::summaryLine() const {
    std::ostringstream os;
    os << "peak_abs=" << peakAbs << " rms=" << rms
       << " clipped_samples=" << clippedSamples;
    return os.str();
}

QualityAnalyzer::QualityAnalyzer(int sampleRate) : sampleRate_(sampleRate) {}

void QualityAnalyzer::addValidBlock(const AudioBlock& block,
                                    SampleTick expectedStart) {
    if (block.kind != BlockKind::Normal &&
        block.kind != BlockKind::IncompleteTail) {
        return; // 仅有效块参与统计
    }
    if (block.startSample != expectedStart) {
        ++discontinuities_;
    }
    const auto* p =
        reinterpret_cast<const std::int16_t*>(block.interleavedPcm.data());
    const std::size_t n = block.interleavedPcm.size() / sizeof(std::int16_t);
    for (std::size_t i = 0; i < n; ++i) {
        const std::int32_t v = p[i];
        const std::int32_t a = std::abs(v);
        if (a > peak_) {
            peak_ = a;
        }
        if (a >= 32767) { // 满幅即计削波（含 +32767 / -32768）
            ++clipped_;
        }
        sumSq_ += static_cast<std::uint64_t>(v) *
                  static_cast<std::uint64_t>(v);
    }
    samples_ += n;
}

void QualityAnalyzer::addSkippedRange(SampleRange range) {
    if (!range.empty()) {
        skipped_.push_back(range);
        ++discontinuities_;
    }
}

QualityStats QualityAnalyzer::finalize(SampleRange validRange,
                                       std::uint64_t rawBytes,
                                       std::uint64_t actualBytes,
                                       int targetKbps) const {
    QualityStats q;
    q.validRange = validRange;
    q.discontinuities = discontinuities_;
    q.totalSamples = samples_;
    q.clippedSamples = clipped_;
    q.peakAbs = static_cast<std::int16_t>(peak_);
    q.rms = samples_ > 0 ? std::sqrt(static_cast<double>(sumSq_) /
                                     static_cast<double>(samples_))
                         : 0.0;
    q.durationSec = samplesToSeconds(validRange.length(), sampleRate_);
    q.rawBytes = rawBytes;
    q.actualBytes = actualBytes;
    q.targetKbps = targetKbps;
    if (q.durationSec > 0.0) {
        q.actualKbps =
            (static_cast<double>(actualBytes) * 8.0 / q.durationSec) / 1000.0;
        if (targetKbps > 0) {
            q.bitrateErrorPct =
                std::abs(q.actualKbps - static_cast<double>(targetKbps)) /
                static_cast<double>(targetKbps) * 100.0;
        }
    }
    return q;
}

} // namespace mediacore
