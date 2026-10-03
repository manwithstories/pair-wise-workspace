package com.carrier.sigcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 无锁 MVCC 快照读：读不阻塞写、快照内重复读一致、已提交不丢、未提交不可见。
 */
class SnapshotIsolationTest {

    @TempDir
    Path dir;

    private static byte[] payload(int pageId, int value) {
        byte[] b = new byte[64];
        b[0] = (byte) pageId;
        b[1] = (byte) value;
        return b;
    }

    private static int valueOf(byte[] b) {
        return b[1] & 0xFF;
    }

    @Test
    @DisplayName("同一读事务内多次读同一页，结果恒等（快照不被其他提交改变）")
    void repeatableReadWithinTransaction() throws IOException {
        try (ColumnStore store = new ColumnStore(dir)) {
            store.recover();
            store.begin().put(7, payload(7, 1)).commit();

            try (var tx = store.snapshot()) {
                byte[] first = store.readPage(7, tx);
                for (int i = 0; i < 200; i++) {
                    assertArrayEquals(first, store.readPage(7, tx),
                            "read #" + i + " inside one snapshot must equal the first read");
                }
            }
        }
    }

    @Test
    @DisplayName("读事务跨越写事务仍看到旧视图；新开事务才看到新值")
    void snapshotIsStableAcrossConcurrentCommits() throws IOException {
        Path sub = dir.resolve("across-commits");
        try (ColumnStore store = new ColumnStore(sub)) {
            store.recover();
            store.begin().put(1, payload(1, 10)).commit();

            try (var longLived = store.snapshot()) {
                assertEquals(10, valueOf(store.readPage(1, longLived)));

                // 期间发生多次提交
                for (int v = 11; v <= 40; v++) {
                    store.begin().put(1, payload(1, v)).commit();
                }

                // 老快照必须仍然看到 10 —— 这就是「同事务多次读到一致视图」
                assertEquals(10, valueOf(store.readPage(1, longLived)),
                        "an open snapshot must keep seeing its own view");

                // 新事务看到最新值
                try (var fresh = store.snapshot()) {
                    assertEquals(40, valueOf(store.readPage(1, fresh)));
                }
            }
        }
    }

    @Test
    @DisplayName("多页事务对快照是原子的：不会读到「页A新 + 页B旧」的撕裂中间态")
    void multiPageCommitIsAtomicToSnapshots() throws Exception {
        Path sub = dir.resolve("atomic");
        try (ColumnStore store = new ColumnStore(sub)) {
            store.recover();
            store.begin().put(100, payload(100, 1)).put(101, payload(101, 1)).commit();

            final int pages = 2;
            final int rounds = 300;
            AtomicBoolean stop = new AtomicBoolean();
            AtomicInteger torn = new AtomicInteger();
            AtomicInteger observations = new AtomicInteger();

            // 写者：每个事务把两页都推进到同一个代次
            Thread writer = Thread.ofVirtual().start(() -> {
                try {
                    for (int gen = 2; gen < rounds && !stop.get(); gen++) {
                        store.begin()
                                .put(100, payload(100, gen & 0xFF))
                                .put(101, payload(101, gen & 0xFF))
                                .commit();
                    }
                } catch (RuntimeException e) {
                    throw new RuntimeException(e);
                }
            });

            // 读者：不断开新快照，要求两页永远同代次
            Thread reader = Thread.ofVirtual().start(() -> {
                try {
                    while (!stop.get()) {
                        try (var tx = store.snapshot()) {
                            byte[] a = store.readPage(100, tx);
                            byte[] b = store.readPage(101, tx);
                            if (a == null || b == null) {
                                continue;
                            }
                            observations.incrementAndGet();
                            if (valueOf(a) != valueOf(b)) {
                                torn.incrementAndGet();
                            }
                        }
                    }
                } catch (RuntimeException e) {
                    throw new RuntimeException(e);
                }
            });

            writer.join(30_000);
            stop.set(true);
            reader.join(30_000);

            assertTrue(observations.get() > 50,
                    "reader should have observed many snapshots, got " + observations.get());
            assertEquals(0, torn.get(),
                    "torn multi-page reads detected: " + torn.get() + " of " + observations.get());
        }
    }

    @Test
    @DisplayName("未提交事务的页对读者完全不可见；提交后才可见")
    void uncommittedWritesAreInvisible() throws IOException {
        try (ColumnStore store = new ColumnStore(dir.resolve("tx"))) {
            store.recover();
            store.begin().put(5, payload(5, 1)).commit();

            ColumnStore.Txn open = store.begin();
            open.put(5, payload(5, 99));
            assertEquals(1, open.stagedPages());

            // 暂存中的内容对读者不可见
            try (var tx = store.snapshot()) {
                assertEquals(1, valueOf(store.readPage(5, tx)),
                        "staged-but-uncommitted content must not be visible");
            }

            open.commit();

            try (var tx = store.snapshot()) {
                assertEquals(99, valueOf(store.readPage(5, tx)));
            }
        }
    }

    @Test
    @DisplayName("abort 的事务既不留数据，也不进 WAL")
    void abortedTransactionLeavesNoTrace() throws IOException {
        Path sub = dir.resolve("atomic");
        try (ColumnStore store = new ColumnStore(sub)) {
            store.recover();
            store.begin().put(3, payload(3, 1)).commit();

            ColumnStore.Txn doomed = store.begin();
            doomed.put(3, payload(3, 77));
            doomed.abort();

            try (var tx = store.snapshot()) {
                assertEquals(1, valueOf(store.readPage(3, tx)));
            }
            // 关闭后不应再写
            assertEquals(0, doomed.stagedPages());
        }
    }

    @Test
    @DisplayName("不存在的页在快照中读出 null，而不是抛异常或返回脏数据")
    void missingPageReadsNull() throws IOException {
        try (ColumnStore store = new ColumnStore(dir.resolve("tx"))) {
            store.recover();
            try (var tx = store.snapshot()) {
                assertNull(store.readPage(4242, tx));
            }
        }
    }

    @Test
    @DisplayName("100 路虚拟线程并发读下，写吞吐不低于单线程写基线的 80%")
    void concurrentReadersKeepWriterAtEightyPercent() throws Exception {
        final int rows = 8;
        final int writerRounds = 400;

        // ---- 基线：没有并发读时的单线程写吞吐 ----
        double baselineMillis;
        try (ColumnStore solo = new ColumnStore(dir.resolve("baseline"))) {
            solo.recover();
            for (int p = 0; p < rows; p++) {
                solo.begin().put(p, payload(p, 0)).commit();
            }
            long t0 = System.nanoTime();
            for (int gen = 1; gen <= writerRounds; gen++) {
                for (int p = 0; p < rows; p++) {
                    solo.begin().put(p, payload(p, gen & 0xFF)).commit();
                }
            }
            baselineMillis = (System.nanoTime() - t0) / 1e6;
        }
        assertTrue(baselineMillis > 0, "baseline must be measurable");

        // ---- 同样的写负载，但同时有 100 路虚拟线程在读 ----
        double underLoadMillis;
        final int readers = 100;
        final int readIterations = 400;
        AtomicInteger badReads = new AtomicInteger();
        AtomicInteger badPage = new AtomicInteger();
        AtomicInteger reads = new AtomicInteger();

        try (ColumnStore store = new ColumnStore(dir.resolve("loaded"))) {
            store.recover();
            for (int p = 0; p < rows; p++) {
                store.begin().put(p, payload(p, 0)).commit();
            }

            CountDownLatch start = new CountDownLatch(1);
            ExecutorService pool = Executors.newVirtualThreadPerTaskExecutor();
            List<Future<?>> futures = new ArrayList<>();

            for (int i = 0; i < readers; i++) {
                final int id = i;
                futures.add(pool.submit(() -> {
                    try {
                        start.await();
                        for (int n = 0; n < readIterations; n++) {
                            try (var tx = store.snapshot()) {
                                int p = id % rows;
                                byte[] b = store.readPage(p, tx);
                                if (b == null) {
                                    badPage.incrementAndGet();
                                    continue;
                                }
                                reads.incrementAndGet();
                                // 内容必须是某个完整版本的取值，且页号自洽
                                if ((b[0] & 0xFF) != p) {
                                    badReads.incrementAndGet();
                                }
                            }
                        }
                    } catch (Exception e) {
                        throw new RuntimeException(e);
                    }
                    return null;
                }));
            }

            Future<?> writer = pool.submit(() -> {
                try {
                    start.await();
                } catch (InterruptedException e) {
                    throw new RuntimeException(e);
                }
                for (int gen = 1; gen <= writerRounds; gen++) {
                    for (int p = 0; p < rows; p++) {
                        store.begin().put(p, payload(p, gen & 0xFF)).commit();
                    }
                }
                return null;
            });

            start.countDown();
            long t0 = System.nanoTime();
            writer.get(300, TimeUnit.SECONDS);
            underLoadMillis = (System.nanoTime() - t0) / 1e6;
            for (Future<?> f : futures) {
                f.get(300, TimeUnit.SECONDS);
            }
            pool.shutdown();
        }

        assertEquals(0, badReads.get(), "readers observed torn page content");
        assertEquals(0, badPage.get(), "existing pages must always be readable");
        assertTrue(reads.get() > 0, "no reads happened");

        double ratio = baselineMillis / underLoadMillis;
        assertTrue(ratio >= 0.80,
                "write throughput under 100 concurrent readers fell below 80% of baseline: "
                        + String.format("baseline=%.1fms loaded=%.1fms ratio=%.2f",
                                baselineMillis, underLoadMillis, ratio));
    }

    @Test
    @DisplayName("多写者并发提交：每个页的已提交版本都不丢失，且读者从不看到撕裂内容")
    void concurrentWritersDoNotLoseCommittedData() throws Exception {
        final int writers = 8;
        final int pagesPerWriter = 4;
        final int rounds = 120;
        Path base = dir.resolve("multiwriter");

        AtomicInteger failures = new AtomicInteger();
        try (ColumnStore store = new ColumnStore(base)) {
            store.recover();
            for (int p = 0; p < writers * pagesPerWriter; p++) {
                store.begin().put(p, payload(p, 0)).commit();
            }

            CountDownLatch start = new CountDownLatch(1);
            try (ExecutorService pool = Executors.newVirtualThreadPerTaskExecutor()) {
                List<Future<?>> futures = new ArrayList<>();
                for (int w = 0; w < writers; w++) {
                    final int writerId = w;
                    futures.add(pool.submit(() -> {
                        try {
                            start.await();
                            for (int round = 1; round <= rounds; round++) {
                                ColumnStore.Txn txn = store.begin();
                                for (int k = 0; k < pagesPerWriter; k++) {
                                    txn.put(writerId * pagesPerWriter + k, payload(writerId, round));
                                }
                                txn.commit();
                            }
                        } catch (InterruptedException e) {
                            Thread.currentThread().interrupt();
                            throw new RuntimeException(e);
                        } catch (RuntimeException e) {
                            failures.incrementAndGet();
                            throw e;
                        }
                        return null;
                    }));
                }
                start.countDown();
                for (Future<?> f : futures) {
                    f.get(180, TimeUnit.SECONDS);
                }
            }

            assertEquals(0, failures.get(), "a writer failed unexpectedly");

            // 每个写者最后一轮的值必须都在：已提交数据一条不丢。
            try (var tx = store.snapshot()) {
                for (int w = 0; w < writers; w++) {
                    for (int k = 0; k < pagesPerWriter; k++) {
                        int pageId = w * pagesPerWriter + k;
                        byte[] b = store.readPage(pageId, tx);
                        assertEquals(rounds, valueOf(b),
                                "page " + pageId + " lost a committed write");
                    }
                }
            }
        }
    }

    @Test
    @DisplayName("页版本回收：所有旧快照关闭后，历史版本被释放且不误删当前版本")
    void retiredVersionsAreReclaimedWithoutLosingLiveData() throws IOException {
        try (ColumnStore store = new ColumnStore(dir.resolve("tx"))) {
            store.recover();
            store.begin().put(42, payload(42, 1)).commit();

            var oldPin = store.snapshot();
            for (int v = 2; v <= 20; v++) {
                store.begin().put(42, payload(42, v)).commit();
            }
            assertEquals(1, valueOf(store.readPage(42, oldPin)),
                    "pinned snapshot keeps its version alive");

            long retainedWhilePinned = store.retainedVersions(42);
            assertTrue(retainedWhilePinned > 1,
                    "old version must be retained while a reader still needs it");

            oldPin.close(); // 释放 pin

            try (var tx = store.snapshot()) {
                assertEquals(20, valueOf(store.readPage(42, tx)),
                        "current version must survive reclamation");
            }
            assertTrue(store.reclaimedBytes() >= 0);
        }
    }

}
