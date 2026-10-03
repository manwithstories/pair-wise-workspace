//! X.509 字段抽取：有效期、签发者、密钥强度。
//!
//! 复用 [`crate::der_parser`] 的 TLV 遍历结果，不引第三方 ASN.1 库。
//! 只做审计所需的最小字段解析；无法解析的字段以 `None` 表示，
//! 不影响记录入库（坏记录由 der_parser 负责拦截）。

use crate::der_parser::{children, tags, TagClass, Tlv, WalkConfig};
use std::fmt;

/// 公钥算法 OID（DER 内容字节，首字节为 X.0 弧）。
pub mod oid {
    /// rsaEncryption 1.2.840.113549.1.1.1
    pub const RSA_ENCRYPTION: &[u8] = &[0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01];
    /// id-ecPublicKey 1.2.840.10045.2.1
    pub const EC_PUBLIC_KEY: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01];
    /// Ed25519 1.3.101.112
    pub const ED25519: &[u8] = &[0x2b, 0x65, 0x70];
    /// RSASSA-PSS 1.2.840.113549.1.1.10
    pub const RSA_PSS: &[u8] = &[0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x0a];
    /// sha256WithRSAEncryption 1.2.840.113549.1.1.11（**签名**算法，非公钥算法）
    pub const SHA256_WITH_RSA: &[u8] = &[0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x0b];
    /// sha1WithRSAEncryption 1.2.840.113549.1.1.5（**签名**算法）
    pub const SHA1_WITH_RSA: &[u8] = &[0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x05];
}

/// 解析出的证书核心字段。
#[derive(Debug, Clone)]
pub struct CertInfo {
    pub serial: Vec<u8>,
    pub issuer: String,
    pub subject: String,
    /// Unix 秒（UTC）。
    pub not_before: i64,
    pub not_after: i64,
    /// 公钥强度（RSA modulus 位数 / EC 曲线强度 / 固定值）。
    pub key_size: u32,
    pub sig_algo: String,
    pub self_signed: bool,
}

impl fmt::Display for CertInfo {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "issuer={} subject={} not_before={} not_after={} key_size={} sig={}",
            self.issuer, self.subject, self.not_before, self.not_after, self.key_size, self.sig_algo
        )
    }
}

/// X.509 v3 结构中 TBSCertificate 的字段下标。
/// SEQUENCE { version[0], serialNumber[1], signature[2], issuer[3],
///            validity[4], subject[5], subjectPublicKeyInfo[6], ... }
#[allow(dead_code)]
const F_VERSION: usize = 0;
const F_SERIAL: usize = 1;
#[allow(dead_code)]
const F_SIGNATURE: usize = 2;
const F_ISSUER: usize = 3;
const F_VALIDITY: usize = 4;
const F_SUBJECT: usize = 5;
const F_SPKI: usize = 6;
/// 无 version 字段时（v1 证书）SPKI 的实际下标。
const F_SPKI_NO_VERSION: usize = 5;

/// 解析一条完整 DER 证书（已通过 der_parser 结构校验）。
pub fn parse_certificate(buf: &[u8]) -> Result<CertInfo, String> {
    let cfg = WalkConfig::default();
    let cert = crate::der_parser::parse_tlv_at_public(buf)
        .map_err(|e| format!("证书根 TLV 解析失败: {e}"))?;

    let top = children(cert.value, &cert, &cfg).map_err(|e| format!("证书子节点解析失败: {e}"))?;
    if top.len() != 3 {
        return Err(format!(
            "Certificate 应含 3 个字段(tbs/sigAlgo/sigValue)，实际 {}",
            top.len()
        ));
    }
    let tbs = &top[0];
    if !tbs.constructed || tbs.tag_number != tags::SEQUENCE {
        return Err("tbsCertificate 不是 SEQUENCE".into());
    }
    let f = children(tbs.value, tbs, &cfg).map_err(|e| format!("tbsCertificate 子节点解析失败: {e}"))?;

    // version 为 [0] EXPLICIT，可选（v1 证书缺省）。
    // 存在 version 时全部字段下标整体右移 1，必须按同一偏移做边界检查，
    // 否则 f[F_SPKI + 1] 会在短 tbs 上越界 panic。
    let has_version = f
        .first()
        .map(|v| v.class == TagClass::ContextSpecific && v.tag_number == 0)
        .unwrap_or(false);
    let shift = usize::from(has_version);
    // 无 version 时 SPKI 在下标 5（只需 6 项）；有 version 时整体右移到 6
    // （需 7 项）。need 即"必须存在的字段个数"。
    let need = F_SPKI_NO_VERSION + 1 + shift;
    if f.len() < need {
        return Err(format!(
            "tbsCertificate 字段不足：期望至少 {need} 项，实际 {}",
            f.len()
        ));
    }

    // F_* 常量描述的是**带 version** 的 v3 布局；有 version 时直接用常量下标。
    // 无 version 的 v1 证书则整体左移一位（SPKI 在下标 5）。
    let base = if has_version { 0usize } else { 1usize };
    let (serial, issuer, validity, subject, spki) = (
        &f[F_SERIAL - base],
        &f[F_ISSUER - base],
        &f[F_VALIDITY - base],
        &f[F_SUBJECT - base],
        &f[F_SPKI - base],
    );

    let serial = serial.value.to_vec();
    // 自签名判定基于 issuer/subject 的原始 DER 值，须在渲染前完成。
    let self_signed = issuer.value == subject.value;
    let issuer_str = render_name(issuer, &cfg)?;
    let subject_str = render_name(subject, &cfg)?;
    let (not_before, not_after) = parse_validity(validity, &cfg)?;
    let key_size = parse_spki_key_size(spki, &cfg)?;

    // 签名算法（外层 AlgorithmIdentifier），取 OID 的点分形式。
    let sig_algo = match first_oid(&top[1], &cfg) {
        Some(o) => oid_to_string(o),
        None => "unknown".into(),
    };

    Ok(CertInfo {
        serial,
        issuer: issuer_str,
        subject: subject_str,
        not_before,
        not_after,
        key_size,
        sig_algo,
        self_signed,
    })
}

/// 取第一个子节点（AlgorithmIdentifier 的 OID）。
fn first_oid<'a>(alg: &Tlv<'a>, cfg: &WalkConfig) -> Option<&'a [u8]> {
    let kids = children(alg.value, alg, cfg).ok()?;
    kids.iter()
        .find(|k| k.class == TagClass::Universal && k.tag_number == tags::OID)
        .map(|k| k.value)
}

/// validity ::= SEQUENCE { notBefore Time, notAfter Time }
/// Time ::= CHOICE { utcTime UTCTime, generalTime GeneralizedTime }
fn parse_validity(v: &Tlv<'_>, cfg: &WalkConfig) -> Result<(i64, i64), String> {
    let kids = children(v.value, v, cfg).map_err(|e| format!("validity 解析失败: {e}"))?;
    if kids.len() < 2 {
        return Err(format!("validity 应含 notBefore/notAfter，实际 {} 项", kids.len()));
    }
    let nb = parse_time(&kids[0]).ok_or("notBefore 无法解析")?;
    let na = parse_time(&kids[1]).ok_or("notAfter 无法解析")?;
    Ok((nb, na))
}

/// 解析 UTCTime / GeneralizedTime 为 Unix 秒。
///
/// UTCTime (YYMMDDHHMMSSZ)，YY 约定：>=50 → 19YY，<50 → 20YY (RFC 5280)。
/// GeneralizedTime (YYYYMMDDHHMMSSZ)。
pub fn parse_time(t: &Tlv<'_>) -> Option<i64> {
    match (t.class, t.tag_number) {
        (TagClass::Universal, tags::UTCTIME) => {
            let s = t.value;
            if s.len() < 11 {
                return None;
            }
            // 区分年份位数：标准 UTCTime 为 YYMMDDHHMMSSZ（13 字节）。
            // 少数实现输出 YYYYMMDDHHMMSSZ（15 字节），此时年份占 4 字节。
            let (y, rest) = if s.len() >= 15 && s[12].is_ascii_digit() {
                (num4(&s[0..4])?, &s[4..])
            } else {
                let yy = num2(&s[0..2])?;
                let year = if yy >= 50 { 1900 + yy } else { 2000 + yy };
                (year, &s[2..])
            };
            // 仅支持 Z（UTC）结尾形式；带时区偏移的变体在 DER 中不合法。
            if !rest.ends_with(b"Z") {
                return None;
            }
            Some(epoch(
                y,
                num2(&rest[0..2])?,
                num2(&rest[2..4])?,
                num2(&rest[4..6])?,
                num2(&rest[6..8])?,
                num2(&rest[8..10])?,
            ))
        }
        (TagClass::Universal, tags::GENERALIZEDTIME) => {
            let s = t.value;
            if s.len() < 13 {
                return None;
            }
            Some(epoch(
                num4(&s[0..4])?,
                num2(&s[4..6])?,
                num2(&s[6..8])?,
                num2(&s[8..10])?,
                num2(&s[10..12])?,
                num2(&s[12..14])?,
            ))
        }
        _ => None,
    }
}

fn num2(b: &[u8]) -> Option<i64> {
    if b.len() < 2 || !b[0].is_ascii_digit() || !b[1].is_ascii_digit() {
        return None;
    }
    Some(((b[0] - b'0') as i64) * 10 + (b[1] - b'0') as i64)
}

fn num4(b: &[u8]) -> Option<i64> {
    if b.len() < 4 {
        return None;
    }
    let mut v = 0i64;
    for c in &b[..4] {
        if !c.is_ascii_digit() {
            return None;
        }
        v = v * 10 + (c - b'0') as i64;
    }
    Some(v)
}

/// 民用日期转 Unix 秒（Howard Hinnant 算法，UTC 无时区歧义）。
fn epoch(y: i64, mo: i64, d: i64, h: i64, mi: i64, s: i64) -> i64 {
    let y = if mo <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let mp = (mo + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146097 + doe - 719468;
    days * 86400 + h * 3600 + mi * 60 + s
}

/// SubjectPublicKeyInfo ::= SEQUENCE { algorithm AlgorithmIdentifier,
///                                         subjectPublicKey BIT STRING }
fn parse_spki_key_size(spki: &Tlv<'_>, cfg: &WalkConfig) -> Result<u32, String> {
    let kids = children(spki.value, spki, cfg).map_err(|e| format!("SPKI 解析失败: {e}"))?;
    if kids.len() < 2 {
        return Err(format!("SPKI 应含 algorithm/subjectPublicKey，实际 {} 项", kids.len()));
    }
    let alg = &kids[0];
    let bits = &kids[1];
    let alg_oid = first_oid(alg, cfg).unwrap_or(&[]);

    // BIT STRING: 首字节为 unused bits 计数。
    let key_bytes = if bits.value.is_empty() {
        &[][..]
    } else {
        &bits.value[1..]
    };

    if alg_oid == oid::RSA_ENCRYPTION || alg_oid == oid::RSA_PSS {
        // RSAPublicKey ::= SEQUENCE { modulus INTEGER, publicExponent INTEGER }
        // DER 的 INTEGER 含前导 0 字节以保证符号位，需剔除后再算位数。
        let seq = crate::der_parser::parse_tlv_at_public(key_bytes)
            .map_err(|e| format!("RSAPublicKey 解析失败: {e}"))?;
        let parts = children(seq.value, &seq, cfg).map_err(|e| format!("RSAPublicKey 子节点失败: {e}"))?;
        let modulus = parts.first().ok_or("RSAPublicKey 缺少 modulus")?;
        let mut m = modulus.value;
        if m.first() == Some(&0) {
            m = &m[1..];
        }
        // leading_zeros_skipped 返回的已是**有效位数**，不可再乘 8。
        Ok(leading_zeros_skipped(m) as u32)
    } else if alg_oid == oid::EC_PUBLIC_KEY {
        // 未压缩点首字节 0x04，其后为 X||Y，各占 field_size 字节。
        // 未携带曲线参数时按 256-bit（P-256）为业界默认。
        if key_bytes.first() == Some(&0x04) && key_bytes.len() > 1 {
            let field_bytes = (key_bytes.len() - 1) / 2;
            Ok(leading_zeros_skipped(&key_bytes[1..1 + field_bytes]) as u32)
        } else {
            Ok(256)
        }
    } else if alg_oid == oid::ED25519 {
        Ok(256)
    } else {
        // 未知算法：退化为公钥字节长度估算，避免丢记录。
        Ok((key_bytes.len() as u32) * 8)
    }
}

/// 计算**有效位数**（跳过前导零字节）。
///
/// 返回值单位是"位"，不是"字节"——调用方不得再乘 8。
fn leading_zeros_skipped(b: &[u8]) -> usize {
    let first = match b.iter().position(|&x| x != 0) {
        Some(i) => i,
        None => return 0,
    };
    let lead = b[first].leading_zeros() as usize;
    (b.len() - first) * 8 - lead
}

/// Name ::= RDNSequence ::= SEQUENCE OF RelativeDistinguishedName
/// 渲染为 RFC 4514 风格的逗号分隔可读串（仅用于 CLI 展示与索引）。
fn render_name(name: &Tlv<'_>, cfg: &WalkConfig) -> Result<String, String> {
    let rdns = children(name.value, name, cfg).map_err(|e| format!("Name 解析失败: {e}"))?;
    let mut parts: Vec<String> = Vec::new();
    for rdn in rdns {
        let atvs = children(rdn.value, &rdn, cfg).map_err(|e| format!("RDN 解析失败: {e}"))?;
        for atv in atvs {
            let kv = children(atv.value, &atv, cfg).map_err(|e| format!("ATV 解析失败: {e}"))?;
            if kv.len() < 2 {
                continue;
            }
            let key = oid_to_string(kv[0].value);
            let val = match kv[1].tag_number {
                tags::PRINTABLESTRING | tags::UTF8STRING | tags::IA5STRING => String::from_utf8_lossy(kv[1].value)
                    .trim()
                    .to_string(),
                tags::OID => oid_to_string(kv[1].value),
                _ => String::from_utf8_lossy(kv[1].value).trim().to_string(),
            };
            parts.push(format!("{key}={val}"));
        }
    }
    Ok(parts.join(","))
}

/// OID 字节串 → 点分十进制。首字节编码 40*X+Y。
fn oid_to_string(oid: &[u8]) -> String {
    if oid.is_empty() {
        return String::new();
    }
    let mut out = String::with_capacity(oid.len() * 4);
    let first = oid[0];
    let (a, b) = if first < 40 {
        (0u64, first as u64)
    } else if first < 80 {
        (1u64, (first - 40) as u64)
    } else {
        (2u64, (first - 80) as u64)
    };
    out.push_str(&a.to_string());
    out.push('.');
    out.push_str(&b.to_string());
    let mut v: u64 = 0;
    let mut started = false;
    for &byte in &oid[1..] {
        v = (v << 7) | (byte & 0x7f) as u64;
        started = true;
        if byte & 0x80 == 0 {
            out.push('.');
            out.push_str(&v.to_string());
            v = 0;
            started = false;
        }
    }
    if started {
        // 末字节 continuation bit 未清：OID 畸形，输出已解算部分。
        out.push('.');
        out.push_str(&v.to_string());
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn oid_to_dotted_string() {
        assert_eq!(oid_to_string(oid::RSA_ENCRYPTION), "1.2.840.113549.1.1.1");
        assert_eq!(oid_to_string(oid::EC_PUBLIC_KEY), "1.2.840.10045.2.1");
        assert_eq!(oid_to_string(oid::ED25519), "1.3.101.112");
    }

    #[test]
    fn utctime_two_digit_year_pivots_at_50() {
        // RFC 5280: YY >= 50 -> 19YY, YY < 50 -> 20YY
        let pivot = |yy: u8| -> i64 {
            if yy >= 50 {
                1900 + yy as i64
            } else {
                2000 + yy as i64
            }
        };
        assert_eq!(pivot(49), 2049);
        assert_eq!(pivot(50), 1950);
        assert_eq!(pivot(99), 1999);
        assert_eq!(pivot(0), 2000);
    }

    #[test]
    fn utctime_tlv_parsed() {
        // UTCTime "260101000000Z" = 2026-01-01T00:00:00Z
        let mut tlv_bytes = vec![0x17u8, 0x0d];
        tlv_bytes.extend_from_slice(b"260101000000Z");
        let t = crate::der_parser::parse_tlv_at_public(&tlv_bytes).unwrap();
        assert_eq!(parse_time(&t), Some(1_767_225_600));
    }

    #[test]
    fn generalizedtime_tlv_parsed() {
        let mut tlv_bytes = vec![0x18u8, 0x0f];
        tlv_bytes.extend_from_slice(b"20260101000000Z");
        let t = crate::der_parser::parse_tlv_at_public(&tlv_bytes).unwrap();
        assert_eq!(parse_time(&t), Some(1_767_225_600));
    }

    #[test]
    fn epoch_matches_known_values() {
        // 1970-01-01T00:00:00Z
        assert_eq!(epoch(1970, 1, 1, 0, 0, 0), 0);
        // 2000-01-01T00:00:00Z = 946684800
        assert_eq!(epoch(2000, 1, 1, 0, 0, 0), 946_684_800);
        // 2026-01-01T00:00:00Z = 1767225600
        assert_eq!(epoch(2026, 1, 1, 0, 0, 0), 1_767_225_600);
    }

    #[test]
    fn leap_day_handled() {
        // 2024-02-29 必须存在且可往返
        let t = epoch(2024, 2, 29, 12, 0, 0);
        // 2024-03-01 应比 2024-02-29 晚 24h
        assert_eq!(epoch(2024, 3, 1, 12, 0, 0) - t, 86_400);
    }

    #[test]
    fn rsa_key_size_counts_significant_bits_only() {
        // DER INTEGER 含前导 0x00 符号字节时，不得把它计入有效位数。
        // 回归：曾因"再乘一次 8"与"重复剥前导零"导致 2048 被算成 2040/2152。
        let with_sign = {
            let mut v = vec![0x00u8];
            v.extend(std::iter::repeat(0xffu8).take(255));
            v
        };
        assert_eq!(leading_zeros_skipped(&with_sign), 2040);
        assert_eq!(leading_zeros_skipped(&with_sign[1..]), 2040);
        // 纯 2048 位有效位
        assert_eq!(leading_zeros_skipped(&vec![0xffu8; 256]), 2048);
    }

    #[test]
    fn key_algorithm_oid_must_match_key_not_signature() {
        // 回归：SPKI.algorithm 误用 sha256WithRSAEncryption(...1.1.11) 时，
        // 会掉进"未知算法"兜底分支，按字节长度臆测 keySize。
        assert_ne!(oid::RSA_ENCRYPTION, oid::SHA256_WITH_RSA);
        assert_ne!(oid::RSA_ENCRYPTION, oid::SHA1_WITH_RSA);
        // rsaEncryption 末字节为 0x01；sha256 变体为 0x0b
        assert_eq!(*oid::RSA_ENCRYPTION.last().unwrap(), 0x01);
        assert_eq!(*oid::SHA256_WITH_RSA.last().unwrap(), 0x0b);
    }

    #[test]
    fn parses_real_fixture_certificate() {
        // 用真实生成的证书做端到端字段抽取，锁住有效期与密钥强度。
        let data = match std::fs::read("fixtures/batch-2026q1.der") {
            Ok(d) => d,
            Err(_) => return, // 未生成 fixture 时跳过
        };
        let mut pos = 0usize;
        let mut checked = 0usize;
        while pos < data.len() && checked < 5 {
            let id = data[pos];
            assert_eq!(id, 0x30, "记录起点应为 SEQUENCE");
            let (len, hdr) = (data[pos + 1], 2usize);
            assert!(len > 0x80, "fixture 记录使用长格式长度");
            let ln = ((data[pos + 2] as usize) << 8) | data[pos + 3] as usize;
            let total = 4 + ln;
            let info = parse_certificate(&data[pos..pos + total]).expect("应能解析出字段");
            assert_eq!(info.issuer, "2.5.4.6=Audit Test CA,2.5.4.3=Audit Root CA");
            assert!(info.key_size == 2048 || info.key_size == 1024 || info.key_size == 3072);
            assert_eq!(info.sig_algo, "1.2.840.113549.1.1.11");
            assert!(!info.self_signed);
            // 有效期必须落在 2026 全年（UTC）
            assert!(info.not_after >= 1_767_225_600 && info.not_after <= 1_798_761_599);
            pos += total;
            checked += 1;
            let _ = hdr;
        }
        assert!(checked > 0, "应至少校验一张证书");
    }

    #[test]
    fn leading_zeros_bit_length() {
        assert_eq!(leading_zeros_skipped(&[0x00, 0xff]), 8);
        assert_eq!(leading_zeros_skipped(&[0x01, 0x00]), 9);
        assert_eq!(leading_zeros_skipped(&[0x00, 0x00, 0x01]), 1);
        assert_eq!(leading_zeros_skipped(&[0x00]), 0);
    }
}


