# GARC 容器与 HDR 静态元数据格式规范 (v1)

本规范定义 `hdrcheck` 解析的自研媒体容器 GARC。夹具生成器与解析器均以本文件
为唯一事实来源；不引用任何外部媒体文件或 FFmpeg。

## 1. 字节序与整数

- 文件头声明字节序：`endianness` = 1 小端（LE），2 大端（BE）。多字节整数
  一律按声明字节序显式解码（`encoding/binary`）。
- 所有长度/偏移为 `uint32`，单位字节。解析时必须做溢出检查：
  `offset + length` 不得超过 `uint32` 上限，且不得超过实际文件长度。
- 变长整数使用 LEB128（无符号），单字段最多 5 字节（uint32 域），超出即
  `VARINT_OVERFLOW`。

## 2. 文件布局

```
+----------------------+
| FileHeader (64B)     |
+----------------------+
| Track Table          | track_count * 32B，绝对偏移 track_table_offset
+----------------------+
| Metadata Region      | [metadata_offset, metadata_offset+metadata_length)
+----------------------+
| SEI Region           | [sei_offset, sei_offset+sei_length)
+----------------------+
```

### 2.1 FileHeader（64 字节）

| 偏移 | 长度 | 字段 | 说明 |
|---|---|---|---|
| 0  | 4 | magic | 固定 ASCII "GARC" |
| 4  | 1 | version | 当前支持 1；其它值报 UNKNOWN_VERSION |
| 5  | 1 | endianness | 1=LE, 2=BE |
| 6  | 2 | flags | 位0=含 HDR 容器元数据；其余保留必须为 0 |
| 8  | 4 | file_size | 声明文件长度，须等于实际长度，否则 LENGTH_MISMATCH |
| 12 | 4 | track_count | 轨道数 |
| 16 | 4 | track_table_offset | 轨道表绝对偏移 |
| 20 | 4 | metadata_offset | 容器元数据区绝对偏移 |
| 24 | 4 | metadata_length | 元数据区长度 |
| 28 | 4 | sei_offset | SEI 区绝对偏移 |
| 32 | 4 | sei_length | SEI 区长度 |
| 36 | 4 | reserved | 保留，必须为 0 |
| 40 | 20 | reserved20 | 保留，必须全 0 |
| 60 | 4 | header_crc32 | 对偏移 [0,60) 的 IEEE CRC32 |

### 2.2 Track 表项（32 字节）

| 偏移 | 长度 | 字段 |
|---|---|---|
| 0 | 4 | track_id |
| 4 | 1 | track_type | 1=video，其它值跳过但保留 |
| 5 | 1 | hdr_flag | 1=该轨声明携带 HDR 元数据 |
| 6 | 2 | reserved16（必须 0） |
| 8 | 4 | sei_records | 该轨在 SEI 区的记录条数 |
| 12 | 4 | data_offset / 保留 |
| 16 | 4 | data_length / 保留 |
| 20 | 4 | reserved_a |
| 24 | 4 | reserved_b |

## 3. 元数据区（容器级，TLV 记录流）

元数据区由若干 TLV 记录顺序拼接：

| 字段 | 长度 |
|---|---|
| tag | uint16 |
| length | uint16（payload 字节数） |
| payload | length 字节 |
| crc32 | uint32，覆盖 tag||length||payload |

标签：

- `0x0001` MASTERING_DISPLAY，payload 固定 36 字节：
  - display_primaries: 6 × uint16（RGB 三色 x/y，每色 4 字节，共 24）
  - white_point: 2 × uint16，单位 0.0001 坐标（4 字节）
  - min_display_mastering_luminance: uint32，单位 0.0001 nit（4 字节）
  - max_display_mastering_luminance: uint32，单位 0.0001 nit（4 字节）
- `0x0002` CONTENT_LIGHT_LEVEL，payload 固定 4 字节：
  - max_content_light_level: uint16（nits）
  - max_frame_average_light_level: uint16（nits）
- `0x0003` UNIT_DIMENSION（量纲标记），payload 1 字节：必须为 1
  （表示坐标 1e-4、亮度 1e-4 nit / CLL 为 nit）；其它值 ILLEGAL_DIMENSION。
- 未知 tag：报 UNKNOWN_FIELD（审计模式保留位置继续解析）。
- 同一 tag 出现两次：DUPLICATE_FIELD（以首次为准，第二次报错）。
- TLV 越过元数据区或文件尾：OUT_OF_BOUNDS。
- 已知 tag 的 length 与规定 payload 不符：ILLEGAL_DIMENSION。

## 4. SEI 区（逐轨补充增强信息）

SEI 区由若干记录顺序拼接，每条记录：

| 字段 | 长度 |
|---|---|
| track_id | uint32 |
| seq_num | uint32（记录序号，同一轨内从 0 递增） |
| time_start | uint32（毫秒） |
| time_end | uint32（毫秒，须 > time_start） |
| payload_type | uint8（1=mastering display，2=content light level） |
| payload_len | LEB128 varint |
| payload | payload_len 字节 |
| crc32 | uint32，覆盖记录中 crc 之前全部字节 |

Payload（紧凑、无对齐填充）：

- type 1：6×uint16 原色 + 2×uint16 白点 + uint32 min + uint32 max = 36B
- type 2：uint16 max_cll + uint16 max_fall = 4B
- 未知 payload_type：UNKNOWN_FIELD，跳过该记录。
- crc 不符：CHECKSUM_ERROR，该记录字段全部丢弃（不参与合并）。
- 同一轨两条记录的 [time_start,time_end) 相交：RECORDS_OVERLAP。
- 记录指向不存在的 track_id：ORPHAN_SEI_RECORD。

## 5. 字段来源、缺省与合并

每个逻辑字段记录来源：`container` / `sei` / `default` / `missing`。

- 容器字段缺省而 SEI 存在：允许继承，来源标 `sei`。
- 容器与 SEI 均存在且相等：保留 container 来源，记 `sei` 佐证。
- 容器与 SEI 均存在但不等：CONFLICT，两个值都保留，禁止用默认值覆盖。
- 两边都没有：missing（不得写零值）。
- 容器区整体不存在（flags 位0=0）时，全部以 SEI 为准。

## 6. 范围检查

- 色坐标（原色/白点，0.0001 单位）：[0, 50000]。
- mastering min/max 亮度（0.0001 nit）：[0, 100_000_000]，且 max>=min。
- MaxCLL / MaxFALL（nit）：[1, 10000]；0 视为非法 VALUE_OUT_OF_RANGE。

## 7. 问题分级与退出码

等级：error（结构损坏）/ warning（业务不一致）。

- 退出码 0：无问题，或审计模式下仅有 warning。
- 退出码 2：业务冲突/缺省/越界值等 warning（严格模式 `--strict` 下遇任一
  warning 即 2；审计模式仍打印全部问题但退出 0）。
- 退出码 3：容器损坏或参数错误（截断、未知版本、校验和、越界等）。
- 退出码 4：内部异常。

## 8. 稳定输出

JSON 中所有数组按 (track_id, offset, code) 稳定排序；对象键固定顺序。
终端摘要包含：字段来源、最大内容亮度、问题等级、问题数量、输入 sha256。
