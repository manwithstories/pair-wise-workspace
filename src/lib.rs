//! Financial PKI compliance audit core.
//!
//! 模块划分：
//! - [`der_parser`]：手写 DER TLV 遍历与长度域校验，坏记录定位。
//! - [`cert`]：X.509 字段抽取（有效期、签发者、密钥强度）。
//! - [`store`]：rusqlite schema 与事务封装。
//! - [`ingest`]：sha256 去重的幂等批量摄入。
//! - [`zone_map`]：块级 min/max 统计与跳过。
//! - [`checkpoint`]：原子落盘的断点续传。

pub mod cert;
pub mod checkpoint;
pub mod store;
pub mod der_parser;
pub mod ingest;
pub mod zone_map;
