#include "gop_analyzer.h"

#include <algorithm>

#include "sps_parser.h"

namespace h264 {

void GopAnalyzer::openGop(const PictureInfo &pic) {
  GopInfo g;
  g.gop_id = cur_gop_++;
  g.start_offset = pic.first_nal_offset;
  g.start_slice_type = pic.slice_type;
  g.starts_with_idr = pic.idr;
  r_.gops.push_back(g);
}

bool GopAnalyzer::samePicture(const PictureInfo &pic, const SliceInfo &sl,
                              bool field, bool bottom) {
  if (pic.frame_num != sl.frame_num)
    return false;
  if (field) {
    // 场图像：同 frame_num 下以场方向区分；同方向的多个分片属同一图像
    return pic.field_pic && pic.bottom_field == bottom;
  }
  return !pic.field_pic; // 帧图像
}

PictureInfo *GopAnalyzer::findOrCreatePicture(const SliceInfo &sl,
                                              const NalUnit &nal, bool idr) {
  const bool field = sl.field_pic_flag;
  const bool bottom = sl.bottom_field_flag;

  // 仅在最近若干图像内回溯，避免 O(n^2)；同 frame_num 的场/帧必然相邻
  for (size_t i = r_.pictures.size(); i-- > 0;) {
    PictureInfo &pic = r_.pictures[i];
    if (samePicture(pic, sl, field, bottom))
      return &pic;
    // frame_num 回绕或已越过当前 frame_num，停止回溯
    if (pic.frame_num != sl.frame_num && !field)
      break;
  }

  PictureInfo pic;
  pic.frame_num = sl.frame_num;
  pic.slice_type = sl.slice_type;
  pic.is_i = sl.is_i;
  pic.is_p = sl.is_p;
  pic.is_b = sl.is_b;
  pic.idr = idr;
  pic.field_pic = field;
  pic.bottom_field = bottom;
  pic.slice_count = 0;
  pic.first_mb_min = sl.first_mb_in_slice;
  pic.first_mb_max = sl.first_mb_in_slice;
  pic.first_nal_offset = nal.start_offset;
  pic.last_nal_offset = nal.start_offset;
  pic.gop_index = cur_gop_;

  // 随机访问点：IDR 一定开新 GOP；非 IDR 的 I 帧也视为恢复点开新 GOP
  const bool random_access = idr || sl.is_i;
  if (r_.gops.empty() || random_access)
    openGop(pic);
  pic.gop_index = cur_gop_ - 1;
  r_.pictures.push_back(pic);
  return &r_.pictures.back();
}

bool GopAnalyzer::processNal(const NalUnit &nal) {
  ++r_.nal_count;
  if (nal.truncated)
    ++r_.truncated_nal_count;

  if (nal.nal_unit_type == NAL_SPS) {
    SpsInfo s;
    auto st = SpsParser::parse(nal, s);
    if (!st.ok) {
      ++r_.parse_failure_count;
      r_.warnings.push_back({nal.start_offset, "SPS: " + st.error});
      return !strict_;
    }
    active_sps_ = s;
    has_sps_ = true;
    r_.sps.push_back(s);
    return true;
  }
  if (nal.nal_unit_type == NAL_PPS) {
    PpsInfo p;
    auto st = PpsParser::parse(nal, p);
    if (!st.ok) {
      ++r_.parse_failure_count;
      r_.warnings.push_back({nal.start_offset, "PPS: " + st.error});
      return !strict_;
    }
    active_pps_ = p;
    has_pps_ = true;
    r_.pps.push_back(p);
    return true;
  }

  if (!NalScanner::isVclNal(nal.nal_unit_type))
    return true; // AUD/SEI 等非 VCL 单元已计入 nal_count，不做结构解析

  ++r_.vcl_nal_count;
  ++r_.slice_count;

  if (!has_sps_ || !has_pps_) {
    ++r_.parse_failure_count;
    r_.warnings.push_back(
        {nal.start_offset, "slice before any valid SPS/PPS"});
    return !strict_;
  }

  SliceInfo sl;
  auto st = SliceParser::parse(nal, active_sps_, active_pps_, sl);
  if (!st.ok) {
    // 上下文不完整的 slice：进入异常统计，后续 NAL 继续解析
    ++r_.parse_failure_count;
    r_.warnings.push_back({nal.start_offset, "slice: " + st.error});
    return !strict_;
  }

  const bool idr = nal.nal_unit_type == NAL_IDR_SLICE;
  PictureInfo *pic = findOrCreatePicture(sl, nal, idr);
  pic->slice_count++;
  pic->last_nal_offset = nal.start_offset;
  pic->first_mb_min = std::min(pic->first_mb_min, sl.first_mb_in_slice);
  pic->first_mb_max = std::max(pic->first_mb_max, sl.first_mb_in_slice);
  return true;
}

AnalysisResult GopAnalyzer::finish() {
  // 汇总每个 GOP 的图像计数与类型计数
  for (const PictureInfo &pic : r_.pictures) {
    if (pic.gop_index >= r_.gops.size())
      continue;
    GopInfo &g = r_.gops[pic.gop_index];
    ++g.picture_count;
    if (pic.is_i)
      ++g.i_count;
    else if (pic.is_p)
      ++g.p_count;
    else if (pic.is_b)
      ++g.b_count;
  }

  // 隔行场对：按 frame_num 归并顶/底场
  for (const PictureInfo &pic : r_.pictures) {
    if (!pic.field_pic)
      continue;
    FieldPairInfo *fp = nullptr;
    for (auto &cand : r_.field_pairs) {
      if (cand.frame_num == pic.frame_num) {
        fp = &cand;
        break;
      }
    }
    if (!fp) {
      FieldPairInfo fresh;
      fresh.frame_num = pic.frame_num;
      r_.field_pairs.push_back(fresh);
      fp = &r_.field_pairs.back();
    }
    if (pic.bottom_field)
      fp->bottom_present = true;
    else
      fp->top_present = true;
  }
  for (const auto &fp : r_.field_pairs) {
    if (fp.top_present && fp.bottom_present)
      ++r_.field_pair_complete;
    else
      ++r_.field_pair_incomplete;
  }

  // 多分片图像的 slice 数量范围
  if (!r_.pictures.empty()) {
    r_.min_slices_per_pic = r_.pictures[0].slice_count;
    r_.max_slices_per_pic = r_.pictures[0].slice_count;
    for (const PictureInfo &pic : r_.pictures) {
      r_.min_slices_per_pic =
          std::min(r_.min_slices_per_pic, pic.slice_count);
      r_.max_slices_per_pic =
          std::max(r_.max_slices_per_pic, pic.slice_count);
    }
  }
  return std::move(r_);
}

} // namespace h264
