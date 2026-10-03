package com.carrier.sigstore;

import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.util.zip.CRC32;

/**
 * 固定尺寸磁盘页的编解码。
 *
 * <p>页头（24 字节）：
 * <pre>
 *   0  magic  (4)  固定 0x53494750 "SIGP"
 *   4  version(8)  该页版本的提交序号
 *  12  rowCount(4) 有效行数
 *  16  sealed (1)   行组是否已封口（封口后不可再追加）
 *  17  flags  (3)   保留
 *  20  crc32  (4)   负载区 CRC，用于识别半截刷盘
 * </pre>
 *
 * <p>所有多字节字段一律 BIG_ENDIAN：跨平台读回同一份文件必须得到同一批字节序列，
 * 这是"同一 WAL 重放两次得到同一结果"的基础。
 */
public final class Page {

    public static final int HEADER = 24;
    public static final int MAGIC = 0x53494750; // "SIGP"
    public static final int DEFAULT_PAGE_SIZE = 64 * 1024;
    public static final int MAX_PAYLOAD_DEFAULT = DEFAULT_PAGE_SIZE - HEADER;
    public static final int MIN_PAGE_SIZE = 4 * 1024;
    /** 页尺寸对齐粒度：让页在磁盘与内存里都对齐友好，同时不至于过度填充。 */
    public static final int ALIGN = 512;

    public final int pageSize;
    public final int payloadCapacity;

    public Page(int pageSize) {
        if (pageSize < HEADER + 64) throw new IllegalArgumentException("页尺寸过小: " + pageSize);
        this.pageSize = pageSize;
        this.payloadCapacity = pageSize - HEADER;
    }

    /**
     * 依据 schema 需要的负载字节数确定页尺寸。
     *
     * <p><b>页尺寸紧贴负载</b>，按 512 字节向上取整，并以 {@link #MIN_PAGE_SIZE} 兜底。
     * 这一点直接决定常驻内存：行组的负载是 {@code rowsPerPage x 行宽}，如果不分页对齐，
     * 一个 6 列 x 64 行的行组只要 2304 字节，却要占满 64KB 页——放大 28 倍。
     * 两百万行按 64KB 分页需要 11.7GB，直接缓冲根本放不下；
     * 紧凑分页后同样的数据只要约 72MB。
     *
     * <p>{@code requested > 0} 时以请求值为下限（供需要更大页的调用方使用）。
     */
    public static int pageSizeFor(int payloadBytes, int requested) {
        int need = HEADER + payloadBytes;
        int floor = requested > 0 ? requested : MIN_PAGE_SIZE;
        if (floor < need) floor = need;
        return (floor + ALIGN - 1) / ALIGN * ALIGN;
    }

    /**
     * 分配页缓冲。优先堆外——堆外页不受 GC 压力影响，适合常驻页缓存；
     * 但堆外内存有硬上限（{@code -XX:MaxDirectMemorySize}），
     * 超出时退回堆缓冲，让功能不因内存上限而不可用。
     *
     * <p>两种选择的可观测行为一致（同样的字节序、同样的页格式），
     * 因此这条降级路径不改变任何正确性保证。
     */
    public static ByteBuffer allocate(int pageSize) {
        try {
            return ByteBuffer.allocateDirect(pageSize).order(ByteOrder.BIG_ENDIAN);
        } catch (OutOfMemoryError | IllegalArgumentException e) {
            return ByteBuffer.allocate(pageSize).order(ByteOrder.BIG_ENDIAN);
        }
    }

    public static ByteBuffer wrap(byte[] src, int pageSize) {
        ByteBuffer bb = ByteBuffer.allocate(pageSize).order(ByteOrder.BIG_ENDIAN);
        bb.put(src, 0, Math.min(src.length, pageSize));
        bb.flip();
        return bb;
    }

    // ---- 页头读写 -------------------------------------------------------

    public static void writeHeader(ByteBuffer buf, long version, int rowCount, boolean sealed, int crc) {
        buf.order(ByteOrder.BIG_ENDIAN);
        buf.putInt(0, MAGIC);
        buf.putLong(4, version);
        buf.putInt(12, rowCount);
        buf.put(16, (byte) (sealed ? 1 : 0));
        buf.put(17, (byte) 0);
        buf.put(18, (byte) 0);
        buf.put(19, (byte) 0);
        buf.putInt(20, crc);
    }

    public static int readCrc(ByteBuffer buf) {
        return buf.order(ByteOrder.BIG_ENDIAN).getInt(20);
    }

    public static long readVersion(ByteBuffer buf) {
        return buf.order(ByteOrder.BIG_ENDIAN).getLong(4);
    }

    public static int readRowCount(ByteBuffer buf) {
        return buf.order(ByteOrder.BIG_ENDIAN).getInt(12);
    }

    public static boolean readSealed(ByteBuffer buf) {
        return buf.order(ByteOrder.BIG_ENDIAN).get(16) != 0;
    }

    /** 页头 magic 与负载 CRC 都对，才认为这一页是完整落地的。 */
    public static boolean isIntact(ByteBuffer buf) {
        ByteBuffer b = buf.duplicate().order(ByteOrder.BIG_ENDIAN);
        if (b.limit() < HEADER) return false;
        if (b.getInt(0) != MAGIC) return false;
        return computeCrc(b) == readCrc(b);
    }

    public static int computeCrc(ByteBuffer buf) {
        CRC32 c = new CRC32();
        ByteBuffer b = buf.duplicate().order(ByteOrder.BIG_ENDIAN);
        for (int i = HEADER; i < b.limit(); i++) c.update(b.get(i));
        return (int) c.getValue();
    }

    // ---- 列值读写（列主序，块内按下标连续） --------------------------------

    public static void putLong(ByteBuffer buf, int colOffset, int row, long value) {
        buf.order(ByteOrder.BIG_ENDIAN).putLong((int) (HEADER + colOffset + (long) row * 8L), value);
    }

    public static long getLong(ByteBuffer buf, int colOffset, int row) {
        return buf.order(ByteOrder.BIG_ENDIAN).getLong((int) (HEADER + colOffset + (long) row * 8L));
    }

    public static void putInt(ByteBuffer buf, int colOffset, int row, int value) {
        buf.order(ByteOrder.BIG_ENDIAN).putInt((int) (HEADER + colOffset + (long) row * 4), value);
    }

    public static int getInt(ByteBuffer buf, int colOffset, int row) {
        return buf.order(ByteOrder.BIG_ENDIAN).getInt((int) (HEADER + colOffset + (long) row * 4));
    }

    /** 逻辑行 -> long 取值（INT 列按位扩展，读侧零成本）。 */
    public static long readValue(ByteBuffer buf, Schema schema, int columnId, int row) {
        return schema.column(columnId).type() == Schema.Type.INT
                ? getInt(buf, schema.offsetOf(columnId), row)
                : getLong(buf, schema.offsetOf(columnId), row);
    }

    public static void writeValue(ByteBuffer buf, Schema schema, int columnId, int row, long value) {
        if (schema.column(columnId).type() == Schema.Type.INT) {
            putInt(buf, schema.offsetOf(columnId), row, (int) value);
        } else {
            putLong(buf, schema.offsetOf(columnId), row, value);
        }
    }

    /** 把整页复制成一个堆内快照，用于 WAL 记账与无锁读者。 */
    public static byte[] toBytes(ByteBuffer src) {
        ByteBuffer dup = src.duplicate().order(ByteOrder.BIG_ENDIAN);
        byte[] out = new byte[dup.limit()];
        dup.get(0, out, 0, out.length);
        return out;
    }

    @Override
    public String toString() {
        return "Page(" + pageSize + ")";
    }
}
