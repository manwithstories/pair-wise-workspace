/**
 * src/persist.ts —— 段文件的二进制编解码
 *
 * 每个段一个文件，形如 `seg-<generation>-<uuid>.srg`，写入采用
 * "临时文件 -> fsync -> rename" 的原子替换，保证进程崩溃时不会读到半成品段。
 *
 * 文件布局（小端）：
 *
 *   magic       : u32   'S','R','G','1'
 *   headerLen   : u32   JSON 头部长度
 *   header      : bytes JSON 段元数据（generation / docCount / deleted / sources / 字段名表）
 *   payload     : bytes 定长记录区
 *
 * payload 里每条记录固定宽度，倒排 posting 以 (offset, length) 引用 payload 区间，
 * 因此重载时不需要反序列化任何对象图，只需按偏移重建 TypedArray 视图 —— 这就是
 * "重启按段重载重建跳表" 的快路径。
 */

import { open, mkdir, readFile, rename, unlink, readdir } from "node:fs/promises";
import path from "node:path";
import { randomUUID } from "node:crypto";

/** 段文件魔数。 */
const MAGIC = 0x31475253; // 'S','R','G','1' 小端
const SUFFIX = ".srg";

/** 段文件头（JSON 序列化）。 */
export interface SegmentHeader {
  generation: number;
  docCount: number;
  /** tombstone 的段内 docId。 */
  deleted: number[];
  /** 本段合并来源。 */
  sources: number[];
  /** 数值字段列表（重建范围索引用）。 */
  numericFields: string[];
  /** 建了词级索引的文本字段（重建派生索引用）。 */
  textFields: string[];
  /** 字段名字典 + payload 里的偏移/长度。 */
  fields: Record<string, FieldEntry>;
  /** docId -> 外部 id 字符串。 */
  ids: string[];
  /** 原文 JSON 字符串，按 docId 对齐。 */
  docPayload: string[];
}

/** 单个 (field, term) 的落盘描述。 */
export interface FieldEntry {
  /** 该字段下 term 字典。 */
  terms: string[];
  /** 每个 term 的 posting 在 payload 里的起始偏移（相对 payload 区）。 */
  offsets: number[];
  /** 每个 term 的 posting 长度（docId 个数）。 */
  lengths: number[];
}

/**
 * 写盘所需的扁平化数据（由引擎从段内存结构拍平得到）。
 * header 就是完整的 SegmentHeader —— 字段字典已经在拍平阶段成型。
 */
export interface SegmentBlob {
  header: SegmentHeader;
  /** 全部 posting 的 docId 数据，按 fields 里的 offset/length 切分。 */
  postingData: Int32Array;
}

export interface SegmentFileInfo {
  file: string;
  generation: number;
  docCount: number;
}

/** 段文件名：`seg-<generation>-<uuid>.srg`，generation 便于排序与排查。 */
export function segmentFileName(generation: number): string {
  return `seg-${String(generation).padStart(12, "0")}-${randomUUID().slice(0, 8)}${SUFFIX}`;
}

/**
 * 原子写段文件：先写临时文件并 fsync，再 rename 到位。
 * 查询侧只会看到 rename 之后的完整文件，不会读到半成品。
 */
export async function writeSegment(dir: string, blob: SegmentBlob, fileName?: string): Promise<string> {
  await mkdir(dir, { recursive: true });
  const file = fileName ?? segmentFileName(blob.header.generation);
  const tmp = path.join(dir, `.tmp-${file}`);

  const headerJson = Buffer.from(JSON.stringify(blob.header), "utf8");
  const head = Buffer.alloc(12);
  head.writeUInt32LE(MAGIC, 0);
  head.writeUInt32LE(headerJson.length, 4);
  head.writeUInt32LE(blob.postingData.length, 8);

  const handle = await open(tmp, "w");
  try {
    await handle.write(head);
    await handle.write(headerJson);
    await handle.write(Buffer.from(blob.postingData.buffer, blob.postingData.byteOffset, blob.postingData.byteLength));
    await handle.sync();
  } finally {
    await handle.close();
  }
  await rename(tmp, path.join(dir, file));
  return file;
}

/** 读段文件，返回头部与 posting 数据。 */
export async function readSegment(dir: string, file: string): Promise<{ header: SegmentHeader; postingData: Int32Array }> {
  const buf = await readFile(path.join(dir, file));
  if (buf.length < 12) throw new Error(`段文件损坏（过短）: ${file}`);
  if (buf.readUInt32LE(0) !== MAGIC) throw new Error(`段文件魔数不匹配: ${file}`);
  const headerLen = buf.readUInt32LE(4);
  const postingLen = buf.readUInt32LE(8);
  const headerJson = buf.subarray(12, 12 + headerLen).toString("utf8");
  const header = JSON.parse(headerJson) as SegmentHeader;
  // postingData 按需对齐：Buffer 可能不是 4 字节起始，copy 一份保证 Int32Array 视图合法
  const start = 12 + headerLen;
  const copy = Buffer.from(buf.subarray(start, start + postingLen * 4));
  return { header, postingData: new Int32Array(copy.buffer, copy.byteOffset, postingLen) };
}

/** 列出目录下的段文件，按文件名（即 generation）升序。 */
export async function listSegmentFiles(dir: string): Promise<string[]> {
  let names: string[];
  try {
    names = await readdir(dir);
  } catch {
    return [];
  }
  return names.filter((n) => n.endsWith(SUFFIX)).sort();
}

/** 删除段文件（合并后回收旧段的物理空间）。 */
export async function removeSegmentFile(dir: string, file: string): Promise<void> {
  try {
    await unlink(path.join(dir, file));
  } catch {
    // 文件已不存在：合并是幂等的，忽略
  }
}
