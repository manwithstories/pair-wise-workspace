package com.carrier.sigstore;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * 崩溃恢复：重启时把磁盘+WAL 重建成最后一个一致状态。
 *
 * <p>算法（单趟扫描，按 LSN 顺序）：
 * <ol>
 *   <li>回放 WAL，得到按 LSN 升序的记录序列，尾部半截记录被丢弃并截断；</li>
 *   <li>先扫数据文件，对每个 pageId 记录"页头 magic 有效 且 CRC 通过"的最新版本；</li>
 *   <li>再扫 WAL：只有出现在<b>已提交事务</b>里的 PAGE 记录才可覆盖数据页。
 *       这里的判定是恢复正确性的全部要害；</li>
 *   <li>同一页被多次重放时按 {@code version} 取最大——天然幂等；</li>
 *   <li>重放后的页写回数据文件并 fsync。</li>
 * </ol>
 *
 * <p><b>为什么半截事务不会有残留</b>：未提交事务的 PAGE 记录虽然已经 fsync 进了 WAL，
 * 但它们没有对应的 COMMIT 记录。第 3 步会把它们整体跳过，因此回滚后磁盘上不存在这批页的痕迹；
 * 内存侧同理——{@link WriteTransaction} 在 fsync 之前从未把页装进页缓存。
 *
 * <p><b>幂等性</b>：重复用同一份 WAL 恢复两次，得到完全相同的页内容，
 * 因为覆盖判据 {@code version} 只增不减，第二次重放全是 no-op。
 */
public final class RecoveryEngine {

    /** 恢复动作分类，测试据此断言"已提交保住、未提交干净"。 */
    public record Report(
            long lastCommitSeq,
            int committedTxns,
            int rolledBackTxns,
            int pagesReplayed,
            int pagesSkippedUncommitted,
            int tornTailBytes,
            int dataPagesRecovered
    ) {
        public boolean clean() {
            return rolledBackTxns == 0 && pagesSkippedUncommitted == 0;
        }
    }

    private RecoveryEngine() {
    }

    public static Report recover(Path dir, Schema schema) throws IOException {
        WALJournal wal;
        try {
            wal = new WALJournal(dir.resolve("wal.log"));
        } catch (IOException e) {
            return new Report(0, 0, 0, 0, 0, 0, 0);   // 从未写过：无 WAL 可恢复
        }
        try {
            return recover(dir, schema, wal);
        } finally {
            wal.close();
        }
    }

    static Report recover(Path dir, Schema schema, WALJournal wal) throws IOException {
        Page page = new Page(Page.pageSizeFor(schema.payloadBytes(), Page.MIN_PAGE_SIZE));
        int columnCount = schema.columnCount();

        // ---- 1. 回放 WAL ----
        WALJournal.ReplayResult replay = wal.replay();
        long torn = replay.fileLength() - replay.validBytes();
        if (replay.hasTornTail()) {
            wal.truncateTo(replay.validBytes());
        }

        // ---- 2. 判定哪些事务已提交 ----
        // 关键：先扫一遍全量记录确定"已提交事务集合"，再据此过滤 PAGE 记录。
        // 这样即使 COMMIT 出现在其 PAGE 之后（顺序正常）或之后（乱序），判定都成立。
        java.util.Set<Long> committedTxns = new java.util.TreeSet<>();
        java.util.Set<Long> abortedTxns = new java.util.TreeSet<>();
        java.util.Set<Long> begunTxns = new java.util.TreeSet<>();
        for (WALJournal.Record r : replay.records()) {
            switch (r.type()) {
                case BEGIN -> begunTxns.add(r.txnId());
                case COMMIT -> committedTxns.add(r.txnId());
                case ABORT -> abortedTxns.add(r.txnId());
                default -> { }
            }
        }
        committedTxns.removeAll(abortedTxns);
        int committedCount = committedTxns.size();
        int rolledBack = (int) begunTxns.stream().filter(t -> !committedTxns.contains(t)).count();

        // ---- 3. 收集候选页版本：数据文件 + 已提交 WAL ----
        // pageId -> (version, bytes)。TreeMap 保证后续处理顺序确定。
        Map<Integer, long[]> bestVersion = new TreeMap<>();
        Map<Integer, byte[]> bestBytes = new TreeMap<>();
        Map<Integer, Integer> bestRowCount = new TreeMap<>();
        Map<Integer, Boolean> bestSealed = new TreeMap<>();

        // 3a. 数据文件里 CRC 完好的页
        Path dataPath = dir.resolve("data.csf");
        int dataPages = 0;
        if (Files.exists(dataPath)) {
            try (var ch = java.nio.channels.FileChannel.open(dataPath, java.nio.file.StandardOpenOption.READ)) {
                long fileLen = ch.size();
                int pageSize = page.pageSize;
                long p = 0;
                ByteBuffer buf = Page.allocate(pageSize);
                while (p + pageSize <= fileLen) {
                    buf.clear();
                    int n = 0;
                    while (n < pageSize) {
                        int r = ch.read(buf, p + n);
                        if (r < 0) break;
                        n += r;
                    }
                    if (n < pageSize) break;      // 尾部半页
                    buf.flip();
                    if (Page.isIntact(buf)) {
                        int pageId = (int) (p / pageSize);
                        long version = Page.readVersion(buf);
                        byte[] img = new byte[pageSize];
                        ByteBuffer dup = buf.duplicate().order(ByteOrder.BIG_ENDIAN);
                        dup.get(img);
                        if (version >= versionOf(bestVersion, pageId)) {
                            bestVersion.put(pageId, new long[]{version});
                            bestBytes.put(pageId, img);
                            bestRowCount.put(pageId, Page.readRowCount(buf));
                            bestSealed.put(pageId, Page.readSealed(buf));
                        }
                        dataPages++;
                    }
                    p += pageSize;
                }
            }
        }

        // 3b. 已提交事务的 WAL 页记录，按 LSN 升序幂等覆盖
        int replayed = 0;
        int skippedUncommitted = 0;
        for (WALJournal.Record r : replay.records()) {
            if (r.type() != WALJournal.Type.PAGE) continue;
            if (!committedTxns.contains(r.txnId())) {
                skippedUncommitted++;           // 未提交事务的页：丢弃，不留残留
                continue;
            }
            int pageId = r.pageId();
            int pgSize = r.pageSize();
            if (pgSize != page.pageSize) continue;
            long version = r.version();
            // 幂等：同一页被多次重放时按 version 取最大。
            // 注意这一行在"正常 WAL"下其实不会生效——回放严格按 LSN 升序，
            // 而 version 来自同一个单调计数器，所以后到的必然更新。
            // 它真正防的是"日志被外部损坏/重排"这种异常输入：那时
            // 顺序不再保证，version 就是唯一的裁决依据。保留它是因为
            // 代价是一次整数比较，而去掉它会让正确性依赖外部假设。
            if (version < versionOf(bestVersion, pageId)) continue;
            ByteBuffer bb = ByteBuffer.wrap(r.pageImage()).order(ByteOrder.BIG_ENDIAN);
            if (!Page.isIntact(bb)) continue;   // 镜像自身损坏则不采用
            bestVersion.put(pageId, new long[]{version});
            bestBytes.put(pageId, r.pageImage());
            bestRowCount.put(pageId, Page.readRowCount(bb));
            bestSealed.put(pageId, Page.readSealed(bb));
            replayed++;
        }

        // ---- 4. 写回数据文件 ----
        long lastCommitSeq = 0;
        for (long v : committedTxns.isEmpty() ? new long[0] : committedSeqs(replay)) {
            lastCommitSeq = Math.max(lastCommitSeq, v);
        }

        Path out = dir.resolve("data.csf");
        try (var ch = java.nio.channels.FileChannel.open(out,
                java.nio.file.StandardOpenOption.CREATE,
                java.nio.file.StandardOpenOption.WRITE,
                java.nio.file.StandardOpenOption.READ)) {
            for (Map.Entry<Integer, byte[]> e : bestBytes.entrySet()) {
                int pageId = e.getKey();
                if (pageId % columnCount < 0 || pageId % columnCount >= columnCount) continue;
                long off = (long) pageId * page.pageSize;
                ByteBuffer b = ByteBuffer.wrap(e.getValue());
                int w = 0;
                while (w < b.remaining()) {
                    int n = ch.write(b, off + w);
                    if (n <= 0) break;
                    w += n;
                }
            }
            ch.force(true);
        }

        return new Report(lastCommitSeq, committedCount, rolledBack, replayed,
                skippedUncommitted, (int) torn, dataPages);
    }

    private static long[] committedSeqs(WALJournal.ReplayResult replay) {
        List<Long> out = new ArrayList<>();
        for (WALJournal.Record r : replay.records()) {
            if (r.type() == WALJournal.Type.COMMIT) out.add(r.version());
        }
        return out.stream().mapToLong(Long::longValue).toArray();
    }

    private static long versionOf(Map<Integer, long[]> m, int pageId) {
        long[] v = m.get(pageId);
        return v == null ? -1L : v[0];
    }

    /** 恢复报告的可读形式。 */
    public static String describe(Report r) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("lastCommitSeq", r.lastCommitSeq());
        m.put("committedTxns", r.committedTxns());
        m.put("rolledBackTxns", r.rolledBackTxns());
        m.put("pagesReplayed", r.pagesReplayed());
        m.put("pagesSkippedUncommitted", r.pagesSkippedUncommitted());
        m.put("tornTailBytes", r.tornTailBytes());
        m.put("dataPagesRecovered", r.dataPagesRecovered());
        return m.toString();
    }
}
