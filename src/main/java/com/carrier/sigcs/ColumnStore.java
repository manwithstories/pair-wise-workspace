package com.carrier.sigcs;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.channels.FileChannel;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;

/**
 * 列族与页分配、事务提交管道、快照查询入口 —— 整个内核的门面。
 *
 * <h2>写路径</h2>
 * <pre>
 *   BEGIN -> 暂存页(事务私有, 不可见) -> WAL 记录 + fsync -> COMMIT 记录 + fsync
 *         -> [提交点] 发布到页缓存(MVCC, 原子替换) -> 异步回写数据文件
 * </pre>
 * <ul>
 *   <li><b>未提交不可见</b>：暂存区在提交点之前对读者完全不存在。</li>
 *   <li><b>读不阻塞写</b>：发布是一次 {@code ConcurrentHashMap.put}，读者无锁。</li>
 *   <li><b>回写可丢</b>：数据文件只是页缓存的持久化镜像；它落后了可以用 WAL 重做补齐，
 *       所以回写不需要参与正确性，只影响恢复时间。</li>
 * </ul>
 *
 * <h2>确定性</h2>
 * <p>聚合与排序一律走 {@link TreeMap} / 显式 {@link java.util.Comparator} 全序，
 * 绝不使用 {@code HashMap} / {@code HashSet} 遍历顺序来产出对外结果。
 */
public final class ColumnStore implements AutoCloseable {

    /** 建图阈值（位图），与 {@link BitmapIndex} 一致。 */
    public static final int MAX_BITMAP_CARDINALITY = BitmapIndex.MAX_BITMAP_CARDINALITY;

    /**
     * 数据文件中的页槽大小。
     *
     * <p>必须 &gt; 页内容长度上限，因为槽内布局是 {@code [len:4][content:len]}：
     * 页内容若恰好等于页槽大小就会写不下长度前缀。
     * 页缓存里存的是裸内容，不受这个前缀影响。
     */
    private static final int PAGE_SLOT_BYTES = 4096 + 4;

    private final Path dir;
    private final CrashInjector injector;
    private final SnapshotManager snapshots;
    private final WALJournal wal;
    private final RecoveryEngine recovery;

    /** 列族名 -> 该列族的位图索引。 */
    private final Map<String, BitmapIndex> families = new ConcurrentHashMap<>();

    /** 页号 -> 已应用的最大 WAL LSN（幂等重放判定）。 */
    private final Map<Integer, Long> appliedLsns = new ConcurrentHashMap<>();

    /** 数据文件：页镜像。落后不影响正确性。 */
    private final Path dataFile;

    private final AtomicLong nextTxId = new AtomicLong(0L);
    private final AtomicLong lastRecoveryMillis = new AtomicLong(-1L);

    private volatile FileChannel dataChannel;
    private volatile boolean closed;

    public ColumnStore(Path dir) throws IOException {
        this(dir, CrashInjector.DISABLED);
    }

    public ColumnStore(Path dir, CrashInjector injector) throws IOException {
        this.dir = dir;
        this.injector = injector;
        Files.createDirectories(dir);
        this.snapshots = new SnapshotManager(1L << 20);
        this.wal = new WALJournal(dir.resolve("wal.log"), injector);
        this.dataFile = dir.resolve("pages.dat");
        this.recovery = new RecoveryEngine(wal);
        this.dataChannel = FileChannel.open(dataFile,
                StandardOpenOption.CREATE, StandardOpenOption.READ, StandardOpenOption.WRITE);
    }

    /**
     * 启动恢复。必须在对外提供读服务之前调用一次。
     */
    public RecoveryEngine.Report recover() {
        RecoveryEngine.Report report = recovery.recover(this, appliedLsns);
        // 恢复阶段不开放读服务；全部页重做完后一次性推进水位，
        // 避免重做过程中被读者看到「重做了一半」的中间状态。
        snapshots.publishRecoveredUpTo(recoveredWatermark.get());
        lastRecoveryMillis.set(report.durationMillis());
        return report;
    }

    /** 上次恢复耗时（毫秒）；未恢复过为 -1。 */
    public long lastRecoveryMillis() {
        return lastRecoveryMillis.get();
    }

    // ------------------------------------------------------------------ 列族

    /**
     * 创建一个列族并为其中的低基数列建位图。
     *
     * @param family    列族名（例如 {@code "cdr"}）
     * @param columns   列名 -> 每行取值
     * @param rowCount  行数
     * @param numeric   列名集合中可按数值序分段的列
     */
    public void createFamily(String family, Map<String, long[]> columns, int rowCount, java.util.Set<String> numeric) {
        BitmapIndex idx = new BitmapIndex(rowCount);
        for (var e : columns.entrySet()) {
            idx.buildColumn(e.getKey(), e.getValue(), numeric.contains(e.getKey()));
        }
        families.put(family, idx);
        // 保留列的原始取值，供聚合/维度读取使用（复制一份，隔离外部修改）。
        Map<String, long[]> snapshot = new LinkedHashMap<>();
        for (var e : columns.entrySet()) {
            snapshot.put(e.getKey(), e.getValue().clone());
        }
        familyColumns.put(family, snapshot);
    }

    public BitmapIndex family(String family) {
        BitmapIndex idx = families.get(family);
        if (idx == null) {
            throw new IllegalArgumentException("unknown family: " + family);
        }
        return idx;
    }

    // ------------------------------------------------------------------ 事务

    /** 一个写事务：暂存页直到提交点。 */
    public final class Txn implements AutoCloseable {
        private final long txId = nextTxId.incrementAndGet();
        private final Map<Integer, byte[]> staged = new TreeMap<>();
        private boolean committed;
        private boolean aborted;

        public long txId() {
            return txId;
        }

        /**
         * 写一个页。数据只进事务私有暂存区，此刻对任何读者不可见。
         */
        public Txn put(int pageId, byte[] content) {
            if (committed || aborted) {
                throw new IllegalStateException("txn " + txId + " already finished");
            }
            staged.put(pageId, content.clone());
            return this;
        }

        /** 暂存的页数（测试断言用）。 */
        public int stagedPages() {
            return staged.size();
        }

        /**
         * 提交：WAL 落盘 -> 提交点 -> 发布到页缓存 -> 异步回写。
         */
        public void commit() {
            if (committed || aborted) {
                throw new IllegalStateException("txn " + txId + " already finished");
            }
            // 整个事务只分配一个版本号：提交后该事务的所有页在快照里同时可见，
            // 不会出现「页A已更新、页B还是旧值」的中间态。
            final long txnVersion = nextVersion();
            try {
                injector.checkpoint(CrashInjector.Step.PRE_COMMIT);

                // 1) 预写日志：每个页一条记录，逐条 fsync。
                for (var e : staged.entrySet()) {
                    long lsn = wal.appendPage(txId, txnVersion, e.getKey(), e.getValue());
                    pendingLsnByPage.put(e.getKey(), lsn);
                }

                // 2) 提交点：COMMIT 记录 fsync 成功即提交。
                wal.appendCommit(txId);

                committed = true;

                // 3) 发布到页缓存：先把所有页挂进版本链，**再**推进可见水位。
                //    顺序至关重要 —— 水位是读者快照号的来源，水位不动，新页就一个都看不见。
                for (var e : staged.entrySet()) {
                    snapshots.publish(e.getKey(), txnVersion, e.getValue());
                    appliedLsns.put(e.getKey(), pendingLsnByPage.get(e.getKey()));
                }
                snapshots.makeVisible(txnVersion);

                // 4) 异步回写数据文件（可丢，恢复时由 WAL 补齐）。
                for (var e : staged.entrySet()) {
                    injector.checkpoint(CrashInjector.Step.PAGE_DIRTY);
                    writePageToDataFile(e.getKey(), e.getValue());
                    injector.checkpoint(CrashInjector.Step.PAGE_FSYNC_DONE);
                }
                dataChannel.force(false);
            } catch (CrashInjector.CrashException crash) {
                aborted = true;
                throw crash; // 进程视角：此处即「消失」
            } catch (IOException io) {
                aborted = true;
                throw new UncheckedIOException(io);
            }
        }

        /** 放弃事务：暂存页直接丢弃，绝不进入 WAL 可见区。 */
        public void abort() {
            aborted = true;
            staged.clear();
        }

        @Override
        public void close() {
            if (!committed && !aborted) {
                abort(); // 未显式 commit 的事务视为放弃
            }
            staged.clear();
        }
    }

    private final Map<Integer, Long> pendingLsnByPage = new ConcurrentHashMap<>();

    public Txn begin() {
        return new Txn();
    }

    private long nextVersion() {
        return snapshots.allocateVersion();
    }

    // ------------------------------------------------------------------ 恢复入口

    /** 供 {@link RecoveryEngine} 回写重做结果（绕过正常提交路径）。 */
    void applyRecoveredPage(int pageId, long version, byte[] content) {
        snapshots.publish(pageId, version, content);
        recoveredWatermark.accumulateAndGet(version, Math::max);
    }

    /** 恢复期间累积的可见水位；{@link #recover()} 结束时统一放行。 */
    private final AtomicLong recoveredWatermark = new AtomicLong(0L);

    // ------------------------------------------------------------------ 数据文件

    private void writePageToDataFile(int pageId, byte[] content) throws IOException {
        ByteBuffer buf = ByteBuffer.allocate(PAGE_SLOT_BYTES).order(ByteOrder.LITTLE_ENDIAN);
        buf.putInt(content.length).put(content);
        buf.position(0);
        long pos = (long) pageId * PAGE_SLOT_BYTES;
        while (buf.hasRemaining()) {
            int n = dataChannel.write(buf, pos);
            if (n <= 0) {
                throw new IOException("page write stalled");
            }
            pos += n;
        }
    }

    /** 读取数据文件中的页镜像；未刷盘返回 null。 */
    public byte[] readDataFilePage(int pageId) {
        try {
            ByteBuffer buf = ByteBuffer.allocate(PAGE_SLOT_BYTES);
            long pos = (long) pageId * PAGE_SLOT_BYTES;
            int read = dataChannel.read(buf, pos);
            if (read < 4) {
                return null;
            }
            buf.flip();
            int len = buf.getInt();
            if (len < 0 || len > PAGE_SLOT_BYTES - 4) {
                return null;
            }
            byte[] out = new byte[len];
            buf.get(out);
            return out;
        } catch (IOException e) {
            throw new UncheckedIOException(e);
        }
    }

    // ------------------------------------------------------------------ 查询

    /**
     * 开启读事务（无锁快照）。必须在 try-with-resources 中使用。
     */
    public SnapshotManager.ReadTransaction snapshot() {
        return snapshots.beginRead();
    }

    /** 读取某页在快照下的内容（返回新数组；不可见返回 null）。 */
    public byte[] readPage(int pageId, SnapshotManager.ReadTransaction tx) {
        return readPageBytes(pageId, tx.snapshot());
    }

    /**
     * 在给定快照下读取页内容，返回恰好长度的数组；不可见返回 null。
     *
     * <p>长度来自版本链，不从内容里猜 —— 页缓存中是裸字节，没有长度前缀。
     */
    byte[] readPageBytes(int pageId, long snapshot) {
        int len = snapshots.pageLength(pageId, snapshot);
        if (len < 0) {
            return null;
        }
        byte[] out = new byte[len];
        int n = snapshots.readInto(pageId, snapshot, out);
        if (n < 0) {
            return null;
        }
        return (n == len) ? out : java.util.Arrays.copyOf(out, n);
    }

    // ------------------------------------------------------------------ 统计

    /** 位图过滤累计耗时（纳秒），用于确认下推路径确实被走到。 */
    private final AtomicLong bitmapNanos = new AtomicLong();

    /** 位图过滤累计耗时（纳秒）。 */
    long bitmapFilterNanos() {
        return bitmapNanos.get();
    }

    /** 位图过滤：返回满足谓词的行号（升序）。未建图时返回 null 以便回退扫描。 */
    public List<Integer> filterRows(String family, List<BitmapIndex.Predicate> predicates) {
        long t0 = System.nanoTime();
        try {
            return family(family).intersect(predicates);
        } finally {
            bitmapNanos.addAndGet(System.nanoTime() - t0);
        }
    }

    // ------------------------------------------------------------------ 聚合

    /**
     * 一个聚合分组的结果。
     *
     * @param groupKey 分组键（按列值字典序拼接，保证全序）
     * @param bucket   时间桶（小时）
     * @param count    组内行数
     * @param sum      组内度量之和
     */
    public record AggRow(String groupKey, long bucket, long count, long sum) {
    }

    /**
     * 多维过滤 + 时间桶聚合，全程确定性。
     *
     * <p>这是数据组工程师那条查询的形态：{@code 地市 + 套餐 + 终端品牌} 组合筛选，
     * 再按小时分桶聚合。
     *
     * <p><b>确定性来自三处</b>：
     * <ol>
     *   <li>过滤走位图 AND，行号天然升序，与谓词书写顺序无关；</li>
     *   <li>聚合累加在<b>升序行号</b>上进行，因此浮点/整数累加顺序固定，结果逐位可复现；</li>
     *   <li>分组用 {@link TreeMap}，输出按 {@code groupKey} 的字典序 —— 绝不让
     *       {@code HashMap} 的哈希随机化泄漏到对外顺序。</li>
     * </ol>
     *
     * @param family     列族
     * @param groupCols  分组列（按给定次序拼接成 groupKey）
     * @param bucketCol  时间桶列（整除 {@code bucketWidth} 得到桶号）
     * @param bucketWidth 桶宽
     * @param measureCol 度量列
     * @param predicates 过滤谓词（可为空）
     */
    public List<AggRow> aggregate(String family,
                                  List<String> groupCols,
                                  String bucketCol,
                                  long bucketWidth,
                                  String measureCol,
                                  List<BitmapIndex.Predicate> predicates) {
        BitmapIndex idx = family(family);
        List<Integer> rows = idx.intersect(predicates);
        if (rows == null) {
            // 有未建图的谓词：无法用位图下推，调用方需先建图或改写谓词。
            throw new IllegalStateException(
                    "predicates reference non-indexed columns: " + unindexed(predicates, idx)
                            + " — bitmap pushdown unavailable");
        }

        // rows 已按行号升序；按此顺序累加，聚合结果可复现。
        //
        // 分组键必须同时包含 groupCols 和时间桶：一个分组会横跨多个时间桶，
        // 若只用 groupCols 做键，桶号会被后到的行覆盖，结果就依赖行顺序了。
        //
        // 排序键用 {@link GroupKey}（groupKey 升序 + bucket 升序）而不是拼接字符串：
        // 字符串比较下 "10" < "9"，会把桶 10 排到桶 9 前面。数值列一律零填充到
        // 固定宽度，使字典序与数值序一致 —— 排序结果既确定又符合直觉。
        Map<GroupKey, long[]> buckets = new TreeMap<>();
        for (int row : rows) {
            long bucket = valueAt(family, bucketCol, row) / bucketWidth;
            GroupKey key = new GroupKey(
                    groupKeyOf(family, groupCols, row),
                    sortKeyOf(family, groupCols, row),
                    bucket);
            long measure = valueAt(family, measureCol, row);
            long[] acc = buckets.computeIfAbsent(key, k -> new long[2]);
            acc[0]++;
            acc[1] += measure;
        }

        List<AggRow> out = new ArrayList<>(buckets.size());
        for (var e : buckets.entrySet()) { // TreeMap 迭代 = 键全序，确定
            long[] acc = e.getValue();
            out.add(new AggRow(e.getKey().display(), e.getKey().bucket(), acc[0], acc[1]));
        }
        return out;
    }

    /**
     * 构造分组键的<b>展示</b>形式：各分组列取值以 \u0001 连接，例如 {@code "3\u00011"}。
     *
     * <p>展示用原值（便于人读），排序用 {@link GroupKey} 内部的定长编码。
     */
    public String groupKeyOf(String family, List<String> groupCols, int row) {
        StringBuilder key = new StringBuilder();
        for (int i = 0; i < groupCols.size(); i++) {
            if (i > 0) {
                key.append('\u0001');
            }
            key.append(valueAt(family, groupCols.get(i), row));
        }
        return key.toString();
    }

    /**
     * 聚合的内部排序键。
     *
     * <p>比较用的是<b>定长数值编码</b>而非展示字符串：字符串比较下 {@code "10" < "9"}，
     * 会把桶 10 排到桶 9 前面。编码后字典序与数值序完全一致，
     * 排序结果既确定又符合直觉，同时展示字段保持原始可读取值。
     */
    private record GroupKey(String display, String sortKey, long bucket) implements Comparable<GroupKey> {
        @Override
        public int compareTo(GroupKey o) {
            // 先比分组列（定长编码，字典序 == 数值序），再比桶号。
            int c = sortKey.compareTo(o.sortKey);
            return c != 0 ? c : Long.compare(bucket, o.bucket);
        }
    }

    /**
     * 构造分组键的<b>排序</b>形式：各分组列取值零填充后以 \u0001 连接。
     *
     * <p>与 {@link #groupKeyOf} 的展示形式一一对应，但保证字典序等于数值序。
     */
    private String sortKeyOf(String family, List<String> groupCols, int row) {
        StringBuilder key = new StringBuilder();
        for (int i = 0; i < groupCols.size(); i++) {
            if (i > 0) {
                key.append('\u0001');
            }
            key.append(pad(valueAt(family, groupCols.get(i), row)));
        }
        return key.toString();
    }

    /**
     * 把 long 映射成 <b>定长且单调</b>的字符串，使字典序等于数值序。
     *
     * <p>做法：翻转符号位后按无符号十进制输出。翻转符号位把整个 long 值域
     * 平移到 {@code [0, 2^64)} 上的无符号整数，天然单调；再零填充到 20 位，
     * 字典序便与数值序一致，负数也能正确排在更小的位置。
     *
     * <p>不使用 {@code String.format}：它受 locale 影响且比手工拼接慢得多。
     */
    private static String pad(long v) {
        String s = Long.toUnsignedString(v ^ Long.MIN_VALUE);
        if (s.length() >= 20) {
            return s;
        }
        return "0".repeat(20 - s.length()) + s;
    }

    private static List<String> unindexed(List<BitmapIndex.Predicate> predicates, BitmapIndex idx) {
        List<String> bad = new ArrayList<>();
        for (var p : predicates) {
            if (!idx.isIndexed(p.column) && !bad.contains(p.column)) {
                bad.add(p.column);
            }
        }
        return bad;
    }

    /** 读取某列某行的原始值（用于聚合与维度取值）。 */
    public long valueAt(String family, String column, int row) {
        return familyColumns.get(family).get(column)[row];
    }

    private final Map<String, Map<String, long[]>> familyColumns = new ConcurrentHashMap<>();

    // ------------------------------------------------------------------ Top-N

    /**
     * 确定性 Top-N：对 {@link AggRow} 按「度量降序 + 全部键升序」排序后取前 N。
     *
     * <p><b>平局必须有确定解</b>：度量和并不足以唯一确定顺序，若只按度量排，
     * 两条同和的行会随 {@code HashMap} 迭代顺序随机换位 —— 这正是那次
     * 「同一 SQL 两次 Top10 顺序不同被投诉造假」的成因。
     * 这里用全序比较器：先度量降序，再按 {@code groupKey} 字典序、桶号升序兜底，
     * 保证任意两条不同行都有唯一且稳定的先后。
     */
    public static List<AggRow> topN(List<AggRow> rows, int n) {
        List<AggRow> copy = new ArrayList<>(rows);
        copy.sort(java.util.Comparator
                .comparingLong(AggRow::sum).reversed()          // 度量降序
                .thenComparing(AggRow::groupKey)                 // 平局按分组键升序
                .thenComparingLong(AggRow::bucket));            // 再按桶号升序
        return copy.size() <= n ? copy : new ArrayList<>(copy.subList(0, n));
    }

    /**
     * 本 store 使用的崩溃注入器（生产环境恒为 {@link CrashInjector#DISABLED}）。
     * 测试用它来在刷盘流水线的任意步骤武装崩溃点。
     */
    public CrashInjector injector() {
        return injector;
    }

    /** WAL 句柄（测试与诊断用）。 */
    WALJournal walForTesting() {
        return wal;
    }

    /** 某页当前保留的版本数（含被活跃快照 pin 住的历史版本）。 */
    public int retainedVersions(int pageId) {
        return snapshots.retainedVersions(pageId);
    }

    /** 已回收的旧版本字节数。 */
    public long reclaimedBytes() {
        return snapshots.reclaimedBytes();
    }

    /** 回收水位：版本号低于它的页内容都已不可见。 */
    public long reclaimHorizon() {
        return snapshots.reclaimHorizon();
    }

    /** 目录（测试用）。 */
    public Path directory() {
        return dir;
    }

    WALJournal wal() {
        return wal;
    }

    /** 已建索引的列（诊断用）。 */
    public List<String> indexedColumns(String family) {
        return family(family).indexedColumns();
    }

    @Override
    public void close() throws IOException {
        if (closed) {
            return;
        }
        closed = true;
        snapshots.close();
        wal.close();
        if (dataChannel.isOpen()) {
            dataChannel.force(false);
            dataChannel.close();
        }
    }
}
