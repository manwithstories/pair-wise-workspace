package com.carrier.sigstore;

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
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;
import java.util.concurrent.locks.ReentrantLock;

/**
 * 列存内核：列族管理 + 页分配 + 页缓存 + 快照读路径。
 *
 * <p>物理布局按 {@code (rowGroup, columnId)} 定址：
 * <pre>
 *   pageId = rowGroup * columnCount + columnId
 * </pre>
 * 一个 rowGroup 恰好是每个列族各一个页，页内列主序排列，因此列扫描是连续访存，
 * 等值过滤下推后只需回读命中列的块。
 *
 * <p><b>并发模型</b>（整库读路径无锁）：
 * <ul>
 *   <li>每行组持有一个 {@link AtomicReference}，指向该行组各列"最新版本"的数组。
 *       写路径每次提交都构造一个新数组并整体替换——这就是写时复制在引用层面的体现。
 *       数组发布是原子的，读者拿到的一定是某次提交的完整视图，不会看到半截状态。</li>
 *   <li>读者沿 {@link PageVersion#older} 链回溯到第一个 {@code version <= 快照号} 的版本。
 *       版本对象构造后即不可变，回溯过程无需加锁。</li>
 *   <li>写只在"改页内容 + 换引用"这一小段持有 per-rowGroup 锁，且 WAL fsync 在锁外进行，
 *       因此写不阻塞读、读不阻塞写。</li>
 * </ul>
 *
 * <p><b>锁序</b>：若需同时持有多个行组锁，必须按 rowGroupId 升序。单行组写入天然满足，无死锁可能。
 */
public final class ColumnStore implements AutoCloseable {

    private final Schema schema;
    private final Path dataPath;
    private final FileChannel dataChannel;
    private final Page page;
    private final SnapshotManager snapshots;
    private final WALJournal wal;
    private final CrashInjector crashInjector;
    private final ReentrantLock checkpointLock = new ReentrantLock();

    private final ConcurrentHashMap<Integer, RowGroup> rowGroups = new ConcurrentHashMap<>();
    private final AtomicLong committedRowCount = new AtomicLong(0);
    private final AtomicLong commitCounter = new AtomicLong(0);
    private final AtomicLong txnCounter = new AtomicLong(0);
    private final AtomicLong pageReads = new AtomicLong(0);
    private final AtomicLong pageHits = new AtomicLong(0);
    private final AtomicLong diskPageReads = new AtomicLong(0);
    private final BitmapIndex index;
    private final CommitPoints commitPoints;

    private volatile boolean closed;

    /**
     * 提交串行化锁。
     *
     * <p>为什么写路径需要它：rowId 与提交序号都是在<b>提交时</b>分配的，
     * 而行组页的写时复制发生在 append 期。若两个事务并发提交，
     * 它们各自在 beginWrite 时记下的 firstRowId 可能重叠，导致
     * "两个事务都认为自己拥有第 N 行"——位图因此错位、页内容也互相覆盖。
     *
     * <p>这个锁只覆盖<b>提交收尾</b>（分配序号、装页、推水位线），
     * 批量追加的重活在锁外。因此它既保证了 rowId 唯一，
     * 又不会把 fsync 之类的慢操作纳入临界区。
     * 读路径完全不碰这把锁。
     */
    private final java.util.concurrent.locks.ReentrantLock commitLock =
            new java.util.concurrent.locks.ReentrantLock();

    public ColumnStore(Path dir, Schema schema, SnapshotManager snapshots, WALJournal wal,
                       CrashInjector injector) throws IOException {
        this.schema = schema;
        this.snapshots = snapshots;
        this.wal = wal;
        this.crashInjector = injector == null ? new CrashInjector() : injector;
        this.dataPath = dir.resolve("data.csf");
        Files.createDirectories(dir);
        this.page = new Page(Page.pageSizeFor(schema.payloadBytes(), Page.MIN_PAGE_SIZE));
        this.index = new BitmapIndex(schema);
        this.commitPoints = new CommitPoints();
        this.dataChannel = FileChannel.open(dataPath,
                StandardOpenOption.CREATE, StandardOpenOption.READ, StandardOpenOption.WRITE);
    }

    /**
     * 打开（或新建）一个库并执行崩溃恢复。
     *
     * <p>恢复分两步：{@link RecoveryEngine} 把磁盘状态（数据文件 + WAL）收敛到最后一个一致点，
     * 然后这里据此<b>重建内存态</b>——页版本链、提交点表、位图索引、行数。
     * 这一步不能省：读路径只认内存里的页版本，不会在查询时顺带读盘。
     */
    public static ColumnStore open(Path dir, Schema schema) throws IOException {
        return open(dir, schema, new CrashInjector());
    }

    public static ColumnStore open(Path dir, Schema schema, CrashInjector injector) throws IOException {
        RecoveryEngine.Report report = RecoveryEngine.recover(dir, schema);
        WALJournal w = new WALJournal(dir.resolve("wal.log"));
        ColumnStore store = new ColumnStore(dir, schema, new SnapshotManager(), w, injector);
        store.rebuildFromDisk(report);
        return store;
    }

    public static ColumnStore create(Path dir, Schema schema) throws IOException {
        return open(dir, schema, new CrashInjector());
    }

    /** 提交串行化锁（包级可见，供 WriteTransaction 使用）。 */
    java.util.concurrent.locks.ReentrantLock commitLock() {
        return commitLock;
    }

    /** 开始一次批量装载。结束请调用 {@link BulkLoad#finish()}。 */
    public BulkLoad beginBulkLoad() {
        if (closed) throw new IllegalStateException("存储已关闭");
        return new BulkLoad(this);
    }

    /** 只给装载/测试用：按当前已发布页读出一行（不经过快照链）。 */
    long[] materializeForIndex(int rowId) {
        long seq = snapshots.currentCommitSeq();
        int rpp = schema.rowsPerPage();
        int rg = rowId / rpp, slot = rowId % rpp;
        long[] vals = new long[schema.columnCount()];
        for (int c = 0; c < schema.columnCount(); c++) {
            PageVersion v = readPage(rg, c, seq);
            if (v == null || slot >= v.rowCount) return null;
            vals[c] = Page.readValue(v.view(), schema, c, slot);
        }
        return vals;
    }

    /**
     * 从已恢复的数据文件重建全部内存结构。
     *
     * <p>每页按 rowGroup 升序、列升序装载（确定性顺序），版本号取页头里持久化的 version。
     * 行数取所有页里 rowCount 的最大值——每个行组的最后一列总是写到最终行数，
     * 因此这个口径不会漏也不会多。之后为每一行补建位图（位图本就是查询期可重建的派生结构）。
     */
    private void rebuildFromDisk(RecoveryEngine.Report report) throws IOException {
        Page p = new Page(page.pageSize);
        int cc = schema.columnCount();
        long fileLen = dataChannel.size();

        // 每个行组的行数取该行组内各列 page rowCount 的最大值。
        // 不能把各页 rowCount 直接取全局 max：行组 0 有 8 行、行组 2 只有 4 行，
        // 取 max 会把中间的空槽也算进可见行数。
        java.util.TreeMap<Integer, Integer> rowsPerGroup = new java.util.TreeMap<>();

        for (long pageId = 0; pageId * (long) p.pageSize < fileLen; pageId++) {
            ByteBuffer buf = Page.allocate(p.pageSize);
            long off = pageId * (long) p.pageSize;
            int n = 0;
            while (n < p.pageSize) {
                int r = dataChannel.read(buf, off + n);
                if (r < 0) break;
                n += r;
            }
            if (n < p.pageSize) break;          // 尾部半页：丢弃
            buf.flip();
            if (!Page.isIntact(buf)) continue;  // CRC 不符：视为未完成页

            int rowCount = Page.readRowCount(buf);
            long version = Page.readVersion(buf);
            boolean sealed = Page.readSealed(buf);

            // 注意 buf 此时 position=0/limit=pageSize。先整页拷进自有缓冲，
            // 再 clear() 只是把游标复位，不会丢掉刚拷进去的字节。
            ByteBuffer owned = Page.allocate(p.pageSize);
            owned.put(buf);
            owned.clear();

            int rgId = (int) (pageId / cc);
            int colId = (int) (pageId % cc);
            installVersion(rgId, colId, new PageVersion((int) pageId, rgId, colId,
                    version, rowCount, sealed, owned, null));
            rowsPerGroup.merge(rgId, rowCount, Math::max);
        }

        // 总行数 = 行组数 * 每行组行数（行组是满的，只有最后一组可能不满）
        int maxGroup = rowsPerGroup.isEmpty() ? 0 : rowsPerGroup.lastKey();
        long total = 0;
        for (Map.Entry<Integer, Integer> e : rowsPerGroup.entrySet()) {
            total += e.getValue();
        }

        committedRowCount.set(total);
        commitPoints.restore(report.lastCommitSeq(), total);
        snapshots.advanceTo(report.lastCommitSeq());
        reindexRows();
    }

    /** 重建位图索引（派生结构，按 rowId 升序重建，顺序确定）。 */
    private void reindexRows() {
        int rpp = schema.rowsPerPage();
        long rows = committedRowCount.get();
        for (int rowId = 0; rowId < rows; rowId++) {
            int rg = rowId / rpp, slot = rowId % rpp;
            long[] vals = new long[schema.columnCount()];
            boolean ok = true;
            for (int c = 0; c < schema.columnCount(); c++) {
                PageVersion v = readPage(rg, c, snapshots.currentCommitSeq());
                if (v == null || slot >= v.rowCount) { ok = false; break; }
                vals[c] = Page.readValue(v.view(), schema, c, slot);
            }
            if (ok) index.addRow(rowId, vals);
        }
    }

    public Schema schema() {
        return schema;
    }

    public SnapshotManager snapshots() {
        return snapshots;
    }

    public WALJournal wal() {
        return wal;
    }

    public Page page() {
        return page;
    }

    public CrashInjector crashInjector() {
        return crashInjector;
    }

    public long committedRowCount() {
        return committedRowCount.get();
    }

    /** 全局位图索引，由写路径在提交时同步维护。 */
    public BitmapIndex index() {
        return index;
    }

    /** 提交点索引：快照号 -> 可见行数。 */
    public CommitPoints commitPoints() {
        return commitPoints;
    }

    /**
     * 快照 S 下的可见行数。这是所有读路径的唯一行数口径。
     */
    public long visibleRows(long snapshotSeq) {
        return commitPoints.visibleRows(snapshotSeq);
    }

    public long pageReads() {
        return pageReads.get();
    }

    public long diskPageReads() {
        return diskPageReads.get();
    }

    /** 页缓存命中率。测试要求 &ge; 95%。 */
    public double pageHitRate() {
        long total = pageReads.get();
        return total == 0 ? 1.0 : (double) pageHits.get() / total;
    }

    public int rowGroupCount() {
        return rowGroups.size();
    }

    // ---- 行组与页版本 ----------------------------------------------------

    /** 单行组。锁只覆盖"构造新版本 + 换引用"这一小段。 */
    static final class RowGroup {
        final int id;
        final ReentrantLock lock = new ReentrantLock();
        final AtomicReference<PageVersion[]> latest;
        volatile int rowCount;
        volatile boolean sealed;
        /** 批量装载期间该行组已写入的最终行数，发布时取用。 */
        volatile int pendingRowCount;

        RowGroup(int id, int columnCount) {
            this.id = id;
            this.latest = new AtomicReference<>(new PageVersion[columnCount]);
        }

        PageVersion[] snapshotRefs() {
            return latest.get();
        }
    }

    /**
     * 页的一个不可变版本。写时复制的产物。
     *
     * <p>{@code content} 由构造方独占并在发布后不再修改内容；读者通过 {@link #view()}
     * 拿只读切片，因此并发读不会互相干扰。缓冲本身保持可写，是为了让
     * {@code checkpoint()} 能就地回写页头（version/rowCount/CRC）——这不改变负载区字节，
     * 也不会被读者看到不一致：读者只读负载区，而负载区在该版本的整个生命周期内不变。
     */
    static final class PageVersion {
        final int pageId;
        final int rowGroupId;
        final int columnId;
        final long version;              // 提交序号，快照可见性判定依据
        final int rowCount;
        final boolean sealed;
        final ByteBuffer content;
        final PageVersion older;         // 上一版本；被回收后为 null

        PageVersion(int pageId, int rowGroupId, int columnId, long version,
                    int rowCount, boolean sealed, ByteBuffer content, PageVersion older) {
            this.pageId = pageId;
            this.rowGroupId = rowGroupId;
            this.columnId = columnId;
            this.version = version;
            this.rowCount = rowCount;
            this.sealed = sealed;
            this.content = content;
            this.older = older;
        }

        ByteBuffer view() {
            return content.asReadOnlyBuffer().order(ByteOrder.BIG_ENDIAN);
        }

        /** 页镜像（供 WAL 记账与恢复重放）。 */
        byte[] image() {
            ByteBuffer dup = content.duplicate().order(ByteOrder.BIG_ENDIAN);
            Page.writeHeader(dup, version, rowCount, sealed, Page.computeCrc(dup));
            return Page.toBytes(content);
        }
    }

    int pageIdOf(int rowGroupId, int columnId) {
        return rowGroupId * schema.columnCount() + columnId;
    }

    RowGroup rowGroup(int id, boolean create) {
        RowGroup rg = rowGroups.get(id);
        if (rg != null || !create) return rg;
        return rowGroups.computeIfAbsent(id, k -> new RowGroup(k, schema.columnCount()));
    }

    ConcurrentHashMap<Integer, RowGroup> rowGroups() {
        return rowGroups;
    }

    /**
     * 读某行组某列在快照 S 下可见的页版本。<b>读路径的唯一入口。</b>
     *
     * <p>先看链头：绝大多数情况 head 就是可见版本（或只需回退一步），
     * 这条快路径没有额外分配，也没有锁。
     */
    PageVersion readPage(int rowGroupId, int columnId, long snapshotSeq) {
        RowGroup rg = rowGroups.get(rowGroupId);
        if (rg == null) return null;
        PageVersion[] refs = rg.snapshotRefs();
        if (columnId >= refs.length) return null;
        PageVersion v = refs[columnId];
        if (v == null) return null;

        pageReads.incrementAndGet();
        if (v.version <= snapshotSeq) {           // 快路径：链头即可见
            pageHits.incrementAndGet();
            return v;
        }
        // 回溯：找出第一个 version <= snapshotSeq 的版本
        PageVersion cur = v.older;
        while (cur != null && cur.version > snapshotSeq) {
            cur = cur.older;
        }
        if (cur != null) pageHits.incrementAndGet();
        return cur;
    }

    /** 仅用于恢复：安装一个从磁盘/WAL 重建出的版本链。 */
    void installVersion(int rowGroupId, int columnId, PageVersion version) {
        RowGroup rg = rowGroup(rowGroupId, true);
        rg.lock.lock();
        try {
            PageVersion[] cur = rg.snapshotRefs();
            PageVersion[] next = cur.clone();
            next[columnId] = version;
            rg.latest.set(next);
            // 页的 rowCount 即该行组的权威行数：任何一条列页装上，行组就该认这个数。
            // 之前只依赖装载期单独记的 pendingRowCount，导致发布后的行组行数仍为 0，
            // 重建索引时整行读不出来。
            if (columnId == 0) {
                rg.rowCount = version.rowCount;
                rg.sealed = version.sealed;
            }
        } finally {
            rg.lock.unlock();
        }
    }

    // ---- 写事务 ---------------------------------------------------------

    public WriteTransaction beginWrite() {
        if (closed) throw new IllegalStateException("存储已关闭");
        return new WriteTransaction(this);
    }

    // ---- 读快照 ---------------------------------------------------------

    public ReadSnapshot beginRead() {
        return new ReadSnapshot(this);
    }

    /**
     * 一次读事务的不可变视图。
     *
     * <p>快照号在构造时一次性确定，此后所有读取都以它为准——这就是"同事务多次读到一致视图"。
     */
    public final class ReadSnapshot implements AutoCloseable {
        private final SnapshotManager.Snapshot snapshot;

        ReadSnapshot(ColumnStore store) {
            this.snapshot = store.snapshots.acquire();
        }

        public long snapshotId() {
            return snapshot.snapshotId();
        }

        public long commitSeq() {
            return snapshot.commitSeq();
        }

        public SnapshotManager.Snapshot raw() {
            return snapshot;
        }

        /** 读某列在本次快照下可见的页。 */
        PageVersion page(int rowGroupId, int columnId) {
            return readPage(rowGroupId, columnId, snapshot.commitSeq());
        }

        @Override
        public void close() {
            snapshot.close();
        }
    }

    // ---- 落盘 -----------------------------------------------------------

    /**
     * 把页版本刷入数据文件并 fsync。
     *
     * <p>调用方保证 COMMIT 已落 WAL，因此即便这里被崩溃打断，RecoveryEngine 也能从 WAL 补齐。
     */
    void flushPage(PageVersion v) throws IOException {
        crashInjector.maybeCrash(CrashInjector.Stage.BEFORE_SYNC);
        ByteBuffer buf = v.content.duplicate().order(ByteOrder.BIG_ENDIAN);
        Page.writeHeader(buf, v.version, v.rowCount, v.sealed, Page.computeCrc(buf));
        crashInjector.maybeCrash(CrashInjector.Stage.MID_SYNC);
        long off = (long) v.pageId * page.pageSize;
        int written = 0;
        while (written < page.pageSize) {
            int n = dataChannel.write(buf, off + written);
            if (n <= 0) break;
            written += n;
        }
        dataChannel.force(false);
        crashInjector.maybeCrash(CrashInjector.Stage.AFTER_SYNC);
    }

    /**
     * 检查点：把当前所有页写回数据文件并 fsync，随后 fsync WAL。
     *
     * <p><b>必须真的落页</b>，只做 force() 是不够的：force 只保证"已经写过的字节"
     * 落盘，而页可能还只存在于页缓存里。恢复时数据文件若为空，就只能靠 WAL 重放
     * 全量页镜像——功能上仍正确，但把 100MB WAL 变成了每次启动的固定开销。
     * 正常 checkpoint 后 WAL 可以截断，恢复就不必再扫那么大一份日志。
     */
    public void checkpoint() throws IOException {
        checkpointLock.lock();
        try {
            List<PageVersion> all = new ArrayList<>();
            for (RowGroup rg : rowGroups.values()) {
                for (PageVersion v : rg.snapshotRefs()) {
                    if (v != null) all.add(v);
                }
            }
            // 确定性顺序：按 pageId 升序，保证检查点字节序可复现
            all.sort((x, y) -> Integer.compare(x.pageId, y.pageId));
            for (PageVersion v : all) {
                ByteBuffer buf = v.content.duplicate();
                Page.writeHeader(buf, v.version, v.rowCount, v.sealed, Page.computeCrc(buf));
                long off = (long) v.pageId * page.pageSize;
                int w = 0;
                while (w < page.pageSize) {
                    int n = dataChannel.write(buf, off + w);
                    if (n <= 0) break;
                    w += n;
                }
            }
            dataChannel.force(true);
            wal.sync();
        } finally {
            checkpointLock.unlock();
        }
    }

    AtomicLong commitCounter() {
        return commitCounter;
    }

    AtomicLong txnCounter() {
        return txnCounter;
    }

    AtomicLong rowCountRef() {
        return committedRowCount;
    }

    Path dataPath() {
        return dataPath;
    }

    FileChannel dataChannel() {
        return dataChannel;
    }

    void advanceSnapshot(long commitSeq) {
        snapshots.advanceTo(commitSeq);
    }

    void noteDiskRead() {
        diskPageReads.incrementAndGet();
    }

    public Map<String, Object> stats() {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("rows", committedRowCount.get());
        m.put("rowGroups", rowGroups.size());
        m.put("pageReads", pageReads.get());
        m.put("pageHits", pageHits.get());
        m.put("diskPageReads", diskPageReads.get());
        m.put("hitRate", pageHitRate());
        m.put("lastCommitSeq", snapshots.currentCommitSeq());
        m.put("pageSize", page.pageSize);
        return m;
    }

    @Override
    public void close() throws IOException {
        closed = true;
        try {
            checkpoint();
        } finally {
            wal.close();
            dataChannel.close();
        }
    }
}
