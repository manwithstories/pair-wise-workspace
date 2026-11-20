#include "media/types.hpp"

namespace mediaqc {

std::string_view to_string(TrackKind kind) {
  switch (kind) {
    case TrackKind::Video: return "video";
    case TrackKind::Audio: return "audio";
    case TrackKind::Data: return "data";
  }
  return "data";
}

std::optional<TrackKind> parse_track_kind(std::string_view text) {
  if (text == "video") return TrackKind::Video;
  if (text == "audio") return TrackKind::Audio;
  if (text == "data") return TrackKind::Data;
  return std::nullopt;
}

std::string_view to_string(PixelFormatId id) {
  switch (id) {
    case PixelFormatId::kYuv420p: return "yuv420p";
    case PixelFormatId::kYuv422p: return "yuv422p";
    case PixelFormatId::kYuv444p: return "yuv444p";
    case PixelFormatId::kGray: return "gray";
    case PixelFormatId::kRgb24: return "rgb24";
    case PixelFormatId::kUnknown: break;
  }
  return "unknown";
}

std::optional<PixelFormatId> parse_pixel_format(std::string_view text) {
  if (text == "yuv420p") return PixelFormatId::kYuv420p;
  if (text == "yuv422p") return PixelFormatId::kYuv422p;
  if (text == "yuv444p") return PixelFormatId::kYuv444p;
  if (text == "gray") return PixelFormatId::kGray;
  if (text == "rgb24") return PixelFormatId::kRgb24;
  return std::nullopt;
}

RepairMask operator|(RepairMask a, RepairReason b) {
  return a | static_cast<RepairMask>(b);
}

bool has_reason(RepairMask mask, RepairReason reason) {
  return (mask & static_cast<RepairMask>(reason)) != 0;
}

std::vector<std::string> repair_reason_names(RepairMask mask) {
  std::vector<std::string> names;
  if (has_reason(mask, RepairReason::kMissing)) names.emplace_back("missing");
  if (has_reason(mask, RepairReason::kNonMonotonic)) names.emplace_back("non_monotonic");
  if (has_reason(mask, RepairReason::kWraparound)) names.emplace_back("wraparound");
  if (has_reason(mask, RepairReason::kDrift)) names.emplace_back("drift");
  return names;
}

int exit_code_for(ErrorCategory category) {
  switch (category) {
    case ErrorCategory::kInvalidArgument:
    case ErrorCategory::kInvalidInput:
      return 2;
    case ErrorCategory::kCorruption:
      return 3;
    case ErrorCategory::kIo:
    case ErrorCategory::kRecovery:
      return 4;
  }
  return 4;
}

}  // namespace mediaqc
