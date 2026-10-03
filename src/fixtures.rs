//! Synthetic X.509 certificate construction.
//!
//! The audit needs DER that parses under a real TLV walker and carries genuine
//! validity windows, issuers and key strengths, so the fixtures are assembled
//! from actual DER encodings rather than opaque byte blobs. Signatures are
//! structurally present but not cryptographically valid: this is an inventory
//! scanner, and the fixtures exist to exercise framing, validation and querying.

use crate::der_parser::tag;

/// Encode a DER length from a raw content length.
pub fn der_len(len: usize) -> Vec<u8> {
    if len < 0x80 {
        vec![len as u8]
    } else {
        let b = len.to_be_bytes();
        let first = b.iter().position(|&x| x != 0).unwrap_or(b.len() - 1);
        let body = &b[first..];
        let mut out = Vec::with_capacity(body.len() + 1);
        out.push(0x80 | body.len() as u8);
        out.extend_from_slice(body);
        out
    }
}

/// A complete DER TLV.
pub fn tlv(tag_byte: u8, content: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(content.len() + 6);
    out.push(tag_byte);
    out.extend_from_slice(&der_len(content.len()));
    out.extend_from_slice(content);
    out
}

/// Concatenate pre-encoded TLVs into a SEQUENCE body.
pub fn seq(items: &[Vec<u8>]) -> Vec<u8> {
    let body: Vec<u8> = items.iter().flatten().copied().collect();
    tlv(0x30, &body)
}

/// Encode a small non-negative DER INTEGER.
pub fn integer(v: u64) -> Vec<u8> {
    let b = v.to_be_bytes();
    let first = b.iter().position(|&x| x != 0).unwrap_or(b.len() - 1);
    let mut body = b[first..].to_vec();
    // DER integers are signed: add a pad byte when the top bit is set.
    if body[0] & 0x80 != 0 {
        body.insert(0, 0x00);
    }
    tlv(0x02, &body)
}

/// Encode an arbitrary-length INTEGER from raw big-endian magnitude bytes.
pub fn integer_bytes(mag: &[u8]) -> Vec<u8> {
    let mut body = mag.to_vec();
    while body.len() > 1 && body[0] == 0 && body[1] & 0x80 == 0 {
        body.remove(0);
    }
    if body.is_empty() {
        body.push(0);
    }
    if body[0] & 0x80 != 0 {
        body.insert(0, 0x00);
    }
    tlv(0x02, &body)
}

/// OBJECT IDENTIFIER from its already-encoded content octets.
pub fn oid(content: &[u8]) -> Vec<u8> {
    tlv(0x06, content)
}

/// BIT STRING with no unused trailing bits.
pub fn bit_string(content: &[u8]) -> Vec<u8> {
    let mut body = Vec::with_capacity(content.len() + 1);
    body.push(0x00);
    body.extend_from_slice(content);
    tlv(0x03, &body)
}

/// PrintableString / UTF8String by universal tag.
pub fn string(tag_byte: u8, s: &str) -> Vec<u8> {
    tlv(tag_byte, s.as_bytes())
}

/// UTCTime or GeneralizedTime from a Unix timestamp.
pub fn time(ts: i64) -> Vec<u8> {
    let text = crate::x509::format_ts(ts);
    let (date, clock) = text.split_once(' ').expect("format_ts yields a space");
    let mut digits = String::from(date);
    digits.retain(|c| c.is_ascii_digit());
    let year: i64 = date[..4].parse().expect("4-digit year");
    let clock_digits: String = clock.chars().filter(|c| c.is_ascii_digit()).collect();

    if (1950..2050).contains(&year) {
        // RFC 5280 §4.1.2.5: UTCTime with a two-digit year in this window.
        let short = format!("{}{}{}{clock_digits}Z", &digits[2..4], &digits[4..6], &digits[6..8]);
        tlv(0x17, short.as_bytes())
    } else {
        let body = format!("{digits}{clock_digits}Z");
        tlv(0x18, body.as_bytes())
    }
}

const OID_SHA256_RSA: &[u8] = &[0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x0b];
const OID_EC_PUBKEY: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01];
const OID_RSA_ENCRYPTION: &[u8] = &[0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01];
const OID_BC: &[u8] = &[0x55, 0x1d, 0x13];
const OID_CN: &[u8] = &[0x55, 0x04, 0x03];
const OID_O: &[u8] = &[0x55, 0x04, 0x0a];
const OID_C: &[u8] = &[0x55, 0x04, 0x06];

/// One `type=value` attribute inside an RDN.
fn atv(oid_bytes: &[u8], value: &str) -> Vec<u8> {
    let inner = seq(&[oid(oid_bytes), string(tag::PRINTABLE_STRING, value)]);
    let set = tlv(0x31, &inner);
    set
}

/// A `Name` such as `C=CN, O=Example Bank, CN=Example Root CA`.
pub fn name(c: &str, o: &str, cn: &str) -> Vec<u8> {
    seq(&[atv(OID_C, c), atv(OID_O, o), atv(OID_CN, cn)])
}

/// Deterministic pseudo-random modulus of `bits` length.
fn modulus(bits: u32, seed: u8) -> Vec<u8> {
    let len = (bits / 8) as usize;
    let mut v = Vec::with_capacity(len);
    // xorshift, so fixtures are byte-identical across runs and machines.
    let mut s: u32 = 0x9e37_79b9 ^ (seed as u32).wrapping_mul(0x85eb_ca6b);
    for i in 0..len {
        s ^= s << 13;
        s ^= s >> 17;
        s ^= s << 5;
        v.push((s >> (i % 4 * 8)) as u8);
    }
    v[0] |= 0xc0; // keep the top bit set so the DER pad octet is exercised
    v
}

/// Build a structurally valid certificate.
///
/// Returns DER bytes that `der_parser` accepts and `x509::parse_certificate`
/// projects into an audit row.
pub fn build_cert(payload: &[u8]) -> Vec<u8> {
    let h = hash64(payload);
    let issuer = name("CN", "Example Bank", "Example Root CA");
    let subject = name("CN", "Example Bank", "Example Issuing CA");
    let nb = 1_600_000_000i64 + (h % 100_000) as i64;
    let na = nb + 365 * 86_400;
    build_cert_detailed(payload, &issuer, &subject, nb, na, 2048, true)
}

/// Fully specified certificate, used by the fixture generator to spread
/// validity windows and key strengths across the population.
#[allow(clippy::too_many_arguments)]
pub fn build_cert_detailed(
    payload: &[u8],
    issuer: &[u8],
    subject: &[u8],
    not_before: i64,
    not_after: i64,
    key_bits: u32,
    is_ca: bool,
) -> Vec<u8> {
    let h = hash64(payload);
    let serial = integer_bytes(&modulus(64, h as u8));

    let alg_rsa = seq(&[oid(OID_SHA256_RSA), tlv(0x05, &[])]);
    let validity = seq(&[time(not_before), time(not_after)]);

    // RSA keys report strength from the modulus length; EC keys report it from
    // the named curve. The threshold keeps RSA at >=1024 bits and routes the
    // deliberately weak fixtures down the EC path.
    let spki = if key_bits >= 1024 {
        let modulus_der = integer_bytes(&modulus(key_bits, h as u8));
        let rsa_pub = seq(&[modulus_der, integer(65537)]);
        let rsa_alg = seq(&[oid(OID_RSA_ENCRYPTION), tlv(0x05, &[])]);
        seq(&[rsa_alg, bit_string(&rsa_pub)])
    } else {
        // Small EC key: the strength comes from the named-curve OID, so emit a
        // curve that reports the requested size.
        let curve: &[u8] = match key_bits {
            0..=224 => &[0x2b, 0x81, 0x04, 0x00, 0x21],
            225..=256 => &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07],
            _ => &[0x2b, 0x81, 0x04, 0x00, 0x22],
        };
        let ec_alg = seq(&[oid(OID_EC_PUBKEY), oid(curve)]);
        let point = bit_string(&modulus(64, (h >> 8) as u8));
        seq(&[ec_alg, point])
    };

    // basicConstraints: SEQUENCE { BOOLEAN TRUE } when CA, else empty SEQUENCE.
    let bc_inner = if is_ca {
        tlv(0x01, &[0xff])
    } else {
        Vec::new()
    };
    let bc = seq(&[tlv(0x30, &bc_inner)]);
    let ext_seq = seq(&[seq(&[oid(OID_BC), tlv(0x01, &[0xff]), bc])]);
    let exts = tlv(0xa3, &ext_seq);

    let tbs = seq(&[
        tlv(0xa0, &integer(2)), // [0] EXPLICIT version v3
        serial,
        alg_rsa.clone(),
        issuer.to_vec(),
        validity,
        subject.to_vec(),
        spki,
        exts,
    ]);

    // Signature BIT STRING: right shape, deliberately not a valid signature.
    // The scanner inventories certificates and never verifies chains, so a
    // real signature would add a key-management dependency for no gain.
    seq(&[tbs, alg_rsa, bit_string(&modulus(256, (h >> 16) as u8))])
}

/// FNV-1a, used only to make fixture content deterministic.
pub fn hash64(data: &[u8]) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in data {
        h ^= *b as u64;
        h = h.wrapping_mul(0x1000_0000_01b3);
    }
    h
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lengths_use_shortest_form() {
        assert_eq!(der_len(0), vec![0x00]);
        assert_eq!(der_len(127), vec![0x7f]);
        assert_eq!(der_len(128), vec![0x81, 0x80]);
        assert_eq!(der_len(256), vec![0x82, 0x01, 0x00]);
        assert_eq!(der_len(65536), vec![0x83, 0x01, 0x00, 0x00]);
    }

    #[test]
    fn built_cert_parses_as_x509() {
        let der = build_cert(b"roundtrip");
        let info = crate::x509::parse_certificate(&der).unwrap();
        assert_eq!(info.issuer, "CN=Example Root CA, O=Example Bank");
        assert_eq!(info.subject, "CN=Example Issuing CA, O=Example Bank");
        assert!(info.not_after > info.not_before);
        assert_eq!(info.key_size, 2048);
        assert!(info.is_ca);
    }

    #[test]
    fn weak_and_strong_keys_report_their_size() {
        let issuer = name("CN", "Bank", "Root");
        let subj = name("CN", "Bank", "Leaf");
        for bits in [1024u32, 2048, 3072] {
            let der = build_cert_detailed(b"k", &issuer, &subj, 1_700_000_000, 1_800_000_000, bits, false);
            let info = crate::x509::parse_certificate(&der).unwrap();
            assert_eq!(info.key_size as u32, bits, "bits={bits}");
            assert!(!info.is_ca);
        }
    }

    #[test]
    fn certificate_structure_validates_recursively() {
        let der = build_cert(b"deep");
        let tlv = crate::der_parser::parse_tlv_at(&der, 0).unwrap();
        crate::der_parser::validate_recursive(&tlv, 0).expect("fixture must be structurally valid");
    }

    #[test]
    fn utc_and_generalized_time_selection() {
        // 2024 is inside the UTCTime window; 2070 is not.
        assert_eq!(time(1_700_000_000)[0], 0x17);
        let far = 4_000_000_000i64; // 2096
        assert_eq!(time(far)[0], 0x18);
    }

    #[test]
    fn integers_carry_a_pad_when_the_top_bit_is_set() {
        let der = integer(0xff);
        assert_eq!(der, vec![0x02, 0x02, 0x00, 0xff]);
    }
}

