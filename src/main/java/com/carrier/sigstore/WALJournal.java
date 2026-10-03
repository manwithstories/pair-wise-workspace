package com.carrier.sigstore;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.channels.FileChannel;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.locks.ReentrantLock;
import java.util.zip.CRC32;

/**
 * 预写日志（Write-Ahead Log）。
 *
 * <p>写路径严格遵守 WAL 规则：
 * <ol>
 *   <li>事务的页镜像以 {@code PAGE} 记录追加进日志；</li>
 *   <li>{@link #commit(long)} 追加 {@code COMMIT} 并 <b>force(false)</b>（fsync 到盘）；</li>
 *   <li>只有 fsync 返回后，事务的页才被允许进入页缓存。</li>
 * </ol>
 * 于是"已提交"在任何时刻都等价于"日志里存在一条已落盘的 COMMIT"，这是恢复的唯一依据。
 *
 * <p>记录格式（定长头 + 变长负载，全部 BIG_ENDIAN）：
 * <pre>
 *   0  lsn    (8)  全局单调日志序号
 *   8  type   (1)  见 Type
 *   9  txnId  (8)  事务号
 *  17  version(8)  该页版本 = 提交序号
 *  25  len    (4)  负载字节数
 *  29  payload(len)
 * 29+len crc32 (4)  覆盖前 29+len 字节
 * </pre>
 *
 * <p><b>半截记录</b>：尾部若因崩溃只写了一半，长度或 CRC 不匹配。回放时遇到即停止，
 * 之后所有 LSN 更大的记录一并丢弃——因为 WAL 是顺序追加的，半截之后必然没有完整事务。
 */
public final class WALJournal implements AutoCloseable {

    public static final int HEADER = 29;
    public static final int TRAILER = 4;

    public enum Type {
        BEGIN(1), PAGE(2), COMMIT(3), ABORT(4), CHECKPOINT(5);

        public final byte code;

        Type(int code) {
            this.code = (byte) code;
        }

        static Type of(byte code) {
            return switch (code) {
                case 1 -> BEGIN;
                case 2 -> PAGE;
                case 3 -> COMMIT;
                case 4 -> ABORT;
                case 5 -> CHECKPOINT;
                default -> null;
            };
        }
    }

    /** 一条已解码的日志记录。payload 仅对 PAGE 有意义。 */
    public record Record(long lsn, Type type, long txnId, long version, byte[] payload) {
        public int pageId() {
            return ByteBuffer.wrap(payload).order(ByteOrder.BIG_ENDIAN).getInt(0);
        }

        public int pageSize() {
            return ByteBuffer.wrap(payload).order(ByteOrder.BIG_ENDIAN).getInt(4);
        }

        public byte[] pageImage() {
            byte[] out = new byte[payload.length - PAGE_PREFIX];
            System.arraycopy(payload, PAGE_PREFIX, out, 0, out.length);
            return out;
        }

        /** 仅 COMMIT 有意义：本次提交之后的总行数。 */
        public long rowCount() {
            if (payload == null || payload.length < 8) return -1;
            return ByteBuffer.wrap(payload).order(ByteOrder.BIG_ENDIAN).getLong(0);
        }
    }

    private final Path path;
    private final FileChannel channel;
    /** 单写者锁：WAL 追加天然串行化，读路径完全不碰它。 */
    private final ReentrantLock appendLock = new ReentrantLock();
    private long nextLsn;
    private long appendPosition;
    private volatile boolean closed;

    public WALJournal(Path path) throws IOException {
        this.path = path;
        Files.createDirectories(path.toAbsolutePath().getParent());
        this.channel = FileChannel.open(path,
                StandardOpenOption.CREATE, StandardOpenOption.READ, StandardOpenOption.WRITE);
        this.appendPosition = channel.size();
        this.nextLsn = 1;
    }

    public synchronized long beginTxn(long txnId, long version) throws IOException {
        return append(txnId, Type.BEGIN, version, null);
    }

    /**
     * 记录一次页镜像。镜像携带 version，恢复时按 version 大小幂等去重：
     * 同一页被重放多次，结果与一次完全相同。
     */
    /** PAGE 负载前缀长度：pageId(4) + pageSize(4)。 */
    private static final int PAGE_PREFIX = 8;

    public long logPage(long txnId, int pageId, int pageSize, byte[] image, long version) throws IOException {
        // pageId 与 pageSize 都用 4 字节。早先版本把 pageSize 压成 2 字节，
        // 结果 64KiB(65536) 回绕成 0，恢复侧的页尺寸校验会把整条记录判为不匹配而丢弃。
        byte[] payload = new byte[PAGE_PREFIX + image.length];
        ByteBuffer.wrap(payload).order(ByteOrder.BIG_ENDIAN)
                .putInt(0, pageId)
                .putInt(4, pageSize);
        System.arraycopy(image, 0, payload, PAGE_PREFIX, image.length);
        return append(txnId, Type.PAGE, version, payload);
    }

    /**
     * 提交记录。负载带上"本次提交之后的总行数"，
     * 这样重启后可以精确重建 {@link CommitPoints}，而不是靠页 rowCount 反推。
     */
    public long commit(long txnId, long version, long rowCount) throws IOException {
        byte[] payload = new byte[8];
        ByteBuffer.wrap(payload).order(ByteOrder.BIG_ENDIAN).putLong(0, rowCount);
        return append(txnId, Type.COMMIT, version, payload);
    }

    public long abort(long txnId, long version) throws IOException {
        return append(txnId, Type.ABORT, version, null);
    }

    private long append(long txnId, Type type, long version, byte[] payload) throws IOException {
        appendLock.lock();
        try {
            if (closed) throw new IllegalStateException("WAL 已关闭");
            int len = payload == null ? 0 : payload.length;
            ByteBuffer buf = ByteBuffer.allocate(HEADER + len + TRAILER).order(ByteOrder.BIG_ENDIAN);
            long lsn = nextLsn++;
            buf.putLong(0, lsn);
            buf.put(8, type.code);
            buf.putLong(9, txnId);
            buf.putLong(17, version);
            buf.putInt(25, len);
            if (len > 0) buf.put(29, payload);
            CRC32 c = new CRC32();
            for (int i = 0; i < HEADER + len; i++) c.update(buf.get(i));
            buf.putInt(HEADER + len, (int) c.getValue());
            // 注意：上面全部是绝对定位写入（putLong(pos,...)），position 仍是 0。
            // 因此必须显式设置 limit，<b>不能</b>用 flip()——flip 会把 limit 置为当前
            // position(0)，导致 remaining()==0，一条字节都写不出去。
            int total = HEADER + len + TRAILER;
            buf.limit(total);

            // 追加位置在锁内一次性推进完；写循环也持锁，保证并发追加不会互相覆盖。
            long at = appendPosition;
            appendPosition = at + total;
            while (buf.hasRemaining()) {
                at += channel.write(buf, at);
            }
            return lsn;
        } finally {
            appendLock.unlock();
        }
    }

    /**
     * 把日志 fsync 到盘，并返回已落盘的最大 LSN。
     * 事务的 COMMIT 必须返回后，其页才允许进入页缓存。
     */
    public long sync() throws IOException {
        appendLock.lock();
        try {
            if (closed) throw new IllegalStateException("WAL 已关闭");
            channel.force(false);
            return nextLsn - 1;
        } finally {
            appendLock.unlock();
        }
    }

    /** 截断掉尾部残缺记录（例如上次崩溃留下的半截字节），让下一次追加有干净的基线。 */
    public void truncateTo(long position) throws IOException {
        appendLock.lock();
        try {
            channel.truncate(position);
            channel.force(false);
            synchronized (this) {
                appendPosition = position;
            }
        } finally {
            appendLock.unlock();
        }
    }

    public synchronized long appendPosition() {
        return appendPosition;
    }

    public synchronized long lastLsn() {
        return nextLsn - 1;
    }

    public long sizeBytes() throws IOException {
        return channel.size();
    }

    /**
     * 顺序回放日志。遇到 CRC 不符或长度不足的记录立即停止并把有效长度返回给调用方，
     * 以便 {@link #truncateTo(long)} 清理尾部。
     */
    public ReplayResult replay() throws IOException {
        long fileLen = channel.size();
        List<Record> records = new ArrayList<>();
        long validBytes = 0;
        long lastGoodLsn = 0;

        ByteBuffer head = ByteBuffer.allocate(HEADER).order(ByteOrder.BIG_ENDIAN);
        long pos = 0;
        while (pos + HEADER + TRAILER <= fileLen) {
            head.clear();
            readFully(head, pos);
            int len = head.getInt(25);
            if (len < 0 || pos + HEADER + len + TRAILER > fileLen) break;  // 半截负载

            int total = HEADER + len + TRAILER;
            ByteBuffer rec = ByteBuffer.allocate(total).order(ByteOrder.BIG_ENDIAN);
            readFully(rec, pos);

            CRC32 c = new CRC32();
            for (int i = 0; i < HEADER + len; i++) c.update(rec.get(i));
            if ((int) c.getValue() != rec.getInt(HEADER + len)) break;      // 半截/损坏

            long lsn = rec.getLong(0);
            Type type = Type.of(rec.get(8));
            if (type == null) break;
            long txnId = rec.getLong(9);
            long version = rec.getLong(17);
            byte[] payload = len == 0 ? null : new byte[len];
            if (len > 0) {
                ByteBuffer dup = rec.duplicate();
                dup.position(HEADER);
                dup.get(payload);
            }
            records.add(new Record(lsn, type, txnId, version, payload));
            lastGoodLsn = lsn;
            pos += total;
            validBytes = pos;
        }
        return new ReplayResult(List.copyOf(records), validBytes, lastGoodLsn, fileLen);
    }

    private void readFully(ByteBuffer buf, long pos) throws IOException {
        while (buf.hasRemaining()) {
            int n = channel.read(buf, pos + buf.position());
            if (n < 0) break;
        }
    }

    /** 回放结果 + 尾部清理信息。 */
    public record ReplayResult(List<Record> records, long validBytes, long lastGoodLsn, long fileLength) {
        public boolean hasTornTail() {
            return validBytes < fileLength;
        }
    }

    public Path path() {
        return path;
    }

    @Override
    public void close() throws IOException {
        appendLock.lock();
        try {
            if (closed) return;
            channel.force(true);
            closed = true;
            channel.close();
        } finally {
            appendLock.unlock();
        }
    }


}
