//! Hand-written ASN.1 DER TLV traversal with strict length-domain validation.
//!
//! No third-party ASN.1 crate is used. The walker is deliberately *total*:
//! every malformed construct becomes a located [`DerError`] carrying the
//! absolute offset within the record, so the caller can skip exactly one bad
//! record and keep the surrounding batch intact.
//!
//! # Offset convention
//!
//! Every [`Tlv`] offset is *absolute within the record*, which is what makes the
//! hex dumps in [`DerError`] meaningful to an operator staring at a hex editor.
//! Two entry points keep that unambiguous:
//!
//! [`parse_tlv_at`] parses the TLV starting at absolute index `at` of the whole
//! record. Every [`Tlv`] keeps a back-reference to that record, so a defect
//! buried inside a certificate can still dump the 32 bytes on either side of
//! it from the original buffer.

/// Universal tag classes and tags (X.690 §8.1).
pub mod tag {
    pub const UNIVERSAL: u8 = 0x00;
    pub const CONTEXT: u8 = 0x80;
    pub const CONSTRUCTED: u8 = 0x20;

    pub const BOOLEAN: u8 = 0x01;
    pub const INTEGER: u8 = 0x02;
    pub const BIT_STRING: u8 = 0x03;
    pub const OCTET_STRING: u8 = 0x04;
    pub const OID: u8 = 0x06;
    pub const SEQUENCE: u8 = 0x10;
    pub const PRINTABLE_STRING: u8 = 0x13;
    pub const IA5_STRING: u8 = 0x16;
    pub const UTC_TIME: u8 = 0x17;
    pub const GENERALIZED_TIME: u8 = 0x18;
}

/// Bytes of context captured on either side of a bad record for the hex dump.
pub const HEX_DUMP_CONTEXT: usize = 32;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DerErrorKind {
    /// Ran past the end of the record mid-TLV.
    Truncated,
    /// Length field is not in its shortest possible form.
    NonMinimalLength,
    /// Indefinite length — forbidden in DER.
    IndefiniteLength,
    /// Declared length does not fit the record, or would overflow usize.
    LengthOverflow,
    /// Constructed bit missing from an encoding that must be constructed.
    NotConstructed,
    /// Trailing garbage after the outermost TLV was fully consumed.
    TrailingBytes,
    /// Recursive walk found an inner TLV that breaks its parent's length domain.
    NestedLengthMismatch,
    /// Structurally valid TLV chain but not an X.509 certificate.
    NotACertificate,
}

impl DerErrorKind {
    pub fn as_str(self) -> &'static str {
        match self {
            DerErrorKind::Truncated => "truncated",
            DerErrorKind::NonMinimalLength => "non-minimal-length",
            DerErrorKind::IndefiniteLength => "indefinite-length",
            DerErrorKind::LengthOverflow => "length-overflow",
            DerErrorKind::NotConstructed => "not-constructed",
            DerErrorKind::TrailingBytes => "trailing-bytes",
            DerErrorKind::NestedLengthMismatch => "nested-length-mismatch",
            DerErrorKind::NotACertificate => "not-a-certificate",
        }
    }
}

/// A located structural defect plus the surrounding bytes as a hex dump.
#[derive(Debug, Clone)]
pub struct DerError {
    pub kind: DerErrorKind,
    /// Absolute offset within the record at which the defect was detected.
    pub offset: usize,
    /// Up to [`HEX_DUMP_CONTEXT`] bytes preceding `offset`, hex and space separated.
    pub before: String,
    /// Bytes from `offset` through the failing TLV, hex and space separated.
    pub after: String,
    pub detail: &'static str,
}

impl DerError {
    /// Single-line operator-facing rendering, e.g. for the corrupt-record log.
    pub fn describe(&self) -> String {
        format!(
            "{} @0x{:x}: {} [before: {} | after: {}]",
            self.kind.as_str(),
            self.offset,
            self.detail,
            if self.before.is_empty() { "-" } else { &self.before },
            if self.after.is_empty() { "-" } else { &self.after },
        )
    }
}

impl std::fmt::Display for DerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.describe())
    }
}

/// Build a located error, capturing up to [`HEX_DUMP_CONTEXT`] bytes either side.
///
/// `at` and `end` are absolute offsets into `record`, so the dump windows are
/// taken from the original buffer and can reach outside the enclosing value
/// slice in which the defect was detected.
fn located(kind: DerErrorKind, at: usize, end: usize, record: &[u8], detail: &'static str) -> DerError {
    let i = at.min(record.len());
    // Widen the forward window to cover the whole failing TLV when it is short,
    // so an operator sees the whole bogus length domain, not just 32 bytes.
    let j = end.min(record.len()).max(i + HEX_DUMP_CONTEXT).min(record.len());
    DerError {
        kind,
        offset: at,
        before: hex_dump(&record[i.saturating_sub(HEX_DUMP_CONTEXT)..i]),
        after: hex_dump(&record[i..j]),
        detail,
    }
}

/// Construct an error that has no surrounding bytes to show (logical, not
/// length-domain, failures such as "not a certificate").
pub fn logical_error(kind: DerErrorKind, offset: usize, detail: &'static str) -> DerError {
    DerError {
        kind,
        offset,
        before: String::new(),
        after: String::new(),
        detail,
    }
}

pub fn hex_dump(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut s = String::with_capacity(bytes.len() * 3);
    for (n, b) in bytes.iter().enumerate() {
        if n > 0 {
            s.push(' ');
        }
        s.push(HEX[(b >> 4) as usize] as char);
        s.push(HEX[(b & 0x0f) as usize] as char);
    }
    s
}

/// One TLV: the identifier octet, the length, and the value extent.
///
/// All offsets are absolute within the record being parsed.
#[derive(Debug, Clone, Copy)]
pub struct Tlv<'a> {
    pub tag_byte: u8,
    pub tag_class: u8,
    pub constructed: bool,
    pub tag_number: u32,
    /// Absolute offset of the identifier octet.
    pub start: usize,
    /// Absolute offset of the first value octet.
    pub value_start: usize,
    /// Absolute offset one past the last value octet.
    pub value_end: usize,
    /// The value octets.
    pub value: &'a [u8],
    /// The whole record this TLV was parsed from. Kept so a defect deep inside
    /// a certificate can still dump context from outside its enclosing values.
    pub record: &'a [u8],
}

impl<'a> Tlv<'a> {
    pub fn is_context(&self, n: u32) -> bool {
        self.tag_class == tag::CONTEXT && self.tag_number == n
    }
    /// `n` is the base universal tag (e.g. `tag::SEQUENCE` == 0x10), which is
    /// what X.509 callers think in terms of; the constructed bit and tag class
    /// are checked separately so both primitive and constructed forms match.
    pub fn is_universal(&self, n: u8) -> bool {
        self.tag_class == tag::UNIVERSAL && self.tag_byte & 0x1f == n
    }
    /// Reinterpret the contents of an implicitly tagged field as a TLV.
    pub fn inner(&self) -> Option<Tlv<'a>> {
        parse_tlv_at(self.record, self.value_start).ok()
    }
}

/// Parse the TLV beginning at absolute index `at` of `record`.
///
/// `record` is the whole buffer the hex dump draws from, so the "before" window
/// of any error reaches context preceding the failing TLV even when it sits
/// deep inside a certificate. Trailing bytes after the TLV are not an error
/// here — the caller decides, since a container holds several TLVs.
pub fn parse_tlv_at(record: &[u8], at: usize) -> Result<Tlv<'_>, DerError> {
    let buf = record.get(at..).unwrap_or(&[]);
    let base = at;
    if buf.is_empty() {
        return Err(located(DerErrorKind::Truncated, base, base, record, "empty buffer, no identifier octet"));
    }
    let tag_byte = buf[0];
    let tag_class = tag_byte & 0xc0;
    let constructed = tag_byte & tag::CONSTRUCTED != 0;
    let mut p = 1usize;

    // High-tag-number form: continuation octets carry 7 bits each, top bit set
    // on all but the last.
    let mut tag_number = (tag_byte & 0x1f) as u32;
    if tag_number == 0x1f {
        tag_number = 0;
        let mut octets = 0u32;
        loop {
            let Some(&b) = buf.get(p) else {
                return Err(located(DerErrorKind::Truncated, base + p, base + p, record, "tag continuation runs past end"));
            };
            p += 1;
            octets += 1;
            if octets > 4 {
                return Err(located(DerErrorKind::LengthOverflow, base + p, base + p, record, "tag number wider than 4 octets"));
            }
            // Shifting past 28 bits cannot yield a valid u32 tag number.
            if tag_number > 0x0fff_ffff {
                return Err(located(DerErrorKind::LengthOverflow, base + p, base + p, record, "tag number overflows u32"));
            }
            tag_number = (tag_number << 7) | (b & 0x7f) as u32;
            if b & 0x80 == 0 {
                break;
            }
        }
        if tag_number < 0x1f {
            return Err(located(
                DerErrorKind::NonMinimalLength, base + p, base + p, record,
                "high-tag-number form used for a tag number below 31",
            ));
        }
    }

    let Some(&len_first) = buf.get(p) else {
        return Err(located(DerErrorKind::Truncated, base + p, base + p, record, "missing length octet"));
    };
    p += 1;

    let length: usize;
    if len_first & 0x80 == 0 {
        length = len_first as usize;
    } else {
        let n = (len_first & 0x7f) as usize;
        if n == 0 {
            return Err(located(
                DerErrorKind::IndefiniteLength, base + p - 1, base + p - 1, record,
                "indefinite length is forbidden in DER",
            ));
        }
        if n > 8 {
            return Err(located(
                DerErrorKind::LengthOverflow, base + p - 1, base + p - 1, record,
                "length field wider than 8 octets",
            ));
        }
        if p + n > buf.len() {
            return Err(located(
                DerErrorKind::Truncated, base + p, base + p + n, record,
                "long-form length field runs past end",
            ));
        }
        // Accumulate in u64 and range-check once: n <= 8 octets cannot itself
        // overflow u64, and the usize comparison catches 32-bit targets.
        let mut acc: u64 = 0;
        for &b in &buf[p..p + n] {
            acc = (acc << 8) | b as u64;
        }
        if acc > usize::MAX as u64 {
            return Err(located(
                DerErrorKind::LengthOverflow, base + p - 1, base + p - 1, record,
                "length value overflows usize",
            ));
        }
        p += n;
        if acc < 0x80 {
            return Err(located(
                DerErrorKind::NonMinimalLength, base + p - 1, base + p - 1, record,
                "long form used for a length below 128",
            ));
        }
        if n > 1 && buf[p - n] == 0x00 {
            return Err(located(
                DerErrorKind::NonMinimalLength, base + p - n, base + p - 1, record,
                "long-form length has a leading zero octet",
            ));
        }
        length = acc as usize;
    }

    let Some(rel_end) = p.checked_add(length) else {
        return Err(located(
            DerErrorKind::LengthOverflow, base + p, base + p, record,
            "length + header overflows usize",
        ));
    };
    if rel_end > buf.len() {
        return Err(located(
            DerErrorKind::Truncated, base + p, base + rel_end, record,
            "declared value extends past end of record",
        ));
    }

    Ok(Tlv {
        tag_byte,
        tag_class,
        constructed,
        tag_number,
        start: base,
        value_start: base + p,
        value_end: base + rel_end,
        value: &buf[p..rel_end],
        record,
    })
}

/// Walk the children of a constructed TLV, recursing into nested constructed
/// values. Every child must consume exactly the parent's value: a mismatch is
/// the misaligned-nesting case and is reported with the offending offset.
pub fn validate_recursive(tlv: &Tlv<'_>, depth: usize) -> Result<(), DerError> {
    /// Guards against a length-domain cycle sending us into unbounded descent.
    const MAX_DEPTH: usize = 64;

    if depth > MAX_DEPTH {
        return Err(logical_error(
            DerErrorKind::NestedLengthMismatch,
            tlv.start,
            "nesting deeper than 64 levels, probable length-domain cycle",
        ));
    }
    if !tlv.constructed {
        return Ok(());
    }
    let mut off = 0usize;
    while off < tlv.value.len() {
        let child = parse_tlv_at(tlv.record, tlv.value_start + off)?;
        validate_recursive(&child, depth + 1)?;
        off = child.value_end - tlv.value_start;
    }
    if off != tlv.value.len() {
        return Err(logical_error(
            DerErrorKind::NestedLengthMismatch,
            tlv.value_start + off,
            "children overrun the parent's declared length",
        ));
    }
    Ok(())
}

/// Enumerate the direct children of a constructed TLV, stopping at the first
/// child that fails to parse (the caller has already validated the structure).
pub fn children<'a>(tlv: &Tlv<'a>) -> Vec<Tlv<'a>> {
    let mut out = Vec::new();
    let mut off = 0usize;
    while off < tlv.value.len() {
        match parse_tlv_at(tlv.record, tlv.value_start + off) {
            Ok(c) => {
                let advance = c.value_end - tlv.value_start;
                out.push(c);
                off = advance;
            }
            Err(_) => break,
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_short_form_sequence() {
        let d = [0x30, 0x03, 0x02, 0x01, 0x01];
        let t = parse_tlv_at(&d, 0).unwrap();
        assert!(t.is_universal(tag::SEQUENCE) && t.constructed);
        assert_eq!(t.value_end, 5);
        let kids = children(&t);
        assert_eq!(kids.len(), 1);
        assert_eq!(kids[0].value, &[1]);
    }

    #[test]
    fn parses_long_form_length() {
        let mut d = vec![0x04, 0x82, 0x01, 0x00];
        d.extend(std::iter::repeat(0xab).take(256));
        let t = parse_tlv_at(&d, 0).unwrap();
        assert_eq!(t.value.len(), 256);
        assert_eq!(t.value_end, 260);
    }

    #[test]
    fn offsets_are_absolute_under_nesting() {
        // SEQUENCE { OCTET STRING "hi" }, embedded after 100 filler bytes so
        // the absolute offsets are distinguishable from slice-relative ones.
        let d = [0x30, 0x04, 0x04, 0x02, b'h', b'i'];
        let mut record = vec![0u8; 100];
        record.extend_from_slice(&d);
        let outer = parse_tlv_at(&record, 100).unwrap();
        assert_eq!(outer.start, 100);
        assert_eq!(outer.value_start, 102);
        let kids = children(&outer);
        assert_eq!(kids[0].start, 102);
        assert_eq!(kids[0].value_start, 104);
    }

    #[test]
    fn rejects_indefinite_length() {
        let e = parse_tlv_at(&[0x30, 0x80], 0).unwrap_err();
        assert_eq!(e.kind, DerErrorKind::IndefiniteLength);
    }

    #[test]
    fn rejects_non_minimal_long_form() {
        assert_eq!(
            parse_tlv_at(&[0x04, 0x81, 0x01, 0x00], 0).unwrap_err().kind,
            DerErrorKind::NonMinimalLength
        );
        assert_eq!(
            parse_tlv_at(&[0x04, 0x82, 0x00, 0x80], 0).unwrap_err().kind,
            DerErrorKind::NonMinimalLength
        );
    }

    #[test]
    fn rejects_length_overflow() {
        // All-ones 8-octet length: header + length cannot be represented.
        let mut d = vec![0x04, 0x88];
        d.extend(std::iter::repeat(0xff).take(8));
        assert_eq!(parse_tlv_at(&d, 0).unwrap_err().kind, DerErrorKind::LengthOverflow);

        // A length wider than 8 octets is refused outright.
        let mut w = vec![0x04, 0x89];
        w.extend(std::iter::repeat(0x01).take(9));
        assert_eq!(parse_tlv_at(&w, 0).unwrap_err().kind, DerErrorKind::LengthOverflow);
    }

    #[test]
    fn huge_but_representable_length_reads_as_truncated() {
        // 2^63 does fit in a 64-bit usize; it simply overruns the record, which
        // is a truncation rather than an arithmetic overflow.
        let d = [0x04, 0x88, 0x80, 0, 0, 0, 0, 0, 0, 0, 0];
        assert_eq!(parse_tlv_at(&d, 0).unwrap_err().kind, DerErrorKind::Truncated);
    }

    #[test]
    fn rejects_truncation_with_absolute_offset() {
        let d = [0x30, 0x10, 0x02, 0x01];
        let e = parse_tlv_at(&d, 0).unwrap_err();
        assert_eq!(e.kind, DerErrorKind::Truncated);
        // The TLV starts at 2; its value would run to 18 but the record is 4.
        assert_eq!(e.offset, 2);
    }

    #[test]
    fn misaligned_nesting_is_reported_not_panicked() {
        // Outer SEQUENCE claims 5 bytes; inner OCTET STRING claims 8 and so
        // overruns the parent.
        let d = [0x30, 0x05, 0x04, 0x08, 0x01, 0x02, 0x03];
        let t = parse_tlv_at(&d, 0).unwrap();
        let err = validate_recursive(&t, 0).unwrap_err();
        assert_eq!(err.kind, DerErrorKind::Truncated);
        assert_eq!(err.offset, 4);
    }

    #[test]
    fn error_carries_hex_context() {
        // Record layout: a valid 256-byte OCTET STRING, then a TLV declaring 127
        // value bytes with only a single octet following it.
        let mut d = vec![0x04, 0x82, 0x01, 0x00];
        d.extend(std::iter::repeat(0xbb).take(256));
        let bad_at = d.len();
        d.extend_from_slice(&[0x04, 0x7f, 0xaa]);

        let err = parse_tlv_at(&d, bad_at).unwrap_err();
        assert_eq!(err.kind, DerErrorKind::Truncated);
        // The reported offset is the start of the failing value, one past the
        // tag+length header of the defective TLV.
        assert_eq!(err.offset, bad_at + 2);
        // `after` starts at that offset, widened to the failing TLV's declared
        // extent and then clamped to what the record holds.
        assert_eq!(err.after, "aa");
        // The "before" window reaches back over the full context radius.
        assert_eq!(err.before.split(' ').count(), HEX_DUMP_CONTEXT);
        // The window is anchored at the start of the failing value, so it ends
        // with the defective TLV's own tag and length octets — which is exactly
        // the context an operator needs to see the bogus length domain.
        assert!(
            err.before.ends_with(" 04 7f"),
            "before = ...{}",
            &err.before[err.before.len().saturating_sub(24)..]
        );
    }

    #[test]
    fn recursive_walk_accepts_wellformed() {
        let d = [0x30, 0x05, 0x30, 0x03, 0x02, 0x01, 0x07];
        let t = parse_tlv_at(&d, 0).unwrap();
        validate_recursive(&t, 0).unwrap();
    }

    #[test]
    fn high_tag_number_form() {
        let t = parse_tlv_at(&[0x9f, 0x1f, 0x01, 0xaa], 0).unwrap();
        assert_eq!(t.tag_number, 31);
        assert_eq!(t.value, &[0xaa]);
    }

    #[test]
    fn nesting_depth_is_bounded() {
        // 70 nested SEQUENCEs, innermost empty: legal DER, but past our guard.
        let mut d: Vec<u8> = Vec::new();
        for _ in 0..70 {
            d.push(0x30);
            d.push(0x00);
        }
        // Rebuild as properly nested: innermost first.
        let mut inner: Vec<u8> = Vec::new();
        for _ in 0..70 {
            let mut tlv = vec![0x30];
            let len = inner.len();
            if len < 0x80 {
                tlv.push(len as u8);
            } else {
                tlv.push(0x81);
                tlv.push(len as u8);
            }
            tlv.extend_from_slice(&inner);
            inner = tlv;
        }
        let t = parse_tlv_at(&inner, 0).unwrap();
        assert_eq!(validate_recursive(&t, 0).unwrap_err().kind, DerErrorKind::NestedLengthMismatch);
    }

    #[test]
    fn fuzz_never_panics() {
        // Deterministic xorshift over every short byte pattern and a large
        // random-ish corpus; the parser must return Err, never panic.
        let mut state: u64 = 0x2545_F491_4F6C_DD1D;
        let mut next = move || {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            state
        };
        for _ in 0..20_000 {
            let len = (next() % 96) as usize;
            let mut buf: Vec<u8> = (0..len).map(|_| (next() & 0xff) as u8).collect();
            if let Ok(t) = parse_tlv_at(&buf, 0) {
                // Never index outside the slice, whatever the length domain said.
                assert!(t.value_end <= buf.len());
                let _ = validate_recursive(&t, 0);
                let _ = children(&t);
            }
            // All-ones is the classic adversarial length-domain input.
            for f in [&mut buf[..len.min(4)]] {
                f.fill(0xff);
            }
            let _ = parse_tlv_at(&buf, 0);
        }
    }
}
