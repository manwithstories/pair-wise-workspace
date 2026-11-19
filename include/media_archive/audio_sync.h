#pragma once
// Audio resampling and alignment. Audio fragments are stored in a small
// self-describing PCM archive (magic "MAPCM1\0") so fragment boundaries and
// their source PTS survive the transport; libswresample converts each
// fragment to the common sample rate and float layout, after which the
// fragments are placed on the shared 90 kHz media clock.

#include <cstdint>
#include <string>
#include <vector>

#include "media_archive/error.h"
#include "media_archive/types.h"

namespace media_archive {

inline constexpr uint32_t kCommonAudioRate = 48000;

// One stored fragment: signed 16 bit mono PCM plus its shared-clock PTS.
struct PcmFragment {
  uint64_t start_pts_ticks = 0;
  std::vector<int16_t> samples_s16;
};

struct AudioArchive {
  uint32_t sample_rate = 0;
  uint16_t channels = 1;
  std::vector<PcmFragment> fragments;
};

// Read/write the MAPCM1 container. Corruption is reported with a byte offset.
Diagnostic read_audio_archive(const std::string& path, AudioArchive* archive);
Diagnostic write_audio_archive(const std::string& path,
                               const AudioArchive& archive);

// Resample every fragment to `common_rate` and align it to the shared media
// clock. Gaps between fragments are filled with silence so boundaries stay
// intact; every placement error is measured and the worst one reported.
// `timeline_end_pts` is the video timeline length used for sample accounting.
AudioSyncResult align_audio(const AudioArchive& archive,
                            uint32_t common_rate,
                            uint64_t timeline_end_pts);

}  // namespace media_archive
