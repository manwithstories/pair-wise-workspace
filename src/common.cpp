#include "common.hpp"

namespace md {

const char* err_name(ErrType t) noexcept {
    switch (t) {
        case ErrType::None: return "none";
        case ErrType::TruncatedRecord: return "truncated_record";
        case ErrType::BadMagic: return "bad_magic";
        case ErrType::UnsupportedCodec: return "unsupported_codec";
        case ErrType::MissingKeyframe: return "missing_keyframe";
        case ErrType::ShortSegment: return "short_segment";
        case ErrType::SilenceCrossBoundary: return "silence_cross_boundary";
        case ErrType::UntrustedTime: return "untrusted_time";
        case ErrType::ClockDrift: return "clock_drift";
        case ErrType::DecodeError: return "decode_error";
        case ErrType::DiskWriteFailed: return "disk_write_failed";
        case ErrType::QueueCancelled: return "queue_cancelled";
        case ErrType::CheckpointMismatch: return "checkpoint_mismatch";
        case ErrType::SegmentHashMismatch: return "segment_hash_mismatch";
    }
    return "unknown";
}

const char* codec_name(CodecId c) noexcept {
    switch (c) {
        case CodecId::H264: return "h264";
        case CodecId::PcmS16: return "pcm_s16le";
        case CodecId::Aac: return "aac";
        case CodecId::Unknown: return "unknown";
    }
    return "unknown";
}

const char* disposition_name(Disposition d) noexcept {
    switch (d) {
        case Disposition::Normal: return "normal";
        case Disposition::MissingKeyframe: return "missing_keyframe";
        case Disposition::ShortSegment: return "short_segment";
        case Disposition::SilenceCrossBoundary: return "silence_cross_boundary";
        case Disposition::UntrustedTime: return "untrusted_time";
    }
    return "unknown";
}

} // namespace md
