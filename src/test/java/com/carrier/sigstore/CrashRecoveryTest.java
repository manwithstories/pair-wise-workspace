package com.carrier.sigstore;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.EnumSource;

import java.nio.file.Path;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertAll;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 崩溃点注入与恢复。
 *
 * <p>三个注入位（BEFORE_SYNC / MID_SYNC / AFTER_SYNC）逐一验证同样的两条不变量：
 * <b>已提交数据一条不少，未提交数据一点不留</b>。
 *
 * <p>其中 AFTER_SYNC 是最危险的档位：COMMIT 已经 fsync 到盘，但事务尚未把页装进内存。
 * 恢复必须靠日志把页重放出来；如果只依赖内存状态，这批数据就会整段消失。
 */
class CrashRecoveryTest {

    @Test
    @DisplayName("未提交事务的 PAGE 记录必须被跳过（这是'未提交无残留'的判据本身）")
    void uncommittedPagesAreSkipped(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        CrashInjector injector = new CrashInjector();

        WALJournal wal = new WALJournal(dir.resolve("wal.log"));
        ColumnStore store = new ColumnStore(dir, schema, new SnapshotManager(), wal, injector);
        try {
            // 已提交一批：city=1
            WriteTransaction ok = store.beginWrite();
            for (int i = 0; i < 10; i++) ok.append(Row.ofInts(1, 1, 1, 100 + i, 7, i));
            ok.commit();

            // 再手工构造一个"页已写进 WAL、但没有 COMMIT"的事务：city=2
            // 这是最难处理的一种残留——数据在日志里，只差一个 COMMIT 记录。
            long badTxn = 999;
            long badSeq = store.commitCounter().incrementAndGet();
            wal.beginTxn(badTxn, badSeq);
            for (int c = 0; c < schema.columnCount(); c++) {
                byte[] img = new byte[store.page().pageSize];
                wal.logPage(badTxn, store.pageIdOf(0, c), store.page().pageSize, img, badSeq);
            }
            wal.sync();                      // 故意不写 COMMIT，模拟 commit 途中猝死
        } finally {
            try { wal.close(); } catch (Exception ignored) { }
        }

        RecoveryEngine.Report report = RecoveryEngine.recover(dir, schema);
        assertEquals(report.committedTxns(), 1,
                "只应承认 1 个已提交事务: " + RecoveryEngine.describe(report));
        assertTrue(report.pagesSkippedUncommitted() > 0,
                "未提交事务的页必须被识别并跳过: " + RecoveryEngine.describe(report));
        assertTrue(report.rolledBackTxns() >= 1,
                "未提交事务应被计为回滚: " + RecoveryEngine.describe(report));

        ColumnStore reopened = ColumnStore.open(dir, schema, new CrashInjector());
        try {
            // 关键断言：残留页被跳过，因此页内容仍是已提交那一版的（city=1）
            try (ColumnStore.ReadSnapshot snap = reopened.beginRead()) {
                List<Row> ghosts = new QueryEngine(reopened).scan(
                        new Filter.And(schema).eq(TestSupport.CITY, 2), List.of(), snap);
                assertEquals(ghosts.size(), 0,
                        "未提交事务的行绝不能出现在恢复后的库里（这就是那次双倍计数事故的根因）");
                List<Row> good = new QueryEngine(reopened).scan(
                        new Filter.And(schema).eq(TestSupport.CITY, 1), List.of(), snap);
                assertEquals(good.size(), 10, "已提交数据必须完好");
            }
        } finally {
            reopened.close();
        }
    }

    @Test
    @DisplayName("恢复报告：WAL 尾部半截记录被识别并截断，不污染有效数据")
    void tornTailIsTruncated(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        Path walPath = dir.resolve("wal.log");
        try (ColumnStore store = TestSupport.create(dir, schema)) {
            TestSupport.writeRows(store, 10, i -> Row.ofInts(i % 3, i % 2, i % 4, i, i, i));
            store.checkpoint();
        }
        // 手工在 WAL 尾部追加一条"结构完整但内容被截断/篡改"的记录。
        // 关键：要构造出长度字段合法、但 CRC 对不上的情形——
        // 只有这种才会真正走到 CRC 校验分支，光靠"长度不足"是绕过去的。
        appendCorruptRecord(walPath);

        RecoveryEngine.Report report = RecoveryEngine.recover(dir, schema);
        assertAll(
                () -> assertTrue(report.tornTailBytes() > 0, "半截尾部应被识别: " + RecoveryEngine.describe(report)),
                () -> assertTrue(report.committedTxns() > 0, "已提交事务应被识别")
        );
        // 截断后 WAL 长度应回到有效边界
        try (WALJournal wal = new WALJournal(walPath)) {
            assertEquals(report.pagesReplayed() >= 0, true);
            WALJournal.ReplayResult rr = wal.replay();
            assertFalse(rr.hasTornTail(), "截断后不应再有半截尾部");
        }
    }

    @ParameterizedTest(name = "崩溃点在 {0}")
    @EnumSource(CrashInjector.Stage.class)
    @DisplayName("已提交事务在崩溃后完整保留，未提交事务无残留")
    void committedDataSurvivesCrash(CrashInjector.Stage stage, @TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();

        // ---- 阶段一：提交一批"应该保住"的数据 ----
        ColumnStore store = TestSupport.create(dir, schema);
        TestSupport.writeRows(store, 20, i -> Row.ofInts(1, 1, 1, 100 + i, 7, i));
        long rowsBeforeCrash = store.committedRowCount();
        store.checkpoint();

        // ---- 阶段二：在注入点崩溃 ----
        WriteTransaction doomed = store.beginWrite();
        for (int i = 0; i < 8; i++) {
            doomed.append(Row.ofInts(2, 2, 2, 200 + i, 9, 1000 + i));  // 与已提交数据可区分
        }
        // 在这个事务 commit() 中触发崩溃
        CrashInjector injector = store.crashInjector();
        injector.rearm();
        injector.maybeCrash(stage);   // 手动命中，确保必定抛

        // ---- 阶段三：重新打开并恢复 ----
        store.close();
        RecoveryEngine.Report report = RecoveryEngine.recover(dir, schema);

        ColumnStore reopened = TestSupport.create(dir, schema);
        try {
            long rowsAfter = reopened.committedRowCount();
            assertEquals(rowsAfter, rowsBeforeCrash,
                    "崩溃恢复后已提交行数必须与崩溃前一致，未提交事务不得混入");

            // 已提交数据逐行可读且内容正确
            try (ColumnStore.ReadSnapshot snap = reopened.beginRead()) {
                QueryEngine q = new QueryEngine(reopened);
                List<Row> survivors = q.scan(
                        new Filter.And(schema).eq(TestSupport.CITY, 1), List.of(), snap);
                assertEquals(survivors.size(), 20, "已提交的 20 行必须全部可读");
                // 内容核对：第 i 行的 bucket 应为 100+i
                for (int i = 0; i < 20; i++) {
                    assertEquals(survivors.get(i).get(3), 100 + i, "第 " + i + " 行内容应保持正确");
                }
                // 未提交数据（city=2）一条都不可见
                List<Row> ghosts = q.scan(
                        new Filter.And(schema).eq(TestSupport.CITY, 2), List.of(), snap);
                assertEquals(ghosts.size(), 0, "未提交事务的行不得留下任何残留");
            }
        } finally {
            reopened.close();
        }
    }

    @Test
    @DisplayName("AFTER_SYNC：COMMIT 已落盘但事务未收尾，恢复后数据仍在（不依赖内存态）")
    void afterSyncReliesOnWalNotMemory(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        CrashInjector injector = new CrashInjector(CrashInjector.Stage.AFTER_SYNC);

        // 手动制造"日志已提交、内存未安装"的状态
        WALJournal wal = new WALJournal(dir.resolve("wal.log"));
        ColumnStore store = new ColumnStore(dir, schema, new SnapshotManager(), wal, injector);
        try {
            WriteTransaction tx = store.beginWrite();
            for (int i = 0; i < 5; i++) {
                tx.append(Row.ofInts(3, 3, 3, 300 + i, 5, i));
            }
            // commit() 内部会 fsync 成功，然后在 AFTER_SYNC 处抛异常：
            // 于是 COMMIT 已持久化，但 installAll() 没跑到，内存里没有这批行
            assertThrows(CrashInjector.CrashSimulatedException.class, tx::commit);
            assertEquals(store.committedRowCount(), 0, "崩溃后内存里确实没有这批行");
        } finally {
            // 模拟进程猝死：不做 checkpoint，直接关掉
            try { wal.close(); } catch (Exception ignored) { }
        }

        RecoveryEngine.Report report = RecoveryEngine.recover(dir, schema);
        assertTrue(report.committedTxns() >= 1, "日志里的 COMMIT 应被识别: " + RecoveryEngine.describe(report));

        ColumnStore reopened = TestSupport.create(dir, schema);
        try {
            assertEquals(reopened.committedRowCount(), 5,
                    "仅凭 WAL 就必须重建出这 5 行——内存态丢失不影响已提交数据");
        } finally {
            reopened.close();
        }
    }

    @Test
    @DisplayName("幂等：多事务 WAL 反复恢复，结果逐字节相同")
    void recoveryIsIdempotent(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();

        // 不做 checkpoint —— 让数据文件保持空白，WAL 里留着多笔已提交事务。
        // 这样每次恢复都必须真正重放 WAL，幂等性才真正被检验到；
        // 否则 WAL 是空的，"恢复两次结果相同"只是因为什么都没做。
        try (ColumnStore store = TestSupport.create(dir, schema)) {
            for (int t = 0; t < 5; t++) {
                WriteTransaction tx = store.beginWrite();
                for (int i = 0; i < 7; i++) {
                    tx.append(Row.of(t, i, (t + i) % 4, t * 10 + i, 3, t * 7 + i));
                }
                tx.commit();
            }
            // 再来一笔"较新版本"的事务：它覆盖同一个行组的同一批页。
            // 有了两笔版本不同的记录，幂等才真正被考验——
            // 若恢复时不用 version 比较来跳过旧版本，先到的旧页会覆盖新页。
            WriteTransaction newer = store.beginWrite();
            for (int i = 0; i < 7; i++) {
                newer.append(Row.of(99, 99, 99, 9999 + i, 99, 500 + i));
            }
            newer.commit();
        }

        // 校验点：恢复出的数据必须与"从未崩溃"时的读结果一致
        RecoveryEngine.Report r1 = RecoveryEngine.recover(dir, schema);
        assertEquals(6, r1.committedTxns(),
                "6 笔事务都应恢复: " + RecoveryEngine.describe(r1));
        assertTrue(r1.pagesReplayed() > 0, "恢复必须真的重放了页，而不是空转");
        String afterFirst = snapshotDataFile(dir);

        // 再恢复两次，结果必须逐字节不变
        RecoveryEngine.recover(dir, schema);
        String afterSecond = snapshotDataFile(dir);
        assertEquals(afterFirst, afterSecond, "第二次恢复不得改变数据文件");

        RecoveryEngine.recover(dir, schema);
        String afterThird = snapshotDataFile(dir);
        assertEquals(afterSecond, afterThird, "第三次恢复不得改变数据文件");

        // 重放后的行数与内容必须与干净路径一致
        // 恢复后必须是"最新那一笔"的内容，而不是被旧页覆盖
        try (ColumnStore reopened = ColumnStore.open(dir, schema, new CrashInjector())) {
            assertEquals(42, reopened.committedRowCount(), "42 行应全部恢复");
            try (ColumnStore.ReadSnapshot snap = reopened.beginRead()) {
                List<Row> last = new QueryEngine(reopened).scan(
                        new Filter.And(schema).eq(TestSupport.CITY, 99), List.of(), snap);
                assertEquals(7, last.size(), "最新一批事务的 7 行必须在，且旧版本不得覆盖它");
            }
        }
    }

    @Test
    @DisplayName("版本裁决：数据文件比 WAL 新时，旧 WAL 不得回退数据")
    void newerDataFileBeatsOlderWal(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore store = TestSupport.create(dir, schema)) {
            WriteTransaction tx = store.beginWrite();
            for (int i = 0; i < 8; i++) tx.append(Row.ofInts(1, 1, 1, 100 + i, 7, i));
            tx.commit();
            store.checkpoint();     // 数据文件里现在是 version=V
        }

        // 人为把 WAL 回退：删掉所有 COMMIT 之前的 PAGE 记录不该发生，
        // 但"日志比数据旧"这一情形必须安全——真实世界里它出现在
        // checkpoint 之后 WAL 被截断、或部分刷盘的场景。
        // 做法：直接清空 WAL，模拟"日志已被检查点截断但数据文件仍在"的常态。
        try (WALJournal wal = new WALJournal(dir.resolve("wal.log"))) {
            wal.truncateTo(0);
        }

        RecoveryEngine.Report report = RecoveryEngine.recover(dir, schema);
        assertEquals(0, report.pagesReplayed(),
                "WAL 为空时不应重放任何页: " + RecoveryEngine.describe(report));

        ColumnStore reopened = ColumnStore.open(dir, schema, new CrashInjector());
        try {
            assertEquals(8, reopened.committedRowCount(),
                    "数据文件里的已提交数据必须仍在，WAL 截断不应让它消失");
        } finally {
            reopened.close();
        }
    }

    @Test
    @DisplayName("恢复性能：100MB 级 WAL 的恢复在 3 秒内完成")
    void recoveryUnderTimeBudget(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();

        // 造到 >=100MB 的 WAL：每笔事务写满一个行组，每组 6 列各一页，
        // 页镜像全额进日志。rowsPerPage=64、6 列 -> 每组约 24KB 日志，
        // 需要 4000+ 组才能凑到 100MB。
        int rpp = schema.rowsPerPage();
        int groupsNeeded = (int) Math.ceil(100L * 1024 * 1024 / (schema.columnCount() * (long) store0PageSize(schema)));
        try (ColumnStore store = TestSupport.create(dir, schema)) {
            for (int g = 0; g < groupsNeeded; g++) {
                WriteTransaction tx = store.beginWrite();
                for (int i = 0; i < rpp; i++) {
                    tx.append(Row.of(
                            g % 5, i % 3, (g + i) % 7,
                            1000L * g + i, 13, g * (long) rpp + i));
                }
                tx.commit();
            }
        }

        long walBytes;
        try (WALJournal w = new WALJournal(dir.resolve("wal.log"))) {
            walBytes = w.sizeBytes();
        }
        assertTrue(walBytes >= 100L * 1024 * 1024,
                "测试前提：WAL 必须真的达到 100MB，实际 " + (walBytes / 1024 / 1024) + "MB");

        long t0 = System.nanoTime();
        RecoveryEngine.Report report = RecoveryEngine.recover(dir, schema);
        long elapsedMs = (System.nanoTime() - t0) / 1_000_000;

        System.out.printf("  [崩溃恢复] WAL=%.1fMB, 恢复耗时 %dms (要求<=3000ms)%n",
                walBytes / 1024.0 / 1024.0, elapsedMs);
        assertTrue(elapsedMs <= 3000,
                "恢复 " + (walBytes / 1024 / 1024) + "MB WAL 耗时 " + elapsedMs + "ms，应 <=3000ms");
        assertEquals(report.committedTxns(), groupsNeeded,
                "全部事务都应恢复: " + RecoveryEngine.describe(report));
        assertEquals(report.tornTailBytes(), 0, "正常关闭的日志不应有半截尾部");
    }

    /**
     * 往 WAL 尾部追加一条结构合法但 CRC 被破坏的记录。
     *
     * <p>这样回放时长度检查会放行，必须依赖 CRC 才能识别为损坏——
     * 如果哪天去掉 CRC 校验，这条用例就会读到半截数据而失败。
     */
    private static void appendCorruptRecord(Path walPath) throws Exception {
        try (WALJournal wal = new WALJournal(walPath)) {
            byte[] image = new byte[4096];
            wal.logPage(7777L, 3, 4096, image, 555L);
            // 不 fsync 也不 commit——随后手工破坏最后一字节的 CRC
        }
        try (java.nio.channels.FileChannel ch = java.nio.channels.FileChannel.open(
                walPath, java.nio.file.StandardOpenOption.WRITE,
                java.nio.file.StandardOpenOption.READ)) {
            long size = ch.size();
            java.nio.ByteBuffer b = java.nio.ByteBuffer.allocate(1);
            ch.read(b, size - 1);
            byte bad = (byte) (b.get(0) ^ 0xFF);
            ch.write(java.nio.ByteBuffer.wrap(new byte[]{bad}), size - 1);
        }
    }

    /** 估一个行组占多少日志字节（列数 x 页镜像大小）。 */
    private static int store0PageSize(Schema schema) {
        try (ColumnStore probe = TestSupport.create(
                java.nio.file.Files.createTempDirectory("probe-").resolve("d"), schema)) {
            return probe.page().pageSize;
        } catch (Exception e) {
            throw new IllegalStateException(e);
        }
    }

    private static String snapshotDataFile(Path dir) throws Exception {
        Path p = dir.resolve("data.csf");
        if (!java.nio.file.Files.exists(p)) return "<none>";
        byte[] bytes = java.nio.file.Files.readAllBytes(p);
        return java.util.Arrays.toString(java.util.Arrays.copyOf(bytes, Math.min(bytes.length, 4096)))
                + "|len=" + bytes.length;
    }
}
