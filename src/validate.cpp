#include "validate.h"

#include <algorithm>
#include <cmath>
#include <map>
#include <set>

namespace fmp4 {

namespace {

void collectSequence(const std::vector<FragmentRecord>& fragments, SequenceReport& report) {
    std::map<uint32_t, uint64_t> counts;
    uint32_t maximum = 0;
    uint32_t previous = 0;
    bool have_previous = false;
    std::set<uint32_t> seen_out_of_order;
    for (const FragmentRecord& fragment : fragments) {
        ++counts[fragment.sequence];
        maximum = std::max(maximum, fragment.sequence);
        if (have_previous && fragment.sequence < previous &&
            seen_out_of_order.insert(fragment.sequence).second) {
            report.out_of_order.push_back(fragment.sequence);
        }
        if (!have_previous || fragment.sequence > previous) {
            previous = fragment.sequence;
        }
        have_previous = true;
    }
    for (const auto& entry : counts) {
        if (entry.second > 1) {
            report.duplicate.push_back(entry.first);
        }
    }
    if (!fragments.empty()) {
        for (uint32_t sequence = 1; sequence <= maximum; ++sequence) {
            if (counts.find(sequence) == counts.end()) {
                report.missing.push_back(sequence);
                MissingFragment detail;
                detail.sequence = sequence;
                for (const FragmentRecord& fragment : fragments) {
                    if (fragment.sequence < sequence &&
                        (!detail.has_previous || fragment.sequence > detail.previous_sequence)) {
                        detail.has_previous = true;
                        detail.previous_sequence = fragment.sequence;
                        detail.previous_moof_offset = fragment.moof_offset;
                    }
                    if (fragment.sequence > sequence &&
                        (!detail.has_next || fragment.sequence < detail.next_sequence)) {
                        detail.has_next = true;
                        detail.next_sequence = fragment.sequence;
                        detail.next_moof_offset = fragment.moof_offset;
                    }
                }
                report.missing_details.push_back(detail);
            }
        }
    }
    report.in_order = report.out_of_order.empty();
    report.contiguous = !fragments.empty() && report.in_order && report.duplicate.empty() &&
                        report.missing.empty();
}

void collectTiming(const InitSegment& init, const FragmentIndex& index,
                   std::vector<TrackReport>& reports, bool& monotonic) {
    reports.clear();
    monotonic = true;
    for (const TrackInit& track : init.tracks) {
        TrackReport report;
        report.track_id = track.track_id;
        report.timescale = track.timescale;
        report.codec = track.codec;
        report.has_trex = track.has_trex;
        report.default_sample_description_index = track.default_sample_description_index;
        report.default_sample_duration = track.default_sample_duration;
        report.default_sample_size = track.default_sample_size;
        report.default_sample_flags = track.default_sample_flags;

        std::vector<const TrafRecord*> occurrences;
        for (const FragmentRecord& fragment : index.fragments()) {
            for (const TrafRecord& traf : fragment.trafs) {
                if (traf.tfhd.track_id == track.track_id) {
                    occurrences.push_back(&traf);
                    report.sample_count += traf.sampleCount();
                }
            }
        }
        report.fragment_occurrences = occurrences.size();

        long double sum = 0.0L;
        long double maximum = 0.0L;
        uint64_t baseline = 0;
        uint64_t samples_before = 0;
        uint64_t previous_tfdt = 0;
        size_t comparison_count = 0;
        for (size_t i = 0; i < occurrences.size(); ++i) {
            const TrafRecord& traf = *occurrences[i];
            if (i == 0) {
                baseline = traf.tfdt_base;
                previous_tfdt = traf.tfdt_base;
            } else {
                if (traf.tfdt_base < previous_tfdt) {
                    monotonic = false;
                }
                previous_tfdt = traf.tfdt_base;
                const long double expected =
                    static_cast<long double>(baseline) +
                    static_cast<long double>(track.default_sample_duration) * samples_before;
                const long double deviation =
                    std::fabs(static_cast<long double>(traf.tfdt_base) - expected);
                sum += deviation;
                maximum = std::max(maximum, deviation);
            }
            samples_before += traf.sampleCount();
            ++comparison_count;
        }

        report.deviation_max_ticks = static_cast<double>(maximum);
        report.deviation_mean_ticks =
            comparison_count > 1 ? static_cast<double>(sum / comparison_count) : 0.0;
        if (track.timescale > 0) {
            const double scale = 1000.0 / static_cast<double>(track.timescale);
            report.deviation_max_ms = report.deviation_max_ticks * scale;
            report.deviation_mean_ms = report.deviation_mean_ticks * scale;
        }
        reports.push_back(std::move(report));
    }
}

}  // namespace

ValidationResult validateMedia(const BoxReader& reader, const BoxTree& tree,
                               const InitSegment* expected_init) {
    ValidationResult result;
    result.init = InitSegment::parse(reader, tree);
    const FragmentIndex index = FragmentIndex::parse(reader, tree, &result.init);
    result.fragment_count = index.fragments().size();
    collectSequence(index.fragments(), result.sequence);
    collectTiming(result.init, index, result.tracks, result.timing_monotonic);

    if (expected_init != nullptr) {
        result.init_checked = true;
        result.init_diffs = compareInitSegments(*expected_init, result.init);
        result.init_compatible = result.init_diffs.empty();
    }
    result.passed = result.fragment_count > 0 && result.sequence.contiguous &&
                    result.init_compatible && result.timing_monotonic;
    return result;
}

}  // namespace fmp4

