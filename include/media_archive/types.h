#pragma once
// Shared media timebase, error model and data structures used by every
// recovery module. All timestamps inside the project are expressed in the
// shared 90 kHz media clock unless a function explicitly says otherwise.

#include <cstdint>
#include <string>
#include <vector>

namespace media_archive {

// Shared transport / media clock constants.
inline constexpr uint32_t kTimebaseHz = 90000;
// 16 bit transport sequence numbers.
inline constexpr uint32_t kSeqModulo = 1u << 16;
// 33 bit PTS wraparound, as used by MPEG-TS style media clocks.
inline constexpr uint64_t kPtsModulo = 1ull << 33;
// Reorder window: a packet this far behind the high-water mark is a replay,
// anything inside the window is a legitimately late (reordered) packet.
inline constexpr uint32_t kReorderWindow = 1024;

// Hard safety limits (feature 8); overridable from the command line.
inline constexpr uint64_t kDefaultMaxInputBytes = 256ull * 1024 * 1024;
inline constexpr uint32_t kMaxKeyframesPerBlock = 4096;
inline constexpr uint32_t kMaxBlockBytes = 16u * 1024 * 1024;

enum class CodecType { H264, H265, AV1, Unknown };

enum class PacketFlag : uint8_t {
  Ok = 0,       // present, in order
  Duplicate = 1,// same sequence number seen again
  Reordered = 2,// present but arrived late / out of order
  Gap = 3       // burst loss: bytes are absent from the received stream
};

// One row of the transport packet manifest (packets.csv).
struct TransportPacket {
  uint32_t seq = 0;            // 16 bit transport sequence number
  uint64_t pts_ticks = 0;      // encoder PTS in the shared 90 kHz clock
  int64_t source_offset = -1;  // byte offset in the received stream, -1 for Gap
  uint32_t size_bytes = 0;     // original encoded size (known even for gaps)
  PacketFlag flag = PacketFlag::Ok;
  double send_ts_ms = 0.0;     // simulated send time
  double recv_ts_ms = 0.0;     // simulated receive time (bandwidth trace)
};

// A gap is a run of consecutively missing sequence numbers.
struct GapInterval {
  uint64_t extended_seq_first = 0;
  uint64_t extended_seq_last = 0;
  uint64_t missing_bytes = 0;
};

// Result of sequence repair: packets sorted on the extended (wraparound
// aware) sequence number, plus deduplication and gap accounting.
struct SequenceResult {
  std::vector<TransportPacket> ordered;       // recovered packets only
  std::vector<GapInterval> gaps;
  // For each ordered packet: how many gap slots immediately precede it.
  std::vector<uint32_t> gap_before;
  uint64_t duplicate_count = 0;
  uint64_t replay_count = 0;
  uint64_t unrecovered_bytes = 0;
  uint64_t expected_unique = 0;               // OK + DUP + reordered + gaps
};

// A packet after timestamp repair.
struct RepairedPacket {
  TransportPacket packet;
  uint64_t repaired_pts = 0;   // unwrapped, drift-corrected 90 kHz ticks
  bool pts_uncertain = false;  // true when the interval cannot be solved
  bool pts_wrapped = false;    // a wraparound was crossed at this boundary
};

// One random-access keyframe record.
struct KeyframeEntry {
  uint64_t pts_ticks = 0;      // repaired PTS, shared clock
  uint64_t source_offset = 0;  // byte offset inside the recovered stream
  uint32_t size_bytes = 0;
  uint32_t frame_type = 0;     // AVPictureType cast to int
  uint32_t extended_seq = 0;
  uint32_t checksum = 0;       // CRC32 of the encoded frame bytes
};

struct SeekCheck {
  uint64_t requested_pts = 0;
  uint64_t landed_pts = 0;
  double error_ms = 0.0;
  uint64_t source_offset = 0;
};

struct AudioFragment {
  uint32_t seq = 0;
  uint64_t start_pts_ticks = 0;  // aligned position on the shared clock
  std::vector<float> samples;    // mono, resampled to the common rate
};

struct AudioSyncResult {
  std::vector<AudioFragment> fragments;
  double max_sync_error_ms = 0.0;
  double end_to_end_sync_error_ms = 0.0;
  int64_t expected_samples = 0;
  int64_t actual_samples = 0;
  int64_t audio_sample_delta = 0;
  int sample_rate = 0;
};

struct RecoveryReport {
  CodecType codec = CodecType::Unknown;
  uint64_t recovered_packets = 0;
  uint64_t unrecovered_bytes = 0;
  double loss_recovery_rate = 0.0;
  double p95_latency_ms = 0.0;
  uint64_t p95_sample_count = 0;
  double av_sync_error_ms = 0.0;
  int64_t audio_sample_delta = 0;
  double index_seek_error_ms = 0.0;
  bool checkpoint_resumed = false;
  uint64_t rolled_back_block = UINT64_MAX; // UINT64_MAX == no rollback
  int64_t failure_offset = -1;            // -1 == no parse failure
  uint64_t keyframe_count = 0;
  uint64_t gap_count = 0;
  uint64_t uncertain_pts_count = 0;
  bool valid = false;
};

// Process exit codes (documented in the CLI help).
enum class ExitCode : int {
  Ok = 0,
  UsageError = 2,
  MediaCorrupt = 3,
  CheckpointFailure = 4,
};

const char* codec_name(CodecType codec);
CodecType codec_from_name(const std::string& name);
double ticks_to_ms(uint64_t ticks);
uint32_t crc32_ieee(const uint8_t* data, size_t len);

}  // namespace media_archive
