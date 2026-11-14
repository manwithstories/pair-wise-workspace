#include "analyzer.h"

#include <cstdio>
#include <cstring>
#include <stdexcept>
#include <sys/resource.h>

#include "annexb.h"
#include "nal.h"
#include "picture.h"
#include "pps.h"
#include "sei.h"
#include "slice.h"
#include "sps.h"

namespace h264 {

double peak_rss_mb() {
  struct rusage ru;
  if (getrusage(RUSAGE_SELF, &ru) != 0) {
    return 0.0;
  }
#if defined(__APPLE__)
  return static_cast<double>(ru.ru_maxrss) / (1024.0 * 1024.0);
#else
  return static_cast<double>(ru.ru_maxrss) / 1024.0;
#endif
}

namespace {

void require_full_nal(const NalUnit& nal, const char* what) {
  if (nal.length - 1 > nal.prefix_len) {
    fail_parse(std::string(what) + " NAL exceeds parse buffer (truncated)");
  }
}

}  // namespace

Report analyze_file(const std::string& path) {
  FILE* fp = fopen(path.c_str(), "rb");
  if (!fp) {
    throw std::runtime_error("cannot open input: " + path);
  }
  fseek(fp, 0, SEEK_END);
  uint64_t size = static_cast<uint64_t>(ftell(fp));
  fseek(fp, 0, SEEK_SET);

  SpsStore sps_store;
  PpsStore pps_store;
  PictureAggregator aggregator;
  ScanStats scan;
  SpsSummary sps_summary;
  uint64_t zero_length_nals = 0;
  uint32_t last_sps_id = 0xFFFFFFFF;
  bool multi_sps = false;

  NalCallback cb = [&](const NalUnit& nal) {
    if (nal.nal_type == 0xFF) {
      ++zero_length_nals;
      return;
    }
    NalHeader hdr = parse_nal_header(nal.header_byte);
    if (nal_type_unsupported(hdr.nal_unit_type)) {
      fail_unsupported(std::string("NAL type ") +
                       std::to_string(hdr.nal_unit_type) + " (SVC/MVC) not supported");
    }
    switch (hdr.nal_unit_type) {
      case 7: {
        require_full_nal(nal, "SPS");
        Sps sps = parse_sps(nal.prefix, nal.prefix_len);
        if (last_sps_id != 0xFFFFFFFF && sps.sps_id != last_sps_id) {
          multi_sps = true;
        }
        last_sps_id = sps.sps_id;
        sps_store.add(sps);
        break;
      }
      case 8: {
        require_full_nal(nal, "PPS");
        pps_store.add(parse_pps(nal.prefix, nal.prefix_len, sps_store));
        break;
      }
      case 1:
      case 5: {
        SliceHeader h = parse_slice_header(nal.prefix, nal.prefix_len,
                                           hdr.nal_unit_type == 5, pps_store, sps_store);
        if (!sps_summary.valid) {
          sps_summary.valid = true;
          sps_summary.profile_idc = h.sps->profile_idc;
          sps_summary.level_idc = h.sps->level_idc;
          sps_summary.width = h.sps->width();
          sps_summary.height = h.sps->height();
          sps_summary.chroma_format_idc = h.sps->chroma_format_idc;
          sps_summary.frame_mbs_only_flag = h.sps->frame_mbs_only_flag;
          sps_summary.mbaff = h.sps->mb_adaptive_frame_field_flag;
          sps_summary.fps = h.sps->fps();
        }
        aggregator.on_slice(h, hdr.nal_ref_idc, nal.offset);
        break;
      }
      case 6: {
        require_full_nal(nal, "SEI");
        SeiInfo sei = parse_sei(nal.prefix, nal.prefix_len, sps_store);
        if (sei.has_recovery_point) {
          aggregator.on_recovery_point();
        }
        break;
      }
      case 2: case 3: case 4:
        fail_unsupported("data partition NALs not supported");
      default:
        break;  // AUD, end-of-seq, reserved: ignored
    }
  };

  scan_annexb(fp, cb, scan);
  fclose(fp);
  aggregator.finish();

  Report r;
  r.file = path;
  r.size_bytes = size;
  r.sps = sps_summary;
  r.stream = aggregator.stats();
  if (r.stream.total_frames() == 0) {
    fail_parse("no pictures found in stream");
  }
  if (zero_length_nals > 0) {
    r.stream.warnings.push_back("zero_length_nals=" + std::to_string(zero_length_nals));
  }
  if (multi_sps) {
    r.stream.warnings.push_back("multiple_sps_ids");
  }
  return r;
}

}  // namespace h264
