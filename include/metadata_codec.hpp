#pragma once

// Decodes the fixed header, versioned metadata fields and payload of a single
// MFS1 record body. Framing (magic, length, resynchronisation) lives in
// PacketReader; this module only interprets bytes that are already known to be
// inside one record.

#include <span>
#include <string>

#include "media_types.hpp"

namespace mfs {

enum class BodyStatus {
  Ok,
  ChecksumMismatch,      // payload digest does not match; record is dropped
  Malformed,             // structurally invalid metadata or payload
  UnknownRequiredField,  // a required field with an unrecognised id
  LimitExceeded,         // payload exceeds the configured per-frame limit
};

struct BodyResult {
  BodyStatus status{BodyStatus::Ok};
  ErrorType error_type{ErrorType::UnsupportedMetadata};
  std::string detail;
};

// Fills `packet.header`, `packet.payload` and `packet.checksum`. On a non-Ok
// status only `result` is meaningful.
BodyResult decode_packet_body(std::span<const uint8_t> body,
                              size_t max_frame_bytes, Packet& packet);

}  // namespace mfs
