//! 手写 DER TLV 遍历器与长度域校验。
//!
//! 设计目标：面对混杂截断包、长度域溢出、错位嵌套的畸形 DER 流时，
//! 不能整体崩溃——必须定位到**绝对偏移**，导出上下文十六进制转储，
//! 交由上层计数并跳过。

use std::fmt;

/// DER 标签类别 (tag & 0b1100_0000 >> 6)。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TagClass {
    Universal,
    Application,
    ContextSpecific,
    Private,
}

impl fmt::Display for TagClass {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = match self {
            TagClass::Universal => "UNIV",
            TagClass::Application => "APPL",
            TagClass::ContextSpecific => "CTXT",
            TagClass::Private => "PRIV",
        };
        f.write_str(s)
    }
}

/// 解析出的单个 TLV 三元组。
#[derive(Debug, Clone, Copy)]
pub struct Tlv<'a> {
    pub class: TagClass,
    /// 是否为复合结构 (constructed bit, 0b0010_0000)。
    pub constructed: bool,
    /// 标签号。低 tag number 形式单字节即 tag，high-tag-number 形式已解算。
    pub tag_number: u32,
    /// 值域切片（借用输入 buffer）。
    pub value: &'a [u8],
    /// 整个 TLV（含 tag+len）在所属 buffer 中的**相对起始偏移**。
    ///
    /// 注意：这是 buffer 相对偏移，恒等于 `value` 在 buffer 中的位置减去
    /// 长度域长度。流级绝对偏移由调用方在报错时叠加 `abs_base` 得到，
    /// 这样嵌套遍历内部无需做指针反推。
    pub start: usize,
    /// 整个 TLV（含 tag+len）在所属 buffer 中的**相对结束偏移**（不含）。
    pub end: usize,
    /// 嵌套深度，根为 0。
    pub depth: u16,
}

impl<'a> Tlv<'a> {
    pub fn len(&self) -> usize {
        self.end - self.start
    }
    pub fn is_empty(&self) -> bool {
        self.value.is_empty()
    }
}

/// TLV 遍历过程中发现的错误分类。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DerErrorKind {
    /// 值域被截断：声明长度超出可用数据。
    Truncated,
    /// 长度域溢出：长度编码不合法（超长前导零 / 长度过大 / indefinite length）。
    LengthOverflow,
    /// 高 tag number 形式不完整。
    BadTag,
    /// 嵌套过深，疑似构造畸形。
    DepthExceeded,
    /// 结构语义非法（如 composite 承载了不能复合的类型）。
    Malformed,
}

impl DerErrorKind {
    pub fn as_str(&self) -> &'static str {
        match self {
            DerErrorKind::Truncated => "truncated",
            DerErrorKind::LengthOverflow => "length-overflow",
            DerErrorKind::BadTag => "bad-tag",
            DerErrorKind::DepthExceeded => "depth-exceeded",
            DerErrorKind::Malformed => "malformed",
        }
    }
}

impl fmt::Display for DerErrorKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// 带绝对偏移与上下文转储的解析错误。
#[derive(Debug, Clone)]
pub struct DerError {
    pub kind: DerErrorKind,
    /// 出错点的绝对偏移。
    pub offset: usize,
    /// 出错点所在 TLV 的起始绝对偏移（若已知）。
    pub record_start: Option<usize>,
    /// 人类可读说明。
    pub detail: String,
    /// 出错点前后各 32 字节的十六进制转储。
    pub hex_dump: String,
    /// 冒泡路径上的父容器记录相对偏移，用于定位错位嵌套。
    pub hex_window_parent: Option<usize>,
}

impl fmt::Display for DerError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "DER {} at offset {} (record@{}): {}",
            self.kind,
            self.offset,
            self.record_start
                .map(|o| o.to_string())
                .unwrap_or_else(|| "?".into()),
            self.detail
        )
    }
}

impl std::error::Error for DerError {}

/// 生成出错点前后各 `win` 字节的十六进制转储，偏移标注在转储内。
pub fn hex_window(buf: &[u8], offset: usize, win: usize) -> String {
    let start = offset.saturating_sub(win);
    let end = std::cmp::min(buf.len(), offset.saturating_add(win));
    let mut s = String::with_capacity((end - start) * 3 + 32);
    s.push_str(&format!("[0x{:08x}..0x{:08x}) ", start, end));
    for (i, b) in buf[start..end].iter().enumerate() {
        if i > 0 && i % 16 == 0 {
            s.push('\n');
            s.push_str(&format!("  0x{:08x}: ", start + i));
        } else if i > 0 {
            s.push(' ');
        }
        s.push_str(&format!("{:02x}", b));
    }
    s
}

/// 构造一个带转储的 DerError。
pub fn make_error(
    buf: &[u8],
    kind: DerErrorKind,
    offset: usize,
    record_start: Option<usize>,
    detail: impl Into<String>,
) -> DerError {
    DerError {
        kind,
        offset,
        record_start,
        detail: detail.into(),
        hex_dump: hex_window(buf, offset, 32),
        hex_window_parent: None,
    }
}

/// 遍历配置。
#[derive(Debug, Clone)]
pub struct WalkConfig {
    pub max_depth: u16,
    /// 单个容器内允许的子节点上限，防御性上限。
    pub max_children: usize,
    /// 是否递归进入 constructed 值域校验嵌套一致性。
    pub recurse: bool,
}

impl Default for WalkConfig {
    fn default() -> Self {
        WalkConfig {
            max_depth: 24,
            max_children: 1_000_000,
            recurse: true,
        }
    }
}

/// 遍历统计。
#[derive(Debug, Clone, Default)]
pub struct WalkStats {
    pub tlvs: u64,
    pub max_depth_seen: u16,
}

/// 识别 X.509 结构中需要专门校验的关键 universal tag。
pub mod tags {
    pub const SEQUENCE: u32 = 0x10;
    pub const SET: u32 = 0x11;
    pub const BIT_STRING: u32 = 0x03;
    pub const OCTET_STRING: u32 = 0x04;
    pub const OID: u32 = 0x06;
    pub const INTEGER: u32 = 0x02;
    pub const UTCTIME: u32 = 0x17;
    pub const GENERALIZEDTIME: u32 = 0x18;
    pub const NULL: u32 = 0x05;
    pub const BOOLEAN: u32 = 0x01;
    pub const PRINTABLESTRING: u32 = 0x13;
    pub const UTF8STRING: u32 = 0x0c;
    pub const IA5STRING: u32 = 0x16;
}

/// 校验长度域编码本身是否合法（DER 规范化要求：短形式/长形式不得混用、
/// 不得有前导冗余 0、长格式至少 2 字节且必须指明字节数）。
///
/// 返回 `(value_length, header_length)`。
fn parse_len(buf: &[u8], pos: usize) -> Result<(usize, usize), (DerErrorKind, String)> {
    if pos >= buf.len() {
        return Err((
            DerErrorKind::Truncated,
            "长度域缺失：TLV 头在数据末尾处被截断".to_string(),
        ));
    }
    let b0 = buf[pos];
    if b0 < 0x80 {
        Ok((b0 as usize, 1))
    } else {
        let n = (b0 & 0x7f) as usize;
        if n == 0 {
            return Err((
                DerErrorKind::LengthOverflow,
                "indefinite length (0x80) 在 DER 中非法".to_string(),
            ));
        }
        if n > 8 {
            return Err((
                DerErrorKind::LengthOverflow,
                format!("长度域长格式指示 {n} 字节，超出 usize 可表示范围"),
            ));
        }
        if pos + 1 + n > buf.len() {
            return Err((
                DerErrorKind::Truncated,
                format!("长度域声明 {n} 字节但仅剩 {} 字节", buf.len() - pos - 1),
            ));
        }
        // DER 规范化：长格式不得用于 <128 的值，也不得有前导零字节。
        if buf[pos + 1] == 0x00 {
            return Err((
                DerErrorKind::LengthOverflow,
                "长度域长格式含前导冗余 0x00，违反 DER 最小编码要求".to_string(),
            ));
        }
        let mut v: u64 = 0;
        for i in 0..n {
            v = (v << 8) | buf[pos + 1 + i] as u64;
            if v > usize::MAX as u64 {
                return Err((
                    DerErrorKind::LengthOverflow,
                    "长度域数值溢出 usize".to_string(),
                ));
            }
        }
        if n == 1 && v < 0x80 {
            return Err((
                DerErrorKind::LengthOverflow,
                "长度域值 <128 却使用长格式，违反 DER 最小编码要求".to_string(),
            ));
        }
        Ok((v as usize, 1 + n))
    }
}

/// 解析单个 TLV 头 + 值域，返回 Tlv。
fn parse_tlv_at<'a>(
    buf: &'a [u8],
    pos: usize,
    depth: u16,
    _cfg: &WalkConfig,
) -> Result<Tlv<'a>, DerError> {
    let abs = pos;
    if pos >= buf.len() {
        return Err(make_error(
            buf,
            DerErrorKind::Truncated,
            abs,
            Some(abs),
            "TLV 起始越界：数据在此处结束",
        ));
    }
    let id = buf[pos];
    let class = match id & 0xc0 {
        0x00 => TagClass::Universal,
        0x40 => TagClass::Application,
        0x80 => TagClass::ContextSpecific,
        _ => TagClass::Private,
    };
    let constructed = id & 0x20 != 0;
    let mut tag_number: u32;
    let mut hp = pos + 1;
    if id & 0x1f == 0x1f {
        // high tag number form：0x1f 后接若干 7-bit 组，末位最高位为 0。
        tag_number = 0;
        let mut groups = 0u32;
        loop {
            if hp >= buf.len() {
                return Err(make_error(
                    buf,
                    DerErrorKind::BadTag,
                    hp,
                    Some(abs),
                    "high-tag-number 形式在数据末尾被截断",
                ));
            }
            let g = buf[hp];
            hp += 1;
            groups += 1;
            if groups > 5 {
                return Err(make_error(
                    buf,
                    DerErrorKind::BadTag,
                    hp - 1,
                    Some(abs),
                    "high-tag-number 形式超过 5 个字节组，标签号异常",
                ));
            }
            // 防溢出：每组最多 7 bit，5 组 = 35 bit，放宽到 u32 上界检测。
            if tag_number > (u32::MAX >> 7) {
                return Err(make_error(
                    buf,
                    DerErrorKind::LengthOverflow,
                    hp - 1,
                    Some(abs),
                    "tag number 溢出 u32",
                ));
            }
            tag_number = (tag_number << 7) | (g & 0x7f) as u32;
            if g & 0x80 == 0 {
                break;
            }
        }
        if tag_number < 0x1f {
            return Err(make_error(
                buf,
                DerErrorKind::BadTag,
                abs,
                Some(abs),
                format!("tag number {tag_number} 应使用低 tag number 形式"),
            ));
        }
    } else {
        tag_number = (id & 0x1f) as u32;
    }

    let (vlen, len_bytes) = parse_len(buf, hp).map_err(|(kind, detail)| {
        make_error(buf, kind, hp, Some(abs), detail)
    })?;
    let value_start = hp + len_bytes;
    // 加法溢出检查：value_start / value_start+vlen 不得回绕。
    if value_start < hp || value_start > buf.len() {
        return Err(make_error(
            buf,
            DerErrorKind::Truncated,
            hp,
            Some(abs),
            "TLV 头跨越数据末尾",
        ));
    }
    let vlen_end = match value_start.checked_add(vlen) {
        Some(e) => e,
        None => {
            return Err(make_error(
                buf,
                DerErrorKind::LengthOverflow,
                hp,
                Some(abs),
                format!("TLV 值域长度 {vlen} 与偏移 {value_start} 相加溢出"),
            ))
        }
    };
    if vlen_end > buf.len() {
        return Err(make_error(
            buf,
            DerErrorKind::Truncated,
            abs,
            Some(abs),
            format!(
                "TLV 值域被截断：声明长度 {vlen}（值域起 {value_start}），实际仅剩 {}",
                buf.len() - value_start
            ),
        ));
    }

    Ok(Tlv {
        class,
        constructed,
        tag_number,
        value: &buf[value_start..vlen_end],
        start: abs,
        end: vlen_end,
        depth,
    })
}

/// 迭代一个 constructed 值域内的所有子 TLV。
///
/// 这是本文件的核心不变量：**扫描位置被严格夹在父值域区间内**。
/// 由于 `parse_tlv_at` 收到的 `buf` 是父值域切片本身（而非整个记录），
/// 子节点声明的长度一旦越过父容器尾端，必然触发 `Truncated` 而被就地捕获，
/// 绝不会读到父容器之外、也就是下一个兄弟节点的数据——"错位嵌套"由此被隔离。
pub fn children<'a>(
    region: &'a [u8],
    parent: &Tlv<'a>,
    cfg: &WalkConfig,
) -> Result<Vec<Tlv<'a>>, DerError> {
    let mut out = Vec::new();
    if !parent.constructed {
        return Ok(out);
    }
    // `region` 必须是父值域切片本身；其长度即子节点可用预算。
    let mut pos = 0usize;
    let limit = region.len();
    let mut count = 0usize;
    while pos < limit {
        if count >= cfg.max_children {
            let delta = region.as_ptr() as usize - parent.value.as_ptr() as usize;
            return Err(make_error(
                parent.value,
                DerErrorKind::Malformed,
                pos + delta,
                Some(parent.start),
                format!("容器子节点数超过上限 {}", cfg.max_children),
            ));
        }
        let child = parse_tlv_at(region, pos, parent.depth + 1, cfg).map_err(|mut e| {
            // parse_tlv_at 的偏移是 `region`(=父值域) 相对的，换算回整个记录相对。
            let delta = region.as_ptr() as usize - parent.value.as_ptr() as usize;
            e.offset += delta;
            if let Some(rs) = e.record_start {
                e.record_start = Some(rs + delta);
            }
            e.hex_dump = hex_window(parent.value, e.offset - delta, 32);
            e.detail = format!("[父 {}@{}] {}", parent.tag_number, parent.start, e.detail);
            e
        })?;
        // 递归一致性：子 TLV 必须在父值域预算内闭合。
        if pos + child.len() > limit {
            let delta = region.as_ptr() as usize - parent.value.as_ptr() as usize;
            return Err(make_error(
                parent.value,
                DerErrorKind::Malformed,
                pos + delta,
                Some(parent.start),
                format!(
                    "错位嵌套：子 TLV 声明结束于 {}，超出父容器值域末尾 {}",
                    pos + child.len(),
                    limit
                ),
            ));
        }
        let consumed = child.len();
        count += 1;
        pos += consumed;
        if cfg.recurse && child.constructed && child.depth < cfg.max_depth {
            // 必须用 child.value：children 的扫描边界必须严格等于当前容器的值域。
            // 若误传 region（父值域），子层 pos 会相对错误的边界前进，
            // 在真实证书上表现为永不推进的死循环。
            children(child.value, &child, cfg)?;
        }
        out.push(child);
    }
    Ok(out)
}

/// 深度优先遍历整条记录，校验长度域递归一致性。
///
/// `abs_base` 是本记录在**整个流**中的绝对起始偏移，使错误报告能给出流级坐标。
pub fn walk<'a>(
    buf: &'a [u8],
    abs_base: usize,
    cfg: &WalkConfig,
    stats: &mut WalkStats,
) -> Result<Tlv<'a>, DerError> {
    if buf.is_empty() {
        return Err(make_error(
            buf,
            DerErrorKind::Truncated,
            abs_base,
            Some(abs_base),
            "记录长度为 0",
        ));
    }
    // 根层错误按流级绝对坐标报告（record-relative + abs_base）。
    let root = parse_tlv_at(buf, 0, 0, cfg).map_err(|mut e| {
        e.offset += abs_base;
        if let Some(rs) = e.record_start {
            e.record_start = Some(rs + abs_base);
        }
        e
    })?;
    stats.tlvs += 1;

    // 根必须完整覆盖记录：既不能有剩余尾随字节，也不能提前结束。
    if root.len() != buf.len() {
        let kind = if root.len() > buf.len() {
            DerErrorKind::Truncated
        } else {
            DerErrorKind::Malformed
        };
        return Err(make_error(
            buf,
            kind,
            root.len(),
            Some(0),
            format!(
                "根 TLV 长度 {} 与记录长度 {} 不一致（错位嵌套/尾随垃圾）",
                root.len(),
                buf.len()
            ),
        ));
    }
    if root.constructed {
        validate_tree(buf, &root, cfg, stats).map_err(|mut e| {
            e.offset += abs_base;
            if let Some(rs) = e.record_start {
                e.record_start = Some(rs + abs_base);
            }
            e
        })?;
    }
    Ok(root)
}

/// 递归统计并校验 constructed 树。
fn validate_tree<'a>(
    buf: &[u8],
    node: &Tlv<'a>,
    cfg: &WalkConfig,
    stats: &mut WalkStats,
) -> Result<(), DerError> {
    if node.depth >= cfg.max_depth {
        return Err(make_error(
            buf,
            DerErrorKind::DepthExceeded,
            node.start,
            Some(node.start),
            format!("嵌套深度 {} 超过上限 {}", node.depth, cfg.max_depth),
        ));
    }
    let kids = children(node.value, node, cfg)?;
    stats.tlvs += kids.len() as u64;
    stats.max_depth_seen = stats.max_depth_seen.max(node.depth);
    for k in kids {
        // BIT_STRING / OCTET_STRING 即使标了 constructed 位在 DER 中也非法。
        if k.constructed
            && k.class == TagClass::Universal
            && matches!(k.tag_number, tags::BIT_STRING | tags::OCTET_STRING)
        {
            return Err(make_error(
                buf,
                DerErrorKind::Malformed,
                k.start,
                Some(node.start),
                format!(
                    "universal tag {} ({}) 在 DER 中不得为 constructed",
                    k.tag_number, "BIT_STRING/OCTET_STRING"
                ),
            ));
        }
        if k.constructed {
            // 无条件递归，使每一层都执行深度检查，越界层立即报错。
            validate_tree(buf, &k, cfg, stats)?;
        }
    }
    Ok(())
}

/// 解析缓冲区中的**单个**顶层 TLV（不做整记录递归校验）。
///
/// 供字段抽取层在已校验过的记录内二次解析子结构使用。
pub fn parse_tlv_at_public(buf: &[u8]) -> Result<Tlv<'_>, DerError> {
    parse_tlv_at(buf, 0, 0, &WalkConfig::default())
}

/// 便捷函数：只校验不返回统计。
pub fn validate_record(buf: &[u8], abs_base: usize) -> Result<Tlv<'_>, DerError> {
    let cfg = WalkConfig::default();
    let mut stats = WalkStats::default();
    walk(buf, abs_base, &cfg, &mut stats)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_simple_sequence() {
        // SEQUENCE { INTEGER 1 }
        let d = [0x30, 0x03, 0x02, 0x01, 0x01];
        let root = validate_record(&d, 0).expect("should parse");
        assert_eq!(root.class, TagClass::Universal);
        assert_eq!(root.tag_number, tags::SEQUENCE);
        assert!(root.constructed);
        assert_eq!(root.len(), 5);
    }

    #[test]
    fn nested_sequence_ok() {
        // SEQUENCE { SEQUENCE { NULL } }
        let d = [0x30, 0x02, 0x30, 0x00];
        let root = validate_record(&d, 0).expect("nested ok");
        let kids = children(root.value, &root, &WalkConfig::default()).unwrap();
        assert_eq!(kids.len(), 1);
        assert_eq!(kids[0].tag_number, tags::SEQUENCE);
    }

    #[test]
    fn deep_parse_offers_every_tlv() {
        // SEQUENCE { INTEGER 42, SEQUENCE { NULL } }
        //   root content = INTEGER(3) + inner SEQ(4) = 7
        let d = [
            0x30, 0x07, // SEQ, len 7
            0x02, 0x01, 0x2a, // INTEGER 42
            0x30, 0x02, // SEQ, len 2
            0x05, 0x00, // NULL
        ];
        let mut st = WalkStats::default();
        let root = walk(&d, 0, &WalkConfig::default(), &mut st).unwrap();
        // root + INTEGER + inner SEQ + NULL = 4
        assert_eq!(st.tlvs, 4, "expected full traversal to reach leaf NULL");
        assert_eq!(st.max_depth_seen, 1);
        let kids = children(root.value, &root, &WalkConfig::default()).unwrap();
        assert_eq!(kids.len(), 2);
        let inner = &kids[1];
        let leaves = children(inner.value, inner, &WalkConfig::default()).unwrap();
        assert_eq!(leaves.len(), 1);
        assert_eq!(leaves[0].tag_number, tags::NULL);
    }

    #[test]
    fn detects_truncation() {
        // SEQUENCE 声明 10 字节，实际只有 2
        let d = [0x30, 0x0a, 0x02, 0x01];
        let e = validate_record(&d, 0).unwrap_err();
        assert_eq!(e.kind, DerErrorKind::Truncated);
        assert!(!e.hex_dump.is_empty());
    }

    #[test]
    fn detects_indefinite_length() {
        let d = [0x30, 0x80, 0x00, 0x00];
        let e = validate_record(&d, 0).unwrap_err();
        assert_eq!(e.kind, DerErrorKind::LengthOverflow);
    }

    #[test]
    fn detects_non_minimal_length() {
        // 0x81 0x05 用于长度 5，违反 DER 最小编码
        let d = [0x30, 0x81, 0x05, 0x02, 0x01, 0x01, 0x00];
        let e = validate_record(&d, 0).unwrap_err();
        assert_eq!(e.kind, DerErrorKind::LengthOverflow);
    }

    #[test]
    fn detects_trailing_garbage() {
        let d = [0x30, 0x03, 0x02, 0x01, 0x01, 0xff, 0xff];
        let e = validate_record(&d, 0).unwrap_err();
        assert_eq!(e.kind, DerErrorKind::Malformed);
    }

    #[test]
    fn detects_misaligned_nesting() {
        // 外层 SEQUENCE 长度 5，但其内子 SEQUENCE 声称长度 10 —— 越过父容器边界
        let d = [0x30, 0x05, 0x30, 0x0a, 0x02, 0x01, 0x01, 0x02, 0x01, 0x02];
        let e = validate_record(&d, 0).unwrap_err();
        assert!(
            e.kind == DerErrorKind::Malformed || e.kind == DerErrorKind::Truncated,
            "unexpected kind {:?}",
            e.kind
        );
    }

    #[test]
    fn absolute_offsets_are_reported() {
        let d = [0x30, 0x0a, 0x02, 0x01];
        let e = walk(&d, 1000, &WalkConfig::default(), &mut WalkStats::default()).unwrap_err();
        assert_eq!(e.offset, 1000);
        assert_eq!(e.record_start, Some(1000));
    }

    #[test]
    fn long_form_length_accepted() {
        let mut d = vec![0x04, 0x81, 0x80];
        d.extend(std::iter::repeat(0u8).take(128));
        let root = validate_record(&d, 0).expect("long form len 128 ok");
        assert_eq!(root.tag_number, tags::OCTET_STRING);
        assert_eq!(root.value.len(), 128);
    }

    #[test]
    fn depth_limit_enforced() {
        // 构造 30 层嵌套 SEQUENCE（手工堆出）
        let mut inner = vec![0x05, 0x00]; // NULL
        for _ in 0..30 {
            let mut t = vec![0x30u8];
            // 长度 <128 用短形式
            t.push(inner.len() as u8);
            t.extend_from_slice(&inner);
            inner = t;
        }
        let cfg = WalkConfig {
            max_depth: 10,
            ..Default::default()
        };
        let mut stats = WalkStats::default();
        let e = walk(&inner, 0, &cfg, &mut stats).unwrap_err();
        assert_eq!(e.kind, DerErrorKind::DepthExceeded);
    }

    #[test]
    fn hex_window_covers_both_sides() {
        let buf: Vec<u8> = (0..200u32).map(|x| x as u8).collect();
        let h = hex_window(&buf, 100, 32);
        assert!(h.contains(&format!("0x{:08x}", 100 - 32)));
        assert!(h.contains(&format!("0x{:08x}", 100 + 32)));
    }
}
