package com.carrier.sigstore;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * 批量装载：把"逐事务提交 + 逐事务 fsync"压成一次持久化。
 *
 * <p><b>为什么必须有这条路径</b>：常规写路径每个事务都要 fsync 一次 WAL。
 * 装载两百万行若按行组分批提交，就是三万多次 fsync——实测 150 秒，
 * 瓶颈完全在 fsync 次数，与 CPU 无关。批量装载把 3 万次降到 1 次。
 *
 * <p><b>语义代价（必须知情）</b>：装载期间不写 WAL、不 fsync，因此这段数据
 * <b>不具备崩溃可恢复性</b>。{@link #finish()} 里的 checkpoint 是整批的唯一持久化屏障；
 * 在它之前崩溃，恢复到该库上次 checkpoint 的状态，中间这批整批丢失。
 * 这正是批量装载应有的语义——它服务于"重建一张表"，而不是"逐条接入生产流"。
 * 需要逐条不丢的场景请走 {@link WriteTransaction}。
 */
public final class BulkLoad {

    private final ColumnStore store;
    private final Schema schema;
    private final long commitSeq;

    /** 本次装载正在写的 (rowGroup,column) -> 页缓冲。HashMap 只用于定位，不参与输出序。 */
    private final Map<Long, ByteBuffer> buffers = new HashMap<>();
    /** 被本次装载触达的行组，按行组号升序提交，保证发布顺序确定。 */
    private final List<Integer> rowGroups = new ArrayList<>();

    private long rowCount;
    private boolean finished;

    BulkLoad(ColumnStore store) {
        this.store = store;
        this.schema = store.schema();
        this.rowCount = store.rowCountRef().get();
        this.commitSeq = store.commitCounter().incrementAndGet();
    }

    public long rowCount() {
        return rowCount;
    }

    /**
     * 追加一行。
     *
     * <p>整页只在首次触达该行组时分配并拷贝一次，之后同一行组的后续行就地写入同一块缓冲。
     * 于是装载的内存搬运量是 O(行组数 x 页大小)，而非 O(行数 x 页大小)。
     */
    public void append(Row row) {
        if (finished) throw new IllegalStateException("装载已结束");
        if (row.width() != schema.columnCount()) {
            throw new IllegalArgumentException("行宽不匹配: 期望 " + schema.columnCount() + " 实际 " + row.width());
        }
        int rgId = (int) (rowCount / schema.rowsPerPage());
        int slot = (int) (rowCount % schema.rowsPerPage());
        ColumnStore.RowGroup rg = store.rowGroup(rgId, true);

        rg.lock.lock();
        try {
            if (slot == 0) rowGroups.add(rgId);
            int newRowCount = slot + 1;
            boolean seal = newRowCount == schema.rowsPerPage();
            for (int c = 0; c < schema.columnCount(); c++) {
                ByteBuffer buf = buffers.get(key(rgId, c));
                if (buf == null) {
                    buf = Page.allocate(store.page().pageSize);
                    ColumnStore.PageVersion head = rg.snapshotRefs()[c];
                    if (head != null) copyInto(buf, head.content);
                    buffers.put(key(rgId, c), buf);
                }
                Page.writeValue(buf, schema, c, slot, row.get(c));
            }
            rg.pendingRowCount = newRowCount;
            rg.sealed = seal;
            rowCount++;
        } finally {
            rg.lock.unlock();
        }
    }

    /**
     * 结束装载：按 rowGroup 升序把页发布进版本链（确定性顺序），
     * 建位图、推进提交点与水位线，最后做一次 checkpoint 完成持久化。
     */
    public void finish() throws IOException {
        if (finished) throw new IllegalStateException("装载已结束");
        finished = true;

        // 按行组号升序发布：保证同样的装载序列产生同样的页版本链
        List<Integer> ordered = new ArrayList<>(new java.util.TreeSet<>(rowGroups));
        for (int rgId : ordered) {
            ColumnStore.RowGroup rg = store.rowGroup(rgId, false);
            if (rg == null) continue;
            rg.lock.lock();
            try {
                for (int c = 0; c < schema.columnCount(); c++) {
                    ByteBuffer buf = buffers.get(key(rgId, c));
                    if (buf == null) continue;
                    ColumnStore.PageVersion next = new ColumnStore.PageVersion(
                            store.pageIdOf(rgId, c), rgId, c, commitSeq,
                            rg.pendingRowCount, rg.sealed, buf, rg.snapshotRefs()[c]);
                    store.installVersion(rgId, c, next);
                }
            } finally {
                rg.lock.unlock();
            }
        }

        // 先推水位线再建位图：位图重建要按快照号读页，
        // 若此刻 lastCommitSeq 还停在旧值，刚发布的页会被判为"快照不可见"而全部读空。
        store.advanceSnapshot(commitSeq);

        // 位图是派生结构，按 rowId 升序补建；顺序确定
        for (long rowId = 0; rowId < rowCount; rowId++) {
            long[] vals = store.materializeForIndex((int) rowId);
            if (vals == null) {
                throw new IllegalStateException("装载发布后第 " + rowId + " 行不可读，装载状态不一致");
            }
            store.index().addRow((int) rowId, vals);
        }

        store.rowCountRef().set(rowCount);
        store.commitPoints().record(commitSeq, rowCount);
        store.checkpoint();          // 整批唯一的持久化屏障
        buffers.clear();
        rowGroups.clear();
    }

    private static long key(int rg, int c) {
        return ((long) rg << 32) | (c & 0xFFFFFFFFL);
    }

    private static void copyInto(ByteBuffer dst, ByteBuffer src) {
        int n = dst.limit();
        for (int i = 0; i < n; i++) dst.put(i, src.get(i));
    }

}
