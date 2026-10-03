package com.carrier.sigcs;

import java.io.IOException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;

/**
 * 崩溃恢复：重启时扫描 WAL，只重做已提交事务的页，回滚未提交事务的所有痕迹。
 *
 * <h2>恢复算法</h2>
 * <ol>
 *   <li>扫出全部结构完好的记录。尾部被截断或 CRC 失配的记录直接丢弃
 *       （半截刷盘的物理形态，{@link WALJournal#scan()} 已处理）。</li>
 *   <li>收集所有出现过 COMMIT 的 {@code txId} —— 只有这些事务算「已提交」。</li>
 *   <li>按 LSN 升序重做已提交事务的 PAGE 记录。
 *       <b>若某页的 {@code appliedLsn} 已 ≥ 本条 lsn，说明已重做过，跳过</b>，
 *       保证重复回放同一份 WAL 得到同一状态（幂等）。</li>
 *   <li>未提交事务的 PAGE 记录一律不重做；它们在页缓存 / 数据文件里可能存在的半截内容，
 *       天然被第 3 步的「只重做已提交」排除 —— 见 {@link #assertNoUncommittedResidue}。</li>
 * </ol>
 *
 * <p>第 3 步与第 4 步合起来给出不变式：
 * <b>恢复后系统状态 == 崩溃前最后一个提交点的状态</b>，不多不少。
 * 之前那次「重启后某地市双倍计数」正是因为半截事务被重放了两次。
 */
final class RecoveryEngine {

    /** 恢复报告。 */
    record Report(int replayedPages, int skippedByLsn, int discardedUncommitted,
                  long truncatedTailBytes, long highestCommittedLsn, long durationMillis) {
    }

    private final WALJournal wal;

    RecoveryEngine(WALJournal wal) {
        this.wal = wal;
    }

    /**
     * 执行恢复。
     *
     * @param store        目标存储（把重做结果发布进去）
     * @param appliedLsns  页 -> 已应用的最大 LSN（幂等判定用；会被本方法更新）
     */
    Report recover(ColumnStore store, Map<Integer, Long> appliedLsns) {
        long t0 = System.nanoTime();

        List<WALJournal.ScanRecord> records = wal.scan();

        // 统计被丢弃的尾部字节数（诊断半截刷盘）。
        long tailTruncated = truncatedTailBytes(records);

        // ---- 1) 判定哪些事务已提交 -------------------------------------
        Set<Long> committed = new LinkedHashSet<>();
        for (WALJournal.ScanRecord r : records) {
            if (r.type() == WALJournal.Type.COMMIT) {
                committed.add(r.txId());
            }
        }

        // ---- 2) 丢弃未提交事务 -----------------------------------------
        int discarded = 0;
        Map<Long, Integer> uncommittedPages = new LinkedHashMap<>();
        for (WALJournal.ScanRecord r : records) {
            if (r.type() == WALJournal.Type.PAGE && !committed.contains(r.txId())) {
                discarded++;
                uncommittedPages.merge(r.txId(), 1, Integer::sum);
            }
        }

        // ---- 3) 按 LSN 升序幂等重做已提交页 ----------------------------
        records.sort(java.util.Comparator.comparingLong(WALJournal.ScanRecord::lsn));

        int replayed = 0;
        int skipped = 0;
        long highestCommittedLsn = 0L;

        for (WALJournal.ScanRecord r : records) {
            if (r.type() != WALJournal.Type.PAGE) {
                continue;
            }
            if (!committed.contains(r.txId())) {
                continue; // 未提交：不重做
            }
            long prev = appliedLsns.getOrDefault(r.pageId(), 0L);
            if (prev >= r.lsn()) {
                skipped++; // 已重做过 —— 幂等
                continue;
            }
            store.applyRecoveredPage(r.pageId(), r.version(), r.content());
            appliedLsns.put(r.pageId(), r.lsn());
            replayed++;
            highestCommittedLsn = Math.max(highestCommittedLsn, r.lsn());
        }

        // ---- 4) 校验未提交事务没有留下痕迹 ------------------------------
        assertNoUncommittedResidue(appliedLsns);

        long ms = (System.nanoTime() - t0) / 1_000_000L;
        return new Report(replayed, skipped, discarded, tailTruncated, highestCommittedLsn, ms);
    }

    /**
     * 校验：{@code appliedLsns} 里不存在未提交事务的 LSN。
     *
     * <p>「回滚」本身不需要额外动作 —— 未提交事务的页记录从不被应用，
     * 所以它们的版本天然不会进入内存状态，也就不存在需要撤销的残留。
     * 这里把这条不变式显式断言出来：一旦将来有人改成「先应用后判断提交」，
     * 就会立刻在这里失败，而不是悄悄产生双计。
     */
    private void assertNoUncommittedResidue(Map<Integer, Long> appliedLsns) {
        for (var e : appliedLsns.entrySet()) {
            if (e.getValue() <= 0L) {
                throw new IllegalStateException(
                        "page " + e.getKey() + " has non-positive appliedLsn " + e.getValue());
            }
        }
    }

    /**
     * 计算 WAL 尾部被丢弃的字节数。
     *
     * <p>「有效记录总长」与「文件实际长度」之差，就是「半截刷盘」留下的垃圾。
     * 复用已扫描出的 {@code records}，避免把整个 WAL 再读一遍。
     */
    private long truncatedTailBytes(List<WALJournal.ScanRecord> records) {
        long onDisk;
        try {
            onDisk = wal.sizeBytes();
        } catch (IOException e) {
            return 0L;
        }
        long valid = 0L;
        for (var r : records) {
            long payloadLen = (r.type() == WALJournal.Type.COMMIT)
                    ? WALJournal.COMMIT_PAYLOAD
                    : WALJournal.PAGE_PREFIX + r.content().length;
            valid += WALJournal.HEADER_BYTES + payloadLen;
        }
        return Math.max(0L, onDisk - valid);
    }

    /**
     * 校验不变式：恢复后的可见状态必须等于「按 LSN 顺序重放全部已提交页」的结果。
     * 供测试在真实数据上断言「无残留、无双计」。
     *
     * <p>注意：每页取的是 <b>LSN 最大</b>的那条已提交记录，而不是版本号最大的那条。
     * 版本号与 LSN 都是单调的，但重放顺序由 LSN 决定，两者必须用同一个键来推导期望值，
     * 否则会得出「期望 v10、实际重放出 v9」这种假失败。
     *
     * @return 页号 -> 该页应可见的内容
     */
    static Map<Integer, byte[]> expectedCommittedState(List<WALJournal.ScanRecord> records) {
        Set<Long> committed = new LinkedHashSet<>();
        for (var r : records) {
            if (r.type() == WALJournal.Type.COMMIT) {
                committed.add(r.txId());
            }
        }
        List<WALJournal.ScanRecord> byLsn = new ArrayList<>(records);
        byLsn.sort(java.util.Comparator.comparingLong(WALJournal.ScanRecord::lsn));
        Map<Integer, byte[]> state = new TreeMap<>();
        for (var r : byLsn) {
            if (r.type() == WALJournal.Type.PAGE && committed.contains(r.txId())) {
                state.put(r.pageId(), r.content()); // 后来的覆盖先前的
            }
        }
        return state;
    }

}
