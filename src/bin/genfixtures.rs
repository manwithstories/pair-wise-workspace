//! 生成 ./fixtures 下的测试 DER 流。
//!
//! 产出真实结构的 X.509 v3 证书（手写 DER 编码，不依赖 openssl），
//! 并刻意混入坏记录，用于验证容错与去重。
//!
//! 用法：`cargo run --release --bin genfixtures -- ./fixtures [条数]`

use std::io::Write;
use std::path::Path;

// ---------- 最小 DER 编码助手 ----------

fn tlv(tag: u8, content: &[u8]) -> Vec<u8> {
    let mut o = vec![tag];
    let len = content.len();
    if len < 0x80 {
        o.push(len as u8);
    } else if len <= 0xff {
        o.push(0x81);
        o.push(len as u8);
    } else {
        o.push(0x82);
        o.push((len >> 8) as u8);
        o.push(len as u8);
    }
    o.extend_from_slice(content);
    o
}

fn seq(items: &[Vec<u8>]) -> Vec<u8> {
    tlv(0x30, &items.concat())
}

fn set(items: &[Vec<u8>]) -> Vec<u8> {
    tlv(0x31, &items.concat())
}

fn int(v: u64) -> Vec<u8> {
    let mut b = v.to_be_bytes().to_vec();
    // 去掉前导 0，再补符号位
    while b.len() > 1 && b[0] == 0 {
        b.remove(0);
    }
    if b[0] & 0x80 != 0 {
        b.insert(0, 0);
    }
    tlv(0x02, &b)
}

fn oid(arcs: &[u32]) -> Vec<u8> {
    let mut b = vec![(arcs[0] * 40 + arcs[1]) as u8];
    for &a in &arcs[2..] {
        let mut stack = Vec::new();
        let mut v = a;
        loop {
            stack.push((v & 0x7f) as u8);
            v >>= 7;
            if v == 0 {
                break;
            }
        }
        for (i, x) in stack.iter().rev().enumerate() {
            if i + 1 < stack.len() {
                b.push(x | 0x80);
            } else {
                b.push(*x);
            }
        }
    }
    tlv(0x06, &b)
}

fn utf8s(s: &str) -> Vec<u8> {
    tlv(0x0c, s.as_bytes())
}

fn bitstr(payload: &[u8]) -> Vec<u8> {
    let mut c = vec![0u8]; // unused bits
    c.extend_from_slice(payload);
    tlv(0x03, &c)
}

fn utc_time(year: i64, mon: u32, day: u32, h: u32, m: u32, s: u32) -> Vec<u8> {
    let yy = (year % 100) as u32;
    let text = format!("{:02}{:02}{:02}{:02}{:02}{:02}Z", yy, mon, day, h, m, s);
    tlv(0x17, text.as_bytes())
}

#[allow(dead_code)]
fn gen_time(year: i64, mon: u32, day: u32, h: u32, m: u32, s: u32) -> Vec<u8> {
    let text = format!("{:04}{:02}{:02}{:02}{:02}{:02}Z", year, mon, day, h, m, s);
    tlv(0x18, text.as_bytes())
}

/// Name ::= RDNSequence
fn name(cn: &str, o: &str) -> Vec<u8> {
    seq(&[
        set(&[seq(&[oid(&[2, 5, 4, 6]), utf8s(o)])]),
        set(&[seq(&[oid(&[2, 5, 4, 3]), utf8s(cn)])]),
    ])
}

/// 生成一个 modulus 为 n 字节的假 RSA 公钥。
fn rsa_pubkey(nbytes: usize) -> Vec<u8> {
    // 目标：模数**有效位长**恰为 nbytes*8。
    // DER INTEGER 最高位为 1 时须补一个 0x00 符号字节；该字节不计入有效位数，
    // 故模数本身取 nbytes 字节，首字节顶位置 1（如 0xff）。
    let modulus = vec![0xffu8; nbytes];
    seq(&[int_slice(&modulus), int(65537)])
}

fn int_slice(b: &[u8]) -> Vec<u8> {
    let mut v = b.to_vec();
    while v.len() > 1 && v[0] == 0 {
        v.remove(0);
    }
    if v[0] & 0x80 != 0 {
        v.insert(0, 0);
    }
    tlv(0x02, &v)
}

/// 构造一张自签 X.509 v3 证书（DER）。
#[allow(clippy::too_many_arguments)]
fn make_cert(
    serial: u64,
    issuer_cn: &str,
    subject_cn: &str,
    not_before: (i64, u32, u32),
    not_after: (i64, u32, u32),
    rsa_bytes: usize,
) -> Vec<u8> {
    // 注意区分两个不同的 OID：
    //   - SubjectPublicKeyInfo.algorithm 必须是 rsaEncryption (1.2.840.113549.1.1.1)
    //   - signatureAlgorithm           是 sha256WithRSAEncryption (...1.1.11)
    // 二者混用是真实证书里的常见笔误，会让公钥算法无法识别。
    let key_alg = seq(&[oid(&[1, 2, 840, 113549, 1, 1, 1]), tlv(0x05, &[])]);
    let sig_alg = seq(&[oid(&[1, 2, 840, 113549, 1, 1, 11]), tlv(0x05, &[])]);
    let alg = key_alg.clone();
    let spki = seq(&[alg.clone(), bitstr(&rsa_pubkey(rsa_bytes))]);
    let validity = seq(&[
        utc_time(not_before.0, not_before.1, not_before.2, 0, 0, 0),
        utc_time(not_after.0, not_after.1, not_after.2, 0, 0, 0),
    ]);
    let tbs = seq(&[
        tlv(0xa0, &int(2).to_vec()), // [0] version v3
        int(serial),
        sig_alg.clone(), // tbsCertificate.signature
        name(issuer_cn, "Audit Test CA"),
        validity,
        name(subject_cn, "Audit Test CA"),
        spki,
    ]);
    let sig = bitstr(&vec![0x5au8; 64]);
    seq(&[tbs, sig_alg, sig])
}

fn write_file(dir: &Path, name: &str, data: &[u8]) -> std::io::Result<()> {
    let p = dir.join(name);
    let mut f = std::fs::File::create(&p)?;
    f.write_all(data)?;
    Ok(())
}

fn main() -> std::io::Result<()> {
    let args: Vec<String> = std::env::args().collect();
    let dir = Path::new(args.get(1).map(|s| s.as_str()).unwrap_or("./fixtures"));
    let count: usize = args.get(2).and_then(|s| s.parse().ok()).unwrap_or(200);
    std::fs::create_dir_all(dir)?;

    // 1) 干净批次：200 张真实结构证书
    let mut clean = Vec::new();
    for i in 0..count {
        let yr = 2026i64;
        // not_after 落在 2026 全年，便于验证 --not-after-range 2026-01-01,2026-12-31
        let cert = make_cert(
            i as u64 + 1,
            "Audit Root CA",
            &format!("host-{i:04}.bank.internal"),
            (yr - 1, 1, 1),
            (yr, ((i % 12) + 1) as u32, 15),
            match i % 3 {
                0 => 256, // RSA-2048
                1 => 128, // RSA-1024  <- 弱密钥
                _ => 384, // RSA-3072
            },
        );
        clean.extend_from_slice(&cert);
    }
    write_file(dir, "batch-2026q1.der", &clean)?;

    // 2) 重复投递：与 batch-2026q1.der 完全相同，验证 sha256 去重零新增
    write_file(dir, "batch-2026q1-redelivery.der", &clean)?;

    // 3) 坏记录批次。
    //
    // 重要前提：拼接 DER 流中"长度域自洽但语义错误"的字节与合法记录**不可区分**。
    // 因此每种损坏形态都单独成文件，避免相互掩盖：
    //   - 截断：只可能在读到流尾时判定，故必须独占文件尾部；
    //   - 长度域溢出 / 错位嵌套：头部即畸形，可置于流中任意位置；
    //   - 纯垃圾：验证 resync 不挂死。
    let good = |serial: u64, cn: &str| {
        make_cert(serial, "Audit Root CA", cn, (2025, 1, 1), (2027, 1, 1), 256)
    };

    // 3a) 截断：好记录 + 文件末尾一条声明 900 字节、实际 4 字节的包
    let mut trunc = good(9001, "before-truncation");
    trunc.extend_from_slice(&[0x30, 0x84, 0x03, 0x84, 0x01]);
    write_file(dir, "batch-truncated.der", &trunc)?;

    // 3b) 长度域溢出 / 错位嵌套 / 非最小编码：头部即畸形，resync 可恢复
    let mut bad = good(9002, "before-malformed");
    // indefinite length（DER 非法）
    bad.extend_from_slice(&[0x30, 0x80, 0x00, 0x00, 0x00]);
    // 错位嵌套：外层 SEQ 声明 8 字节，内层 SEQ 声称 200 字节
    let mut mis = vec![0x30u8, 0x08, 0x30, 0xc8];
    mis.extend_from_slice(&[0x02, 0x01, 0x01, 0x05, 0x00]);
    bad.extend_from_slice(&mis);
    // 非最小长度编码：0x81 0x05 用于长度 5
    bad.extend_from_slice(&[0x30, 0x81, 0x05, 0x02, 0x01, 0x01, 0x00]);
    // 长格式长度域超 8 字节
    bad.extend_from_slice(&[0x04, 0x89, 1, 2, 3, 4, 5, 6, 7, 8, 9]);
    let good_tail = good(9003, "after-malformed");
    bad.extend_from_slice(&good_tail);
    write_file(dir, "batch-corrupt.der", &bad)?;


    // 4) 纯垃圾：验证 resync 不挂死
    write_file(dir, "batch-junk.der", &vec![0xffu8; 8192])?;

    let total: u64 = ["batch-2026q1.der", "batch-2026q1-redelivery.der", "batch-truncated.der", "batch-corrupt.der", "batch-junk.der"]
        .iter()
        .filter_map(|n| std::fs::metadata(dir.join(n)).ok())
        .map(|m| m.len() as u64)
        .sum();
    println!("fixtures written to {}", dir.display());
    println!("  batch-2026q1.der               {} 张好证书", count);
    println!("  batch-2026q1-redelivery.der    重复投递（应全部去重）");
    println!("  batch-truncated.der            末尾截断包");
    println!("  batch-corrupt.der              长度域溢出/错位嵌套/非最小编码");
    println!("  batch-junk.der                 8192 字节纯垃圾");
    println!("  合计 {} 字节", total);
    Ok(())
}
