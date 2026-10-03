//! Minimal hand-rolled X.509 field extraction.
//!
//! Only the fields the audit needs are pulled out — validity window, issuer,
//! serial and public-key strength. Signature bytes are never verified: this is
//! an inventory/compliance scanner, not a path validator, and a CA dump may
//! legitimately contain certs whose chain we cannot build offline.

use crate::der_parser::{self, tag, DerError, DerErrorKind, Tlv};

/// Object identifiers we care about, in DER content form (without the 0x06 tag).
mod oid {
    pub const RSA_ENCRYPTION: &[u8] = &[0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01];
    pub const EC_PUBLIC_KEY: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01];
    pub const ED25519: &[u8] = &[0x2b, 0x65, 0x70];
    pub const CN: &[u8] = &[0x55, 0x04, 0x03];
    pub const O: &[u8] = &[0x55, 0x04, 0x0a];
    pub const OU: &[u8] = &[0x55, 0x04, 0x0b];
    pub const C: &[u8] = &[0x55, 0x04, 0x06];
    /// 2.5.29.19 basicConstraints
    pub const BASIC_CONSTRAINTS: &[u8] = &[0x55, 0x1d, 0x13];

    /// Named-curve OID -> bit strength.
    pub fn curve_bits(o: &[u8]) -> Option<u32> {
        match o {
            // 1.2.840.10045.3.1.7 prime256v1 / P-256
            [0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07] => Some(256),
            // 1.3.132.0.34 secp384r1 / P-384
            [0x2b, 0x81, 0x04, 0x00, 0x22] => Some(384),
            // 1.3.132.0.35 secp521r1 / P-521
            [0x2b, 0x81, 0x04, 0x00, 0x23] => Some(521),
            // 1.3.132.0.10 secp256k1
            [0x2b, 0x81, 0x04, 0x00, 0x0a] => Some(256),
            // 1.3.132.0.33 P-224
            [0x2b, 0x81, 0x04, 0x00, 0x21] => Some(224),
            _ => None,
        }
    }
}

/// The audit-relevant projection of one certificate.
#[derive(Debug, Clone)]
pub struct CertInfo {
    pub serial: Vec<u8>,
    /// Human-readable issuer, e.g. `CN=Example Root CA, O=Example Bank`.
    pub issuer: String,
    pub subject: String,
    /// Seconds since the Unix epoch, UTC.
    pub not_before: i64,
    pub not_after: i64,
    /// Modulus/curve strength in bits; 0 when the algorithm is unknown.
    pub key_size: i32,
    pub is_ca: bool,
    pub sig_alg: String,
}

fn located(kind: DerErrorKind, off: usize, detail: &'static str) -> DerError {
    DerError {
        kind,
        offset: off,
        before: String::new(),
        after: String::new(),
        detail,
    }
}

/// Parse the audit projection out of a complete DER certificate.
///
/// The caller has already validated the TLV structure; this walks the
/// certificate body positionally, the way X.509 mandates, so a malformed inner
/// field is reported at its own offset rather than failing the whole batch.
pub fn parse_certificate(der: &[u8]) -> Result<CertInfo, DerError> {
    let cert = der_parser::parse_tlv_at(der, 0)?;
    if !cert.is_universal(tag::SEQUENCE) || !cert.constructed {
        return Err(located(DerErrorKind::NotACertificate, 0, "outer element is not a SEQUENCE"));
    }
    let top = der_parser::children(&cert);
    if top.is_empty() {
        return Err(located(DerErrorKind::NotACertificate, 0, "certificate has no tbsCertificate"));
    }
    let tbs = top[0];
    if !tbs.is_universal(tag::SEQUENCE) {
        return Err(located(DerErrorKind::NotACertificate, tbs.start, "tbsCertificate is not a SEQUENCE"));
    }
    let f = der_parser::children(&tbs);

    let mut i = 0usize;
    // version [0] EXPLICIT INTEGER DEFAULT v1
    if i < f.len() && f[i].is_context(0) {
        i += 1;
    }
    if i >= f.len() {
        return Err(located(DerErrorKind::NotACertificate, tbs.start, "tbsCertificate truncated before serialNumber"));
    }
    let serial = if f[i].is_universal(tag::INTEGER) {
        f[i].value.to_vec()
    } else {
        return Err(located(DerErrorKind::NotACertificate, f[i].start, "serialNumber is not an INTEGER"));
    };
    i += 1;
    if i >= f.len() {
        return Err(located(DerErrorKind::NotACertificate, tbs.start, "tbsCertificate truncated before signature"));
    }
    let sig_alg = i;
    i += 1; // AlgorithmIdentifier
    if i >= f.len() {
        return Err(located(DerErrorKind::NotACertificate, tbs.start, "tbsCertificate truncated before issuer"));
    }
    let issuer_tlv = f[i];
    let issuer = render_name(&issuer_tlv);
    i += 1;
    if i >= f.len() {
        return Err(located(DerErrorKind::NotACertificate, tbs.start, "tbsCertificate truncated before validity"));
    }
    let validity = f[i];
    let vf = der_parser::children(&validity);
    if vf.len() < 2 {
        return Err(located(DerErrorKind::NotACertificate, validity.start, "validity needs notBefore and notAfter"));
    }
    let not_before = parse_time(&vf[0])?;
    let not_after = parse_time(&vf[1])?;
    i += 1;
    if i >= f.len() {
        return Err(located(DerErrorKind::NotACertificate, tbs.start, "tbsCertificate truncated before subject"));
    }
    let subject = render_name(&f[i]);
    i += 1;
    if i >= f.len() {
        return Err(located(DerErrorKind::NotACertificate, tbs.start, "tbsCertificate truncated before subjectPublicKeyInfo"));
    }
    let spki = f[i];
    let (key_size, _) = parse_spki(&spki)?;
    i += 1;

    // Optional [1],[2],[3] extensions: look for basicConstraints CA bit.
    let mut is_ca = false;
    while i < f.len() {
        let e = f[i];
        if e.is_context(3) {
            if let Some(inner) = e.inner() {
                if let Some(exts) = inner.inner() {
                    is_ca = has_ca_extension(&exts);
                }
            }
        }
        i += 1;
    }
    // Reuse the inner signature algorithm name; fall back to the outer one.
    let alg_name = if sig_alg < f.len() {
        alg_id_name(&f[sig_alg]).unwrap_or_else(|| "unknown".to_string())
    } else {
        "unknown".to_string()
    };

    if not_after < not_before {
        return Err(located(
            DerErrorKind::NestedLengthMismatch,
            validity.start,
            "notAfter precedes notBefore",
        ));
    }

    Ok(CertInfo {
        serial,
        issuer,
        subject,
        not_before,
        not_after,
        key_size,
        is_ca,
        sig_alg: alg_name,
    })
}

/// Detect basicConstraints with cA=TRUE among the extension sequences.
fn has_ca_extension(exts: &Tlv<'_>) -> bool {
    for ext in der_parser::children(exts) {
        let parts = der_parser::children(&ext);
        if parts.is_empty() {
            continue;
        }
        let oid = parts[0];
        if !oid.is_universal(tag::OID) || oid.value != oid::BASIC_CONSTRAINTS {
            continue;
        }
        // basicConstraints ::= SEQUENCE { cA BOOLEAN DEFAULT FALSE, ... }
        if parts.len() >= 2 && parts[1].is_universal(tag::SEQUENCE) {
            let inner = der_parser::children(&parts[1]);
            if let Some(b) = inner.first() {
                if b.is_universal(tag::BOOLEAN) && b.value.first().copied().unwrap_or(0) != 0 {
                    return true;
                }
            }
        }
    }
    false
}

/// Render a `Name` as `CN=..., O=...`, falling back to a hex digest so the
/// column is never empty and grouping by issuer still works.
fn render_name(name: &Tlv<'_>) -> String {
    let mut parts: Vec<String> = Vec::new();
    for rdn in der_parser::children(name) {
        for atv in der_parser::children(&rdn) {
            let kv = der_parser::children(&atv);
            if kv.len() < 2 {
                continue;
            }
            let key = kv[0].value;
            let val = String::from_utf8_lossy(kv[1].value).trim().to_string();
            let label = match key {
                x if x == oid::CN => "CN",
                x if x == oid::O => "O",
                x if x == oid::OU => "OU",
                x if x == oid::C => "C",
                _ => continue,
            };
            if !val.is_empty() && !parts.iter().any(|p| p == &format!("{label}={val}")) {
                parts.push(format!("{label}={val}"));
            }
        }
    }
    if parts.is_empty() {
        format!("OID:{}", &der_parser::hex_dump(name.value)[..name.value.len().min(24).max(0)])
    } else {
        parts.join(", ")
    }
}

/// Map an AlgorithmIdentifier OID to a short name.
fn alg_id_name(alg: &Tlv<'_>) -> Option<String> {
    let parts = der_parser::children(alg);
    let o = parts.first()?;
    if !o.is_universal(tag::OID) {
        return None;
    }
    Some(
        match o.value {
            x if x == oid::RSA_ENCRYPTION => "sha256WithRSAEncryption",
            x if x == oid::EC_PUBLIC_KEY => "ecdsa-with-SHA256",
            x if x == oid::ED25519 => "Ed25519",
            _ => "unknown",
        }
        .to_string(),
    )
}

/// Extract the public-key strength in bits from a SubjectPublicKeyInfo.
fn parse_spki(spki: &Tlv<'_>) -> Result<(i32, String), DerError> {
    let parts = der_parser::children(spki);
    if parts.len() < 2 {
        return Err(located(DerErrorKind::NotACertificate, spki.start, "SPKI needs algorithm and key bits"));
    }
    let alg = der_parser::children(&parts[0]);
    let oid = alg.first().map(|t| t.value).unwrap_or(&[]);
    let bits = parts[1];
    if !bits.is_universal(tag::BIT_STRING) || bits.value.is_empty() {
        return Err(located(DerErrorKind::NotACertificate, bits.start, "subjectPublicKey is not a BIT STRING"));
    }
    let body = &bits.value[1..]; // first octet is the unused-bit count

    if oid == oid::RSA_ENCRYPTION {
        // RSAPublicKey ::= SEQUENCE { modulus INTEGER, publicExponent INTEGER }
        let seq = match der_parser::parse_tlv_at(body, 0) {
            Ok(s) if s.is_universal(tag::SEQUENCE) => s,
            _ => return Err(located(DerErrorKind::NotACertificate, bits.start, "RSA key is not a SEQUENCE")),
        };
        let k = der_parser::children(&seq);
        let modulus = match k.first() {
            Some(m) if m.is_universal(tag::INTEGER) => m.value,
            _ => return Err(located(DerErrorKind::NotACertificate, seq.start, "RSA modulus missing")),
        };
        // DER INTEGERs are signed, so a leading 0x00 pad is not part of the size.
        let significant = match modulus.first() {
            Some(0x00) if modulus.len() > 1 => &modulus[1..],
            _ => modulus,
        };
        Ok(((significant.len() * 8) as i32, "RSA".to_string()))
    } else if oid == oid::EC_PUBLIC_KEY {
        let bits = match alg.get(1) {
            Some(p) if p.is_universal(tag::OID) => oid::curve_bits(p.value).unwrap_or(0),
            _ => 0,
        };
        Ok((bits as i32, "EC".to_string()))
    } else if oid == oid::ED25519 {
        Ok((256, "Ed25519".to_string()))
    } else {
        Ok((0, "unknown".to_string()))
    }
}

/// Parse UTCTime / GeneralizedTime into a Unix timestamp.
fn parse_time(t: &Tlv<'_>) -> Result<i64, DerError> {
    let b = t.value;
    let (y, rest) = if t.is_universal(tag::UTC_TIME) {
        if b.len() < 11 {
            return Err(located(DerErrorKind::Truncated, t.start, "UTCTime shorter than 11 octets"));
        }
        let yy = two(&b[0..2])?;
        // RFC 5280: 00-49 => 2000s, 50-99 => 1900s.
        let year = if yy >= 50 { 1900 + yy } else { 2000 + yy };
        (year as i64, &b[2..])
    } else if t.is_universal(tag::GENERALIZED_TIME) {
        if b.len() < 13 {
            return Err(located(DerErrorKind::Truncated, t.start, "GeneralizedTime shorter than 13 octets"));
        }
        (four(&b[0..4])? as i64, &b[4..])
    } else {
        return Err(located(DerErrorKind::NotACertificate, t.start, "validity field is not a Time"));
    };

    let month = two(&rest[0..2])?;
    let day = two(&rest[2..4])?;
    let hour = two(&rest[4..6])?;
    let min = two(&rest[6..8])?;
    // Seconds are optional in UTCTime; DER mandates them, but tolerate absence.
    let sec = if rest.len() >= 10 && rest[8].is_ascii_digit() { two(&rest[8..10])? } else { 0 };
    if !(1..=12).contains(&month) || !(1..=31).contains(&day) || hour > 23 || min > 59 || sec > 60 {
        return Err(located(DerErrorKind::NestedLengthMismatch, t.start, "time field out of range"));
    }
    Ok(days_from_civil(y, month, day) * 86_400 + hour as i64 * 3600 + min as i64 * 60 + sec as i64)
}

fn two(b: &[u8]) -> Result<u32, DerError> {
    if b.len() < 2 || !b[0].is_ascii_digit() || !b[1].is_ascii_digit() {
        return Err(located(DerErrorKind::NestedLengthMismatch, 0, "non-digit in time field"));
    }
    Ok(((b[0] - b'0') as u32) * 10 + (b[1] - b'0') as u32)
}

fn four(b: &[u8]) -> Result<u32, DerError> {
    if b.len() < 4 || !b.iter().all(|c| c.is_ascii_digit()) {
        return Err(located(DerErrorKind::NestedLengthMismatch, 0, "non-digit in time field"));
    }
    Ok(b.iter().fold(0u32, |a, c| a * 10 + (c - b'0') as u32))
}

/// Days from 1970-01-01 to y-m-d (proleptic Gregorian, Howard Hinnant's algorithm).
pub fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = (y - era * 400) as u64; // [0, 399]
    let mp = ((m + 9) % 12) as u64; // Mar=0
    let doy = (153 * mp + 2) / 5 + d as u64 - 1; // [0, 365]
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy; // [0, 146096]
    era * 146_097 + doe as i64 - 719_468
}

/// Inverse of [`days_from_civil`].
pub fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = (z - era * 146_097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

/// Render a Unix timestamp as `YYYY-MM-DD HH:MM:SS` UTC.
pub fn format_ts(ts: i64) -> String {
    let days = ts.div_euclid(86_400);
    let secs = ts.rem_euclid(86_400);
    let (y, m, d) = civil_from_days(days);
    format!(
        "{y:04}-{m:02}-{d:02} {:02}:{:02}:{:02}",
        secs / 3600,
        (secs % 3600) / 60,
        secs % 60
    )
}

/// Parse `YYYY-MM-DD` (or `YYYY-MM-DDTHH:MM:SS`) into a Unix timestamp.
pub fn parse_ts(s: &str) -> Option<i64> {
    let s = s.trim();
    let (date, time) = match s.split_once(&['T', ' '][..]) {
        Some((d, t)) => (d, t),
        None => (s, "00:00:00"),
    };
    let dp: Vec<&str> = date.split('-').collect();
    if dp.len() != 3 {
        return None;
    }
    let y: i64 = dp[0].parse().ok()?;
    let mo: u32 = dp[1].parse().ok()?;
    let d: u32 = dp[2].parse().ok()?;
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) {
        return None;
    }
    let tp: Vec<&str> = time.split(':').collect();
    let h: i64 = tp.first().and_then(|x| x.parse().ok()).unwrap_or(0);
    let mi: i64 = tp.get(1).and_then(|x| x.parse().ok()).unwrap_or(0);
    let se: i64 = tp.get(2).and_then(|x| x.parse().ok()).unwrap_or(0);
    Some(days_from_civil(y, mo, d) * 86_400 + h * 3600 + mi * 60 + se)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn civil_roundtrip() {
        for ts in [0i64, 1_000_000_000, 1_767_225_600, -86_400] {
            let days = ts.div_euclid(86_400);
            let (y, m, d) = civil_from_days(days);
            assert_eq!(days_from_civil(y, m, d), days, "roundtrip failed for {ts}");
        }
    }

    #[test]
    fn epoch_is_1970() {
        assert_eq!(days_from_civil(1970, 1, 1), 0);
        assert_eq!(days_from_civil(2000, 1, 1), 10_957);
    }

    #[test]
    fn ts_parse_and_format() {
        let t = parse_ts("2026-01-01").unwrap();
        assert_eq!(format_ts(t), "2026-01-01 00:00:00");
        let t2 = parse_ts("2026-12-31").unwrap();
        assert!(t2 > t);
        assert_eq!(parse_ts("garbage"), None);
        assert_eq!(parse_ts("2026-13-01"), None);
    }

    #[test]
    fn rfc5280_two_digit_year_window() {
        // 49 => 2049, 50 => 1950 (RFC 5280 §4.1.2.5.1).
        let a = b"490101000000Z";
        let b = b"500101000000Z";
        let ta = parse_time(&Tlv {
            tag_byte: 0x17, tag_class: tag::UNIVERSAL, constructed: false, tag_number: 23,
            start: 0, value_start: 2, value_end: 2 + a.len(), value: a, record: b"\x17\x0d490101000000Z",
        })
        .unwrap();
        let tb = parse_time(&Tlv {
            tag_byte: 0x17, tag_class: tag::UNIVERSAL, constructed: false, tag_number: 23,
            start: 0, value_start: 2, value_end: 2 + b.len(), value: b, record: b"\x17\x0d500101000000Z",
        })
        .unwrap();
        let (ya, _, _) = civil_from_days(ta.div_euclid(86_400));
        let (yb, _, _) = civil_from_days(tb.div_euclid(86_400));
        assert_eq!(ya, 2049);
        assert_eq!(yb, 1950);
    }

    #[test]
    fn generalized_time_is_supported() {
        // Some CAs emit post-2050 validity in GeneralizedTime form.
        let g = b"20491231235959Z";
        let t = parse_time(&Tlv {
            tag_byte: 0x18, tag_class: tag::UNIVERSAL, constructed: false, tag_number: 24,
            start: 0, value_start: 2, value_end: 2 + g.len(), value: g, record: b"\x18\x0f20491231235959Z",
        })
        .unwrap();
        let (y, m, d) = civil_from_days(t.div_euclid(86_400));
        assert_eq!((y, m, d), (2049, 12, 31));
    }
}
