package com.carrier.sigstore;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * 写事务：严格 WAL 顺序 + 原子发布。
 *
 * <p>提交序列：
 * <ol>
 *   <li>每个行组分配 {@code commitSeq}（预留一个单调号）；</li>
 *   <li>把将要写入的页镜像 append 进 WAL；</li>
 *   <li>append {@code COMMIT} 并 <b>fsync</b>；</li>
 *   <li>安装页版本（写时复制：构造新 PageVersion，换引用）；</li>
 *   <li>推进快照水位线。</li>
 * </ol>
 *
 * <p>第 3 步之前崩溃：WAL 无已落盘 COMMIT，恢复时整个事务回滚，无残留。
 * 第 3 步之后崩溃：COMMIT 已 fsync，恢复时重放页镜像，已提交数据完整。
 *
 * <p>真正"安装"发生在 fsync 之后，是这套设计能同时满足"崩溃后已提交不丢 + 未提交干净"的唯一原因：
 * 内存里的可见性永远落后于 WAL 的持久化边界。
 */
public final class WriteTransaction implements AutoCloseable {

    private final ColumnStore store;
    private final long txnId;
    private final long commitSeq;
    private final List<StagedPage> staged = new ArrayList<>();
    private final List<Row> stagedRows = new ArrayList<>();
    /** 提交时在提交锁内分配的唯一行号起点；append 阶段不使用。 */
    private long firstRowId;
    private boolean committed;
    private boolean aborted;

    /**
     * 一次页更新：写时复制出的页缓冲。
     *
     * <p>{@code rowCount}/{@code sealed} 是可变的：同一行组被本事务多次追加时，
     * 它们必须跟着最新写入一起推进，否则提交时会把页头写成过期的行数。
     */
    private static final class StagedPage {
        final int rowGroupId;
        final int columnId;
        final ByteBuffer content;
        int rowCount;
        boolean sealed;

        StagedPage(int rowGroupId, int columnId, ByteBuffer content, int rowCount, boolean sealed) {
            this.rowGroupId = rowGroupId;
            this.columnId = columnId;
            this.content = content;
            this.rowCount = rowCount;
            this.sealed = sealed;
        }
    }

    WriteTransaction(ColumnStore store) {
        this.store = store;
        this.txnId = store.txnCounter().incrementAndGet();
        this.commitSeq = store.commitCounter().incrementAndGet();
        this.firstRowId = 0;
    }

    public long txnId() {
        return txnId;
    }

    public long commitSeq() {
        return commitSeq;
    }

    /**
     * 追加一行。返回该行分配到的全局 rowId。
     *
     * <p>rowId = rowGroupId * rowsPerPage + slot。因为只有提交成功后才会计入 rowCount，
     * 未提交事务追加的行不会占用 rowId，因此不会留下"空洞导致重复计数"的问题。
     */
    /**
     * 追加一行。
     *
     * <p>本方法只把行<b>暂存</b>起来，不碰页、不分配行号。真正的页构建与 rowId 分配
     * 都在 {@link #commit()} 内、持有提交锁时完成。
     *
     * <p>之所以推迟到提交：rowId 必须在提交瞬间唯一，否则两个并发事务会各自
     * 认为自己拥有同一段行号区间，位图与页内容就会互相覆盖。
     * 推迟之后，事务与事务之间在 append 阶段完全无共享状态。
     *
     * @return 暂存行数（不是 rowId——此时还没有 rowId）
     */
    public int append(Row row) {
        if (row.width() != store.schema().columnCount()) {
            throw new IllegalArgumentException("行宽不匹配: 期望 " + store.schema().columnCount()
                    + " 实际 " + row.width());
        }
        stagedRows.add(row);
        return stagedRows.size();
    }

    /**
     * 在提交锁内把暂存行物化到页（写时复制），返回该事务的行号起点。
     */
    private long materializePages() {
        Schema schema = store.schema();
        firstRowId = store.rowCountRef().get();
        staged.clear();
        for (int i = 0; i < stagedRows.size(); i++) {
            Row row = stagedRows.get(i);
            long pending = firstRowId + i;
            int rgId = (int) (pending / schema.rowsPerPage());
            int slot = (int) (pending % schema.rowsPerPage());
            ColumnStore.RowGroup rg = store.rowGroup(rgId, true);
            rg.lock.lock();
            try {
                int newRowCount = slot + 1;
                boolean seal = newRowCount == schema.rowsPerPage();
                for (int c = 0; c < schema.columnCount(); c++) {
                    // 写时复制只在"本事务首次触碰这个行组"时发生一次：
                    // 之后同一行组的后续行直接写进已经暂存的那份缓冲。
                    // 若每行都从已提交 head 重新拷贝整页，装载 n 行就要搬
                    // n x pagesize 字节，200 万行 x 4KB = 8GB，纯属自伤。
                    StagedPage sp = stagedPageFor(rgId, c);
                    ByteBuffer buf;
                    if (sp == null) {
                        ColumnStore.PageVersion head = rg.snapshotRefs()[c];
                        buf = newPageBuffer();
                        if (head != null) copyInto(buf, head.content);
                        sp = new StagedPage(rgId, c, buf, newRowCount, seal);
                        staged.add(sp);
                    } else {
                        // 复用已有暂存页时必须同步推进行数/封口标志，
                        // 否则页头会停留在"本事务第一次追加时"的旧值——
                        // 症状是数据都在，但 rowCount 恒为 1，扫描只认一行。
                        sp.rowCount = newRowCount;
                        sp.sealed = seal;
                        buf = sp.content;
                    }
                    Page.writeValue(buf, schema, c, slot, row.get(c));
                }
            } finally {
                rg.lock.unlock();
            }
        }
        return firstRowId;
    }

    /** 找本事务已暂存的 (rowGroupId, columnId) 页；没有则返回 null。 */
    private StagedPage stagedPageFor(int rowGroupId, int columnId) {
        for (int i = staged.size() - 1; i >= 0; i--) {
            StagedPage sp = staged.get(i);
            if (sp.rowGroupId == rowGroupId && sp.columnId == columnId) return sp;
        }
        return null;
    }

    private ByteBuffer newPageBuffer() {
        return Page.allocate(store.page().pageSize);
    }

    /**
     * 整页拷贝（写时复制的核心）。
     *
     * <p>直接用 absolute put/get 逐字节复制最省心，也最容易证明正确：
     * 两端的 position 都不参与运算，因此无论传入缓冲当前游标在哪都不受影响。
     */
    private static void copyInto(ByteBuffer dst, ByteBuffer src) {
        int n = dst.limit();
        for (int i = 0; i < n; i++) {
            dst.put(i, src.get(i));
        }
    }

    public int stagedRowCount() {
        return stagedRows.size();
    }

    /**
     * 提交。顺序见类注释，fsync 是不可跳过的同步点。
     */
    public void commit() {
        if (committed || aborted) throw new IllegalStateException("事务已结束");
        WALJournal w = store.wal();
        store.commitLock().lock();
        try {
            // 行号与页在提交锁内一次性确定：并发事务因此拿到互不重叠的行号区间。
            long startRow = materializePages();

            w.beginTxn(txnId, commitSeq);
            for (StagedPage sp : staged) {
                ByteBuffer buf = sp.content.duplicate();
                Page.writeHeader(buf, commitSeq, sp.rowCount, sp.sealed, Page.computeCrc(buf));
                w.logPage(txnId, store.pageIdOf(sp.rowGroupId, sp.columnId),
                        store.page().pageSize, Page.toBytes(sp.content), commitSeq);
            }
            w.commit(txnId, commitSeq, startRow + stagedRows.size());
            // 唯一的持久化屏障。返回之后这个事务才算"已提交"。
            store.crashInjector().maybeCrash(CrashInjector.Stage.BEFORE_SYNC);
            w.sync();
            store.crashInjector().maybeCrash(CrashInjector.Stage.AFTER_SYNC);

            installAll(startRow);
            committed = true;
        } catch (IOException e) {
            aborted = true;
            throw new UncheckedIOException("WAL 写入失败，事务已中止", e);
        } finally {
            store.commitLock().unlock();
        }
    }

    /** 安装全部页版本并推进水位线。必须在 COMMIT fsync 之后调用。 */
    private void installAll(long startRow) {
        for (StagedPage sp : staged) {
            ColumnStore.RowGroup rg = store.rowGroup(sp.rowGroupId, true);
            rg.lock.lock();
            try {
                ColumnStore.PageVersion head = rg.snapshotRefs()[sp.columnId];
                ColumnStore.PageVersion next = new ColumnStore.PageVersion(
                        store.pageIdOf(sp.rowGroupId, sp.columnId),
                        sp.rowGroupId, sp.columnId,
                        commitSeq, sp.rowCount, sp.sealed,
                        sp.content, head);
                ColumnStore.PageVersion[] cur = rg.snapshotRefs();
                ColumnStore.PageVersion[] arr = cur.clone();
                arr[sp.columnId] = next;
                rg.latest.set(arr);
                if (sp.columnId == 0) {
                    rg.rowCount = sp.rowCount;
                    rg.sealed = sp.sealed;
                }
            } finally {
                rg.lock.unlock();
            }
        }
        // 位图与提交点保持同一 rowId 口径（rowId = startRow + i），顺序确定。
        for (int i = 0; i < stagedRows.size(); i++) {
            Row r = stagedRows.get(i);
            store.index().addRow((int) (startRow + i), r.toArray());
        }
        long newTotal = startRow + stagedRows.size();
        store.commitPoints().record(commitSeq, newTotal);
        store.rowCountRef().set(newTotal);
        store.advanceSnapshot(commitSeq);
    }

    /** 放弃。已 staged 的页从未进入页缓存，因此无需回滚任何内存状态。 */
    public void abort() {
        if (committed || aborted) return;
        aborted = true;
        try {
            store.wal().abort(txnId, commitSeq);
        } catch (IOException e) {
            throw new UncheckedIOException("写 ABORT 记录失败", e);
        }
    }

    public boolean isCommitted() {
        return committed;
    }

    public boolean isAborted() {
        return aborted;
    }

    @Override
    public void close() {
        if (!committed && !aborted) abort();
    }
}
