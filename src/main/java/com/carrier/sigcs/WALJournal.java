package com.carrier.sigcs;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.channels.FileChannel;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.List;
import java.util.zip.CRC32;

/**
 * 预写日志（WAL）：先落日志、再改页缓存、最后异步回写数据文件。
 *
 * <h2>记录格式</h2>
 * <pre>
 *   header : type(1) | payloadLen(4) | lsn(8) | crc32(4)      = 17 字节
 *   payload: 随 type 而定
 * </pre>
 * <p>每条记录带 CRC，尾部还有一条 {@link Type#COMMIT} 标记事务提交。
 *
 * <h2>提交点</h2>
 * <p>事务的所有 {@link Type#PAGE} 记录 fsync 之后，才写 {@link Type#COMMIT} 记录并 fsync。
 * <b>COMMIT 记录落盘的那一刻即为提交点</b>：
 * <ul>
 *   <li>崩溃前 COMMIT 已落盘 =&gt; 已提交，必须完整回放；</li>
 *   <li>崩溃前 COMMIT 未落盘 =&gt; 未提交，其页记录一律丢弃。</li>
 * </ul>
 * 这个「要么全在、要么全不在」的原子性，正是避免「合并中途 OOM 导致某地市双倍计数」的关键：
 * 半截事务的页永远不会被重放。
 *
 * <h2>幂等</h2>
 * <p>每条页记录带全局递增 {@code lsn}。回放时若页上已记录的 {@code appliedLsn} &ge; 本条 lsn，
 * 说明这次重做已经做过，直接跳过 —— 重复回放同一份 WAL 得到同一份状态。
 */
final class WALJournal implements AutoCloseable {

    /** 记录类型。 */
    enum Type {
        /** 一个页的新版本内容。 */
        PAGE((byte) 1),
        /** 事务提交标记 —— 提交点。 */
        COMMIT((byte) 2);

        final byte code;

        Type(byte code) {
            this.code = code;
        }

        static Type of(byte code) {
            return switch (code) {
                case 1 -> PAGE;
                case 2 -> COMMIT;
                default -> null;
            };
        }
    }

    static final int HEADER_BYTES = 1 + 4 + 8 + 4;
    /** PAGE 记录的 payload 固定前缀：txId(8) + version(8) + pageId(4)。 */
    static final int PAGE_PREFIX = 8 + 8 + 4;
    /** COMMIT 记录的 payload：txId(8) + commitLsn(8)。 */
    static final int COMMIT_PAYLOAD = 8 + 8;

    private final Path path;
    private final CrashInjector injector;

    private FileChannel channel;
    private long nextLsn = 1L;

    WALJournal(Path path, CrashInjector injector) throws IOException {
        this.path = path;
        this.injector = injector;
        this.channel = FileChannel.open(path,
                StandardOpenOption.CREATE, StandardOpenOption.READ, StandardOpenOption.WRITE);
        this.nextLsn = scanLastLsn() + 1;
    }

    /** 打开已有 WAL 时，接续其 LSN 序列，避免与历史记录冲突。 */
    private long scanLastLsn() {
        long last = 0L;
        for (ScanRecord ignored : scan()) {
            last = ignored.lsn();
        }
        return last;
    }

    long nextLsn() {
        return nextLsn;
    }

    /**
     * 写一条页记录并 fsync。
     *
     * <p>这就是刷盘流水线的第一段：{@code WAL_APPEND}（进入 OS 缓冲）->
     * {@code WAL_FSYNC_DONE}（真正落盘）。
     *
     * @return 该记录的 lsn
     */
    long appendPage(long txId, long version, int pageId, byte[] content) throws IOException {
        long lsn = nextLsn++;
        int payloadLen = PAGE_PREFIX + content.length;
        ByteBuffer buf = ByteBuffer.allocate(HEADER_BYTES + payloadLen).order(ByteOrder.LITTLE_ENDIAN);
        writeHeader(buf, Type.PAGE, payloadLen, lsn);
        buf.putLong(txId).putLong(version).putInt(pageId).put(content);
        buf.flip();

        injector.checkpoint(CrashInjector.Step.WAL_APPEND);
        writeFully(buf);
        channel.force(false);            // fsync：此刻日志才真正耐久
        injector.checkpoint(CrashInjector.Step.WAL_FSYNC_DONE);
        return lsn;
    }

    /**
     * 写提交标记并 fsync —— <b>提交点</b>。返回提交 LSN。
     *
     * <p>一旦本方法正常返回，事务就算提交成功；在此之前的一切崩溃都不会让它的数据生效。
     */
    long appendCommit(long txId) throws IOException {
        long lsn = nextLsn++;
        ByteBuffer buf = ByteBuffer.allocate(HEADER_BYTES + COMMIT_PAYLOAD).order(ByteOrder.LITTLE_ENDIAN);
        writeHeader(buf, Type.COMMIT, COMMIT_PAYLOAD, lsn);
        buf.putLong(txId).putLong(lsn);
        buf.flip();

        injector.checkpoint(CrashInjector.Step.WAL_APPEND);
        writeFully(buf);
        channel.force(false);
        injector.checkpoint(CrashInjector.Step.WAL_FSYNC_DONE);
        return lsn;
    }

    /** CRC 只覆盖 header 中的 (type, payloadLen, lsn) —— 精确界定「哪几个字节必须完好」。 */
    private void writeHeader(ByteBuffer buf, Type type, int payloadLen, long lsn) {
        buf.put(type.code).putInt(payloadLen).putLong(lsn).putInt(crcOf(type.code, payloadLen, lsn));
    }

    private static int crcOf(byte type, int payloadLen, long lsn) {
        CRC32 crc = new CRC32();
        crc.update((int) type);
        crc.update(payloadLen);
        // long 按小端逐字节喂入，与磁盘上的字节序一致。
        for (int i = 0; i < 8; i++) {
            crc.update((int) (byte) (lsn >>> (8 * i)));
        }
        return (int) crc.getValue();
    }

    private void writeFully(ByteBuffer buf) throws IOException {
        long pos = channel.size();
        while (buf.hasRemaining()) {
            int n = channel.write(buf, pos);
            if (n <= 0) {
                throw new IOException("WAL write stalled at " + pos);
            }
            pos += n;
        }
    }

    // ------------------------------------------------------------------ 扫描

    /** 扫出的一条记录。 */
    record ScanRecord(Type type, long lsn, long txId, long version, int pageId, byte[] content) {
    }

    /**
     * 顺序扫描 WAL。
     *
     * <p>遇到 <b>截断或 CRC 不匹配</b> 的尾部就停止 —— 这正是「半截刷盘」在磁盘上的样子。
     * 崩溃时最后一条记录很可能只写了一半，把它当作有效记录就会把半截事务当成已提交，
     * 那正是「双倍计数 / 半截事务残留」的成因。丢弃不完整的尾部是正确的。
     */
    List<ScanRecord> scan() {
        List<ScanRecord> out = new ArrayList<>();
        byte[] all;
        try (var in = Files.newInputStream(path)) {
            all = in.readAllBytes();
        } catch (IOException e) {
            throw new IllegalStateException("cannot read WAL " + path, e);
        }

        int pos = 0;
        while (pos + HEADER_BYTES <= all.length) {
            byte typeCode = all[pos];
            Type type = Type.of(typeCode);
            if (type == null) {
                break; // 未知类型：尾部损坏
            }
            int payloadLen = (int) readIntLE(all, pos + 1);
            long lsn = readLongLE(all, pos + 5);
            int storedCrc = readIntLE(all, pos + 13);

            if (payloadLen < 0 || pos + HEADER_BYTES + payloadLen > all.length) {
                break; // 记录被截断（半截刷盘）
            }
            int payloadOff = pos + HEADER_BYTES;
            int expect = crcOf(typeCode, payloadLen, lsn);
            if (expect != storedCrc) {
                break; // 内容损坏：从这里往后的记录都不可信
            }

            if (type == Type.PAGE) {
                if (payloadLen < PAGE_PREFIX) {
                    break;
                }
                long txId = readLongLE(all, payloadOff);
                long version = readLongLE(all, payloadOff + 8);
                int pageId = readIntLE(all, payloadOff + 16);
                byte[] content = new byte[payloadLen - PAGE_PREFIX];
                System.arraycopy(all, payloadOff + PAGE_PREFIX, content, 0, content.length);
                out.add(new ScanRecord(Type.PAGE, lsn, txId, version, pageId, content));
            } else {
                if (payloadLen < COMMIT_PAYLOAD) {
                    break;
                }
                long txId = readLongLE(all, payloadOff);
                out.add(new ScanRecord(Type.COMMIT, lsn, txId, 0L, -1, new byte[0]));
            }
            pos = payloadOff + payloadLen;
        }
        return out;
    }

    private static int readIntLE(byte[] b, int off) {
        return (b[off] & 0xFF)
                | ((b[off + 1] & 0xFF) << 8)
                | ((b[off + 2] & 0xFF) << 16)
                | ((b[off + 3] & 0xFF) << 24);
    }

    private static long readLongLE(byte[] b, int off) {
        return (b[off] & 0xFFL)
                | ((b[off + 1] & 0xFFL) << 8)
                | ((b[off + 2] & 0xFFL) << 16)
                | ((b[off + 3] & 0xFFL) << 24)
                | ((b[off + 4] & 0xFFL) << 32)
                | ((b[off + 5] & 0xFFL) << 40)
                | ((b[off + 6] & 0xFFL) << 48)
                | ((b[off + 7] & 0xFFL) << 56);
    }

    long sizeBytes() throws IOException {
        return channel.size();
    }

    @Override
    public void close() throws IOException {
        if (channel.isOpen()) {
            channel.close();
        }
    }
}
