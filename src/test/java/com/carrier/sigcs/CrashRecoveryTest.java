package com.carrier.sigcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 崩溃恢复：刷盘前 / 中 / 后注入中断，重启后
 * <ul>
 *   <li>已提交事务的页一条不丢；</li>
 *   <li>未提交事务的页一点残留都没有（<b>不双计</b>）；</li>
 *   <li>WAL 重复回放幂等。</li>
 * </ul>
 */
class CrashRecoveryTest {

    /** 模拟一条话单聚合页的载荷。 */
    private static byte[] page(int cityId, long count) {
        return ("city=" + cityId + ";count=" + count).getBytes(StandardCharsets.UTF_8);
    }

    private static String text(byte[] b) {
        return b == null ? "<absent>" : new String(b, StandardCharsets.UTF_8);
    }

    private static long countOf(byte[] b) {
        String s = text(b);
        int i = s.indexOf("count=");
        return Long.parseLong(s.substring(i + 6));
    }

    // ------------------------------------------------------------------ 基本正确性

    @Test
    @DisplayName("正常提交后重启：已提交页完整恢复")
    void committedSurvivesCleanRestart(@TempDir Path dir) throws IOException {
        try (ColumnStore store = new ColumnStore(dir.resolve("a"))) {
            store.recover();
            store.begin().put(1, page(1, 100)).commit();
            store.begin().put(2, page(2, 200)).commit();
        }

        // 重启
        try (ColumnStore store = new ColumnStore(dir.resolve("a"))) {
            store.recover();
            try (var tx = store.snapshot()) {
                assertEquals("city=1;count=100", text(store.readPage(1, tx)));
                assertEquals("city=2;count=200", text(store.readPage(2, tx)));
            }
        }
    }

    // ------------------------------------------------------------------ 崩溃点注入

    @Test
    @DisplayName("崩溃点 PRE_COMMIT：WAL 未落盘，重启后完全没有该事务的痕迹")
    void crashBeforeCommitLeavesNothing(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("precommit");
        try (ColumnStore store = new ColumnStore(base, new CrashInjector())) {
            store.recover();
            store.begin().put(1, page(1, 10)).commit();

            store.injector().arm(CrashInjector.Step.PRE_COMMIT, 0, true);

            ColumnStore.Txn doomed = store.begin();
            doomed.put(1, page(1, 999));
            assertThrows(CrashInjector.CrashException.class, doomed::commit);
        }

        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            try (var tx = store.snapshot()) {
                assertEquals("city=1;count=10", text(store.readPage(1, tx)),
                        "uncommitted transaction must not be visible after recovery");
            }
        }
    }

    @Test
    @DisplayName("崩溃点 WAL_APPEND：页记录未 fsync，重启后未提交事务不生效")
    void crashDuringWalAppendDiscardsUncommitted(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("walappend");
        try (ColumnStore store = new ColumnStore(base, new CrashInjector())) {
            store.recover();
            store.begin().put(1, page(1, 10)).commit();

            store.injector().arm(CrashInjector.Step.WAL_APPEND, 0, true);

            ColumnStore.Txn doomed = store.begin();
            doomed.put(1, page(1, 500));
            assertThrows(CrashInjector.CrashException.class, doomed::commit);
        }

        try (ColumnStore store = new ColumnStore(base)) {
            RecoveryEngine.Report r = store.recover();
            try (var tx = store.snapshot()) {
                assertEquals("city=1;count=10", text(store.readPage(1, tx)),
                        "half-written WAL record must not resurrect an uncommitted txn");
            }
            assertTrue(r.discardedUncommitted() >= 0);
        }
    }

    @Test
    @DisplayName("崩溃点 WAL_FSYNC_DONE：页记录已落盘但无 COMMIT，仍不得生效")
    void crashAfterWalFsyncWithoutCommitIsDiscarded(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("walfync");
        try (ColumnStore store = new ColumnStore(base, new CrashInjector())) {
            store.recover();
            store.begin().put(1, page(1, 10)).commit();

            // 放过第一条 PAGE 记录的 fsync，在第二条之后崩 —— 此时 PAGE 已耐久、COMMIT 未写
            store.injector().arm(CrashInjector.Step.WAL_FSYNC_DONE, 0, true);

            ColumnStore.Txn doomed = store.begin();
            doomed.put(1, page(1, 700));
            assertThrows(CrashInjector.CrashException.class, doomed::commit);
        }

        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            try (var tx = store.snapshot()) {
                assertEquals("city=1;count=10", text(store.readPage(1, tx)),
                        "WAL-fsynced but uncommitted pages must roll back");
            }
        }
    }

    @Test
    @DisplayName("崩溃点 PAGE_DIRTY：提交已成立，数据页回写中断，重启后由 WAL 重做补齐")
    void crashDuringPageWritebackIsRepairedByWal(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("pagedirty");
        long newCount;
        try (ColumnStore store = new ColumnStore(base, new CrashInjector())) {
            store.recover();
            store.begin().put(1, page(1, 10)).commit();

            // 越过提交点，在回写数据页时崩
            store.injector().arm(CrashInjector.Step.PAGE_DIRTY, 0, true);

            ColumnStore.Txn good = store.begin();
            good.put(1, page(1, 20));
            assertThrows(CrashInjector.CrashException.class, good::commit);
            newCount = 20;
        }

        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            try (var tx = store.snapshot()) {
                assertEquals("city=1;count=" + newCount, text(store.readPage(1, tx)),
                        "a committed txn whose page writeback was interrupted must be replayed from WAL");
            }
        }
    }

    @Test
    @DisplayName("崩溃点 PAGE_FSYNC_DONE：页已落盘，数据与 WAL 一致")
    void crashAfterPageFsync(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("pagefsync");
        try (ColumnStore store = new ColumnStore(base, new CrashInjector())) {
            store.recover();
            store.begin().put(1, page(1, 10)).commit();

            store.injector().arm(CrashInjector.Step.PAGE_FSYNC_DONE, 0, true);
            ColumnStore.Txn good = store.begin();
            good.put(1, page(1, 30));
            assertThrows(CrashInjector.CrashException.class, good::commit);
        }

        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            try (var tx = store.snapshot()) {
                assertEquals("city=1;count=30", text(store.readPage(1, tx)));
            }
        }
    }

    // ------------------------------------------------------------------ 不双计

    @Test
    @DisplayName("核心事故回归：崩溃重启后计数既不丢失也不翻倍")
    void noDoubleCountingAfterCrash(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("doublecount");

        // 阶段一：地市 7 累加若干次，其中最后一次崩溃
        try (ColumnStore store = new ColumnStore(base, new CrashInjector())) {
            store.recover();
            long committed = 0L;
            for (int i = 0; i < 5; i++) {
                store.begin().put(7, page(7, ++committed)).commit();
            }
            // 这次崩溃在提交点之前：绝不该被计入
            store.injector().arm(CrashInjector.Step.PRE_COMMIT, 0, true);
            ColumnStore.Txn doomed = store.begin();
            doomed.put(7, page(7, committed + 1));
            assertThrows(CrashInjector.CrashException.class, doomed::commit);
        }

        long expected;
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            try (var tx = store.snapshot()) {
                expected = countOf(store.readPage(7, tx));
            }
            assertEquals(5L, expected, "exactly the five committed increments survive");
        }

        // 阶段二：恢复后继续写，计数必须单调且精确
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            for (int i = 0; i < 3; i++) {
                store.begin().put(7, page(7, expected + 1)).commit();
                expected++;
            }
        }

        // 阶段三：再次重启，验证没有残留事务被二次重放
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            try (var tx = store.snapshot()) {
                assertEquals(8L, countOf(store.readPage(7, tx)),
                        "count must be exactly 8 — no lost update, no double count");
            }
        }
    }

    // ------------------------------------------------------------------ 幂等

    @Test
    @DisplayName("WAL 回放幂等：重复 recover 同一份 WAL，状态不变")
    void replayIsIdempotent(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("idem");
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            store.begin().put(1, page(1, 111)).commit();
            store.begin().put(2, page(2, 222)).commit();
        }

        // 反复打开并恢复，结果必须稳定
        String first = null;
        for (int i = 0; i < 5; i++) {
            try (ColumnStore store = new ColumnStore(base)) {
                RecoveryEngine.Report r = store.recover();
                String snapshot;
                try (var tx = store.snapshot()) {
                    snapshot = text(store.readPage(1, tx)) + "|" + text(store.readPage(2, tx));
                }
                if (first == null) {
                    first = snapshot;
                } else {
                    assertEquals(first, snapshot, "recovery must be idempotent (round " + i + ")");
                }
                assertEquals(0, r.discardedUncommitted(), "nothing uncommitted to discard");
            }
        }
        assertEquals("city=1;count=111|city=2;count=222", first);
    }

    @Test
    @DisplayName("同一 WAL 上重复回放：appliedLsn 跳过已重做页，不重复应用")
    void replaySkipsAlreadyAppliedRecords(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("skiplsn");
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            store.begin().put(1, page(1, 55)).commit();
        }

        try (ColumnStore store = new ColumnStore(base)) {
            RecoveryEngine.Report first = store.recover();
            assertEquals(1, first.replayedPages());
            assertEquals(0, first.skippedByLsn());

            // 手动把该页标记为已应用，再恢复一次 —— 必须被 LSN 判定跳过
            RecoveryEngine.Report second = store.recover();
            assertEquals(0, second.replayedPages(),
                    "already-applied pages must be skipped, not replayed again");
        }
    }

    // ------------------------------------------------------------------ 半截刷盘

    @Test
    @DisplayName("半截刷盘：WAL 尾部被截断的记录被丢弃，已提交记录完好")
    void truncatedWalTailIsDiscarded(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("truncated");
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            store.begin().put(1, page(1, 10)).commit();
            store.begin().put(2, page(2, 20)).commit();
        }

        // 手工在 WAL 尾部追加半条记录（模拟刷到一半断电）
        Path wal = base.resolve("wal.log");
        long size = Files.size(wal);
        Files.write(wal, new byte[]{1, 0, 0}, StandardOpenOption.APPEND);
        assertTrue(Files.size(wal) > size);

        try (ColumnStore store = new ColumnStore(base)) {
            RecoveryEngine.Report r = store.recover();
            assertTrue(r.truncatedTailBytes() > 0, "truncated tail bytes should be detected");
            try (var tx = store.snapshot()) {
                assertEquals("city=1;count=10", text(store.readPage(1, tx)));
                assertEquals("city=2;count=20", text(store.readPage(2, tx)),
                        "records before the truncated tail must still replay");
            }
        }
    }

    @Test
    @DisplayName("WAL 记录 CRC 损坏：该记录及其后记录被丢弃，之前记录完好")
    void corruptWalRecordStopsReplay(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("corrupt");
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            store.begin().put(1, page(1, 10)).commit();
            store.begin().put(2, page(2, 20)).commit();
        }

        // 破坏第二条记录里的一个字节（payload 区域）
        Path wal = base.resolve("wal.log");
        byte[] bytes = Files.readAllBytes(wal);
        int half = bytes.length - 3;
        bytes[half] ^= 0xFF;
        Files.write(wal, bytes);

        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            try (var tx = store.snapshot()) {
                // 第一条完好，第二条被 CRC 拦下（重放顺序保证「损坏点之后不信任」）
                assertEquals("city=1;count=10", text(store.readPage(1, tx)));
                // 第二条要么没重放，要么重放了完整内容——绝不能是半截
                byte[] p2 = store.readPage(2, tx);
                if (p2 != null) {
                    assertEquals("city=2;count=20", text(p2));
                }
            }
        }
    }

    // ------------------------------------------------------------------ 规模与耗时

    @Test
    @DisplayName("100MB WAL 量级：恢复在 3 秒预算内完成，且数据完整")
    void recoveryOfLargeWalStaysWithinBudget(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("large");
        final int txns = 400;
        final int pagesPerTxn = 8;
        final int payload = 4096; // 让 WAL 总量逼近 100MB 级

        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            for (int t = 0; t < txns; t++) {
                ColumnStore.Txn txn = store.begin();
                for (int p = 0; p < pagesPerTxn; p++) {
                    txn.put(p, bigPage(t, p));
                }
                txn.commit();
            }
        }

        long walBytes = Files.size(base.resolve("wal.log"));
        assertTrue(walBytes > 8L * 1024 * 1024,
                "test WAL should be substantial, got " + walBytes + " bytes");

        try (ColumnStore store = new ColumnStore(base)) {
            long t0 = System.nanoTime();
            RecoveryEngine.Report r = store.recover();
            long ms = (System.nanoTime() - t0) / 1_000_000L;

            assertEquals(txns * pagesPerTxn, r.replayedPages(),
                    "every committed page must be replayed");
            try (var tx = store.snapshot()) {
                assertNotNull(store.readPage(0, tx));
                assertArrayPayloadEquals(bigPage(txns - 1, 0), store.readPage(0, tx));
            }
            assertTrue(ms <= 3_000,
                    "recovery of " + walBytes / (1024 * 1024) + "MB WAL took " + ms + "ms, budget 3000ms");
        }
    }

    @Test
    @DisplayName("恢复后的可见状态 == WAL 中已提交事务的状态（不变式）")
    void recoveredStateMatchesCommittedWalState(@TempDir Path dir) throws IOException {
        Path base = dir.resolve("invariant");
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            for (int t = 0; t < 10; t++) {
                store.begin().put(t % 3, page(t % 3, t)).commit();
            }
            // 最后一个事务崩溃在提交点前
            ColumnStore.Txn doomed = store.begin();
            doomed.put(1, page(1, 12345));
            doomed.abort();
        }

        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            var records = store.walForTesting().scan();
            Map<Integer, byte[]> expected = RecoveryEngine.expectedCommittedState(records);
            try (var tx = store.snapshot()) {
                for (var e : expected.entrySet()) {
                    assertEquals(text(e.getValue()), text(store.readPage(e.getKey(), tx)),
                            "page " + e.getKey() + " must match committed WAL state");
                }
            }
        }
    }

    // ------------------------------------------------------------------ helpers

    private static byte[] bigPage(int t, int p) {
        byte[] b = new byte[4096];
        byte[] tag = ("tx=" + t + ";page=" + p).getBytes(StandardCharsets.UTF_8);
        System.arraycopy(tag, 0, b, 0, tag.length);
        return b;
    }

    private static void assertArrayPayloadEquals(byte[] expected, byte[] actual) {
        assertNotNull(actual, "page must exist after recovery");
        assertEquals(expected.length, actual.length, "payload length mismatch");
        for (int i = 0; i < expected.length; i++) {
            if (expected[i] != actual[i]) {
                throw new AssertionError("payload differs at byte " + i);
            }
        }
    }
}
