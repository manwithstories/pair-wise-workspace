#include "picture.h"

#include "nal.h"

namespace h264 {

void PictureAggregator::warn(const std::string& msg) {
  if (st_.warnings.size() < 200) {
    st_.warnings.push_back(msg);
  } else {
    ++st_.warning_overflow;
  }
}

int64_t PictureAggregator::compute_poc(const SliceHeader& h, uint8_t nal_ref_idc) {
  const Sps& sps = *h.sps;
  if (sps.pic_order_cnt_type != 0) {
    return 2 * static_cast<int64_t>(h.frame_num) + (h.bottom_field_flag ? 1 : 0);
  }
  int64_t max_lsb = 1LL << sps.log2_max_pic_order_cnt_lsb;
  int64_t lsb = h.pic_order_cnt_lsb;
  if (h.idr) {
    prev_poc_msb_ = 0;
    prev_poc_lsb_ = 0;
  }
  int64_t msb;
  if (lsb < prev_poc_lsb_ && prev_poc_lsb_ - lsb >= max_lsb / 2) {
    msb = prev_poc_msb_ + max_lsb;
  } else if (lsb > prev_poc_lsb_ && lsb - prev_poc_lsb_ > max_lsb / 2) {
    msb = prev_poc_msb_ - max_lsb;
  } else {
    msb = prev_poc_msb_;
  }
  if (nal_ref_idc != 0) {
    prev_poc_msb_ = msb;
    prev_poc_lsb_ = lsb;
  }
  return msb + lsb;
}

void PictureAggregator::on_slice(const SliceHeader& h, uint8_t nal_ref_idc,
                                 uint64_t nal_offset) {
  st_.ue_count += h.ue_count;
  st_.ue_bits += h.ue_bits;
  st_.header_bits += h.header_bits;
  AuSliceInfo cur;
  cur.first_mb_in_slice = h.first_mb_in_slice;
  cur.frame_num = h.frame_num;
  cur.pps_id = h.pps_id;
  cur.idr_pic_id = h.idr_pic_id;
  cur.field_pic = h.field_pic_flag;
  cur.bottom_field = h.bottom_field_flag;
  cur.idr = h.idr;
  cur.poc_lsb = h.pic_order_cnt_lsb;
  if (is_new_access_unit(have_pic_, cur)) {
    finalize_picture();
    have_pic_ = true;
    pic_au_index_ = au_counter_++;
    pic_offset_ = nal_offset;
    pic_type_ = h.frame_type;
    pic_field_ = h.field_pic_flag;
    pic_bottom_ = h.bottom_field_flag;
    pic_idr_ = h.idr;
    pic_frame_num_ = h.frame_num;
    pic_pps_id_ = h.pps_id;
    pic_slices_ = 1;
    last_first_mb_ = h.first_mb_in_slice;
    pic_poc_ = compute_poc(h, nal_ref_idc);
    pic_sps_ = h.sps;
    if (h.first_mb_in_slice >= h.sps->pic_size_in_map_units()) {
      warn("first_mb_out_of_range au=" + std::to_string(pic_au_index_));
    }
  } else {
    if (h.first_mb_in_slice <= last_first_mb_) {
      warn("dup_first_mb au=" + std::to_string(pic_au_index_));
    }
    if (h.first_mb_in_slice > last_first_mb_) {
      last_first_mb_ = h.first_mb_in_slice;
    }
    if (h.pps_id != pic_pps_id_) {
      warn("cross_pps au=" + std::to_string(pic_au_index_));
    }
    ++pic_slices_;
  }
}

void PictureAggregator::finalize_picture() {
  if (!have_pic_) {
    return;
  }
  st_.slices_per_picture[pic_slices_]++;
  if (prev_pic_slices_ > 0 && pic_slices_ > 2 * prev_pic_slices_ && pic_slices_ >= 8) {
    warn("slice_count_surge au=" + std::to_string(pic_au_index_));
  }
  prev_pic_slices_ = pic_slices_;
  if (!pic_field_) {
    emit_frame(pic_type_, pic_offset_, pic_au_index_, pic_poc_, pic_idr_);
  } else if (!have_pending_field_) {
    pending_ = {pic_type_, pic_offset_, pic_au_index_, pic_frame_num_, pic_bottom_,
                pic_idr_, pic_poc_};
    have_pending_field_ = true;
  } else if (pending_.frame_num == pic_frame_num_ && pending_.bottom != pic_bottom_) {
    int64_t poc = pending_.poc < pic_poc_ ? pending_.poc : pic_poc_;
    emit_frame(pending_.type, pending_.offset, pending_.au_index, poc,
               pending_.idr || pic_idr_);
    have_pending_field_ = false;
  } else if (pending_.frame_num == pic_frame_num_) {
    warn("duplicate_field au=" + std::to_string(pic_au_index_));
    pending_ = {pic_type_, pic_offset_, pic_au_index_, pic_frame_num_, pic_bottom_,
                pic_idr_, pic_poc_};
  } else {
    warn("missing_field_pair au=" + std::to_string(pending_.au_index));
    emit_frame(pending_.type, pending_.offset, pending_.au_index, pending_.poc,
               pending_.idr);
    pending_ = {pic_type_, pic_offset_, pic_au_index_, pic_frame_num_, pic_bottom_,
                pic_idr_, pic_poc_};
  }
  have_pic_ = false;
}

void PictureAggregator::emit_frame(FrameType type, uint64_t offset, uint64_t au_index,
                                   int64_t poc, bool idr) {
  if (idr || recovery_pending_ || !gop_open_) {
    if (gop_open_) {
      st_.gops.push_back(cur_gop_);
    }
    cur_gop_ = GopInfo{};
    cur_gop_.index = st_.gops.size();
    cur_gop_.start_byte_offset = offset;
    cur_gop_.au_index = au_index;
    cur_gop_.poc_start = poc;
    gop_open_ = true;
  }
  recovery_pending_ = false;
  ++cur_gop_.frame_count;
  ++cur_gop_.type_mix[frame_type_name(type)];
  switch (type) {
    case FrameType::kI: ++st_.count_i; break;
    case FrameType::kP: ++st_.count_p; break;
    case FrameType::kB: ++st_.count_b; break;
    default: ++st_.count_other; break;
  }
}

void PictureAggregator::finish() {
  finalize_picture();
  if (have_pending_field_) {
    warn("missing_field_pair au=" + std::to_string(pending_.au_index));
    emit_frame(pending_.type, pending_.offset, pending_.au_index, pending_.poc,
               pending_.idr);
    have_pending_field_ = false;
  }
  if (gop_open_) {
    st_.gops.push_back(cur_gop_);
    gop_open_ = false;
  }
}

}  // namespace h264
