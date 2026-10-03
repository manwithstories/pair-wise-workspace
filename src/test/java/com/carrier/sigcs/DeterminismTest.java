package com.carrier.sigcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Random;
import java.util.Set;
import java.util.TreeMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.stream.Collectors;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 确定性迭代：同一语句序列重复执行，行序与聚合序完全一致。
 *
 * <p>回归的真实投诉是「同一 SQL 两次 Top10 顺序不同」——顺序不确定性会让结果无法复现、
 * 看起来像在造假。这里的每条断言都要求 {@code equals} 级一致，而不只是「数量对」。
 */
class DeterminismTest {

    // ------------------------------------------------------------------ 测试数据

    /** 构造一份典型的信令画像数据：地市 / 套餐 / 终端品牌 / 小时 / 流量。 */
    private static Map<String, long[]> sampleColumns(int rows, long seed) {
        Random rnd = new Random(seed);
        long[] city = new long[rows];
        long[] plan = new long[rows];
        long[] brand = new long[rows];
        long[] hour = new long[rows];
        long[] bytes = new long[rows];

        for (int i = 0; i < rows; i++) {
            city[i] = rnd.nextInt(6);      // 地市：6 个
            plan[i] = rnd.nextInt(4);      // 套餐：4 个
            brand[i] = rnd.nextInt(8);      // 终端品牌：8 个
            hour[i] = rnd.nextInt(48);     // 48 小时
            bytes[i] = rnd.nextInt(10_000);
        }
        Map<String, long[]> cols = new LinkedHashMap<>();
        cols.put("city", city);
        cols.put("plan", plan);
        cols.put("brand", brand);
        cols.put("hour", hour);
        cols.put("bytes", bytes);
        return cols;
    }

    private static Set<String> numericCols() {
        return Set.of("city", "plan", "brand", "hour", "bytes");
    }

    private static ColumnStore storeWith(Path dir, int rows, long seed) throws IOException {
        ColumnStore store = new ColumnStore(dir);
        store.recover();
        store.createFamily("cdr", sampleColumns(rows, seed), rows, numericCols());
        return store;
    }

    /** 把聚合结果渲染成稳定字符串，用于逐字符比较。 */
    private static String render(List<ColumnStore.AggRow> rows) {
        return rows.stream()
                .map(r -> r.groupKey() + "|" + r.bucket() + "|" + r.count() + "|" + r.sum())
                .collect(Collectors.joining("\n"));
    }

    // ------------------------------------------------------------------ 重复执行一致性

    @Test
    @DisplayName("同一条聚合语句重复执行 200 次，结果逐字符一致")
    void repeatedAggregationIsByteIdentical(@TempDir Path dir) throws IOException {
        try (ColumnStore store = storeWith(dir.resolve("cdr"), 5_000, 42L)) {
            List<BitmapIndex.Predicate> preds = List.of(
                    BitmapIndex.Predicate.eq("city", 2),
                    BitmapIndex.Predicate.eq("plan", 1));

            String expected = render(store.aggregate("cdr", List.of("city", "plan"),
                    "hour", 1, "bytes", preds));

            for (int i = 0; i < 200; i++) {
                String actual = render(store.aggregate("cdr", List.of("city", "plan"),
                        "hour", 1, "bytes", preds));
                assertEquals(expected, actual, "run #" + i + " diverged from the first run");
            }
        }
    }

    @Test
    @DisplayName("谓词书写顺序不同，结果完全相同（求交顺序无关）")
    void predicateOrderDoesNotAffectResult(@TempDir Path dir) throws IOException {
        try (ColumnStore store = storeWith(dir.resolve("cdr"), 5_000, 7L)) {
            List<BitmapIndex.Predicate> a = List.of(
                    BitmapIndex.Predicate.eq("city", 1),
                    BitmapIndex.Predicate.eq("plan", 2),
                    BitmapIndex.Predicate.range("hour", 3, 20));
            List<BitmapIndex.Predicate> b = List.of(
                    BitmapIndex.Predicate.range("hour", 3, 20),
                    BitmapIndex.Predicate.eq("plan", 2),
                    BitmapIndex.Predicate.eq("city", 1));
            List<BitmapIndex.Predicate> c = List.of(
                    BitmapIndex.Predicate.eq("plan", 2),
                    BitmapIndex.Predicate.eq("city", 1),
                    BitmapIndex.Predicate.range("hour", 3, 20));

            String ra = render(store.aggregate("cdr", List.of("city"), "hour", 1, "bytes", a));
            assertEquals(ra, render(store.aggregate("cdr", List.of("city"), "hour", 1, "bytes", b)));
            assertEquals(ra, render(store.aggregate("cdr", List.of("city"), "hour", 1, "bytes", c)));
        }
    }

    // ------------------------------------------------------------------ 回归：Top10 顺序

    @Test
    @DisplayName("回归投诉：Top10 平局时顺序依然稳定，200 次完全一致")
    void topNOrderIsStableUnderTies(@TempDir Path dir) throws IOException {
        // 刻意造出大量<b>完全相同</b>的分组：每个分组的行数与度量都相等，
        // 于是 Top10 完全由平局裁决 —— 这正是「两次 Top10 顺序不同」的土壤。
        final int groups = 64;
        final int rowsPerGroup = 20;
        final int rows = groups * rowsPerGroup;
        long[] g = new long[rows];
        long[] hour = new long[rows];
        long[] bytes = new long[rows];
        for (int i = 0; i < rows; i++) {
            g[i] = i % groups;              // 64 个分组
            hour[i] = 0;                    // 同一个时间桶 => 每组恰好一行输出
            bytes[i] = 500;                 // 每行度量相同 => 每组 sum 相同
        }
        Map<String, long[]> cols = new LinkedHashMap<>();
        cols.put("g", g);
        cols.put("hour", hour);
        cols.put("bytes", bytes);

        try (ColumnStore store = new ColumnStore(dir.resolve("cdr"))) {
            store.recover();
            store.createFamily("cdr", cols, rows, numericCols());

            List<ColumnStore.AggRow> agg = store.aggregate("cdr", List.of("g"), "hour", 1, "bytes", List.of());
            assertEquals(groups, agg.size(), "each group should produce exactly one row");

            // 确认这 64 行确实全是平局，否则测试没有意义
            long distinctSums = agg.stream().map(ColumnStore.AggRow::sum).distinct().count();
            assertEquals(1L, distinctSums, "all groups must tie for this test to be meaningful");

            String expected = render(ColumnStore.topN(agg, 10));
            for (int i = 0; i < 200; i++) {
                assertEquals(expected, render(ColumnStore.topN(agg, 10)),
                        "Top10 order diverged on run #" + i);
            }
        }
    }

    @Test
    @DisplayName("Top-N 满足排序契约：度量非递增，且完全相等的两行按分组键升序")
    void topNRespectsTotalOrder(@TempDir Path dir) throws IOException {
        try (ColumnStore store = storeWith(dir.resolve("cdr"), 8_000, 5L)) {
            List<ColumnStore.AggRow> agg = store.aggregate("cdr", List.of("city", "brand"),
                    "hour", 24, "bytes", List.of());
            List<ColumnStore.AggRow> top = ColumnStore.topN(agg, 10);

            for (int i = 1; i < top.size(); i++) {
                var prev = top.get(i - 1);
                var cur = top.get(i);
                if (prev.sum() == cur.sum()) {
                    int cmp = prev.groupKey().compareTo(cur.groupKey());
                    assertTrue(cmp < 0 || (cmp == 0 && prev.bucket() < cur.bucket()),
                            "tie must break by groupKey then bucket: " + prev + " vs " + cur);
                } else {
                    assertTrue(prev.sum() > cur.sum(), "sums must be non-increasing");
                }
            }
        }
    }

    @Test
    @DisplayName("把输入列表打乱后再取 TopN，结果与原顺序一致（对输入顺序不敏感）")
    void topNIsIndependentOfInputOrder(@TempDir Path dir) throws IOException {
        try (ColumnStore store = storeWith(dir.resolve("cdr"), 6_000, 11L)) {
            List<ColumnStore.AggRow> agg = store.aggregate("cdr", List.of("city", "plan"),
                    "hour", 6, "bytes", List.of());

            String canonical = render(ColumnStore.topN(agg, 10));

            List<ColumnStore.AggRow> shuffled = new ArrayList<>(agg);
            // 固定种子的确定性洗牌，确保这个测试自己也是可复现的
            Collections.shuffle(shuffled, new Random(2024));
            assertEquals(canonical, render(ColumnStore.topN(shuffled, 10)),
                    "Top-N must not depend on the order rows were discovered in");
        }
    }

    // ------------------------------------------------------------------ 并发下确定性

    @Test
    @DisplayName("32 路虚拟线程并发跑同一查询，结果与单线程完全一致")
    void concurrentQueriesAgreeWithSingleThreaded(@TempDir Path dir) throws Exception {
        try (ColumnStore store = storeWith(dir.resolve("cdr"), 10_000, 3L)) {
            List<BitmapIndex.Predicate> preds = List.of(
                    BitmapIndex.Predicate.eq("city", 4),
                    BitmapIndex.Predicate.eq("brand", 5));
            String expected = render(store.aggregate("cdr", List.of("city", "brand"),
                    "hour", 1, "bytes", preds));
            String expectedTop = render(ColumnStore.topN(
                    store.aggregate("cdr", List.of("city", "brand"), "hour", 1, "bytes", preds), 10));

            final int threads = 32;
            final int iterations = 25;
            CountDownLatch start = new CountDownLatch(1);
            Set<String> distinctResults = java.util.Collections.synchronizedSet(new LinkedHashSet<>());
            List<Future<?>> futures = new ArrayList<>();

            try (ExecutorService pool = Executors.newVirtualThreadPerTaskExecutor()) {
                for (int i = 0; i < threads; i++) {
                    futures.add(pool.submit(() -> {
                        start.await();
                        for (int n = 0; n < iterations; n++) {
                            var agg = store.aggregate("cdr", List.of("city", "brand"),
                                    "hour", 1, "bytes", preds);
                            distinctResults.add(render(agg) + "||" + render(ColumnStore.topN(agg, 10)));
                        }
                        return null;
                    }));
                }
                start.countDown();
                for (Future<?> f : futures) {
                    f.get(120, TimeUnit.SECONDS);
                }
            }

            assertEquals(1, distinctResults.size(),
                    "concurrent identical queries produced " + distinctResults.size() + " distinct results");
            assertEquals(expected + "||" + expectedTop, distinctResults.iterator().next());
        }
    }

    // ------------------------------------------------------------------ 无哈希随机化泄漏

    @Test
    @DisplayName("分组输出按字典序排列，不受 HashMap 迭代顺序影响")
    void groupOutputIsLexicographic(@TempDir Path dir) throws IOException {
        try (ColumnStore store = storeWith(dir.resolve("cdr"), 4_000, 17L)) {
            List<ColumnStore.AggRow> agg = store.aggregate("cdr", List.of("city", "plan"),
                    "hour", 24, "bytes", List.of());

            List<String> keys = new ArrayList<>();
            for (ColumnStore.AggRow r : agg) {
                if (!keys.contains(r.groupKey())) {
                    keys.add(r.groupKey());
                }
            }
            List<String> sorted = new ArrayList<>(keys);
            java.util.Collections.sort(sorted);
            assertEquals(sorted, keys, "group keys must come out in lexicographic order");

            // 交叉验证：与纯 TreeMap 实现的参考结果比对
            Map<String, Long> reference = new TreeMap<>();
            List<Integer> rows = store.filterRows("cdr", List.of());
            for (int row : rows) {
                String key = store.valueAt("cdr", "city", row) + "" + store.valueAt("cdr", "plan", row);
                reference.merge(key, store.valueAt("cdr", "bytes", row), Long::sum);
            }
            Map<String, Long> actual = new TreeMap<>();
            for (ColumnStore.AggRow r : agg) {
                actual.merge(r.groupKey(), r.sum(), Long::sum);
            }
            assertEquals(reference, actual);
        }
    }

    @Test
    @DisplayName("不同插入顺序建出的库，聚合结果仍然相同")
    void resultIndependentOfInsertionOrder(@TempDir Path dir) throws IOException {
        int rows = 5_000;
        Map<String, long[]> forward = sampleColumns(rows, 21L);

        // 构造一份逐行反序的数据
        Map<String, long[]> reversed = new LinkedHashMap<>();
        for (var e : forward.entrySet()) {
            long[] src = e.getValue();
            long[] dst = new long[rows];
            for (int i = 0; i < rows; i++) {
                dst[i] = src[rows - 1 - i];
            }
            reversed.put(e.getKey(), dst);
        }

        List<BitmapIndex.Predicate> preds = List.of(BitmapIndex.Predicate.eq("city", 3));

        String a;
        try (ColumnStore s1 = new ColumnStore(dir.resolve("fwd"))) {
            s1.recover();
            s1.createFamily("cdr", forward, rows, numericCols());
            a = render(s1.aggregate("cdr", List.of("city", "plan"), "hour", 2, "bytes", preds));
        }
        String b;
        try (ColumnStore s2 = new ColumnStore(dir.resolve("rev"))) {
            s2.recover();
            s2.createFamily("cdr", reversed, rows, numericCols());
            b = render(s2.aggregate("cdr", List.of("city", "plan"), "hour", 2, "bytes", preds));
        }
        assertEquals(a, b, "row insertion order must not affect the aggregation result");
    }

    // ------------------------------------------------------------------ 位图下推行为

    @Test
    @DisplayName("多列等值过滤下推为位图 AND，与暴力扫描结果一致")
    void bitmapPushdownMatchesBruteForce(@TempDir Path dir) throws IOException {
        int rows = 20_000;
        try (ColumnStore store = storeWith(dir.resolve("cdr"), rows, 77L)) {
            List<BitmapIndex.Predicate> preds = List.of(
                    BitmapIndex.Predicate.eq("city", 1),
                    BitmapIndex.Predicate.eq("plan", 2),
                    BitmapIndex.Predicate.eq("brand", 5));

            List<Integer> viaBitmap = store.filterRows("cdr", preds);

            List<Integer> brute = new ArrayList<>();
            for (int row = 0; row < rows; row++) {
                if (store.valueAt("cdr", "city", row) == 1
                        && store.valueAt("cdr", "plan", row) == 2
                        && store.valueAt("cdr", "brand", row) == 5) {
                    brute.add(row);
                }
            }
            assertEquals(brute, viaBitmap, "bitmap AND must equal the brute-force scan");
        }
    }

    @Test
    @DisplayName("区间过滤走分段位图，结果与暴力扫描一致且行号升序")
    void rangePredicateMatchesBruteForce(@TempDir Path dir) throws IOException {
        int rows = 20_000;
        try (ColumnStore store = storeWith(dir.resolve("cdr"), rows, 88L)) {
            List<BitmapIndex.Predicate> preds = List.of(
                    BitmapIndex.Predicate.range("hour", 10, 25),
                    BitmapIndex.Predicate.eq("city", 2));
            List<Integer> viaBitmap = store.filterRows("cdr", preds);

            List<Integer> brute = new ArrayList<>();
            for (int row = 0; row < rows; row++) {
                long h = store.valueAt("cdr", "hour", row);
                if (h >= 10 && h <= 25 && store.valueAt("cdr", "city", row) == 2) {
                    brute.add(row);
                }
            }
            assertEquals(brute, viaBitmap);

            for (int i = 1; i < viaBitmap.size(); i++) {
                assertTrue(viaBitmap.get(i - 1) < viaBitmap.get(i),
                        "matched row ids must be strictly ascending");
            }
        }
    }

    @Test
    @DisplayName("过滤无命中返回空集，而不是回退全表")
    void noMatchReturnsEmpty(@TempDir Path dir) throws IOException {
        try (ColumnStore store = storeWith(dir.resolve("cdr"), 1_000, 1L)) {
            List<Integer> rows = store.filterRows("cdr",
                    List.of(BitmapIndex.Predicate.eq("city", 999_999)));
            assertTrue(rows.isEmpty(), "must return empty, not everything");
        }
    }

    @Test
    @DisplayName("未建图列参与过滤时报错，而不是静默返回错误结果")
    void unindexedColumnFailsLoudly(@TempDir Path dir) throws IOException {
        final int overLimit = ColumnStore.MAX_BITMAP_CARDINALITY + 10;
        long[] highCard = new long[overLimit];
        for (int i = 0; i < highCard.length; i++) {
            highCard[i] = i; // distinct 数超过建图阈值
        }
        Map<String, long[]> cols = new LinkedHashMap<>();
        cols.put("userId", highCard);

        try (ColumnStore store = new ColumnStore(dir.resolve("toomany"))) {
            store.recover();
            store.createFamily("cdr", cols, overLimit, numericCols());

            assertTrue(!store.indexedColumns("cdr").contains("userId"),
                    "columns above the cardinality threshold must not be indexed");

            // 关键：不能静默返回一个「看起来合理」的错误结果，必须显式失败。
            assertThrows(IllegalStateException.class,
                    () -> store.aggregate("cdr", List.of("userId"), "userId", 1, "userId",
                            List.of(BitmapIndex.Predicate.eq("userId", 5))),
                    "filtering on a non-indexed column must fail loudly");
        }
    }

    @Test
    @DisplayName("千万行位图过滤在 50ms 预算内完成")
    void bitmapFilterMeetsLatencyBudget(@TempDir Path dir) throws IOException {
        int rows = 10_000_000;
        long[] city = new long[rows];
        long[] plan = new long[rows];
        long[] brand = new long[rows];
        for (int i = 0; i < rows; i++) {
            city[i] = i % 300;        // 300 个地市
            plan[i] = i % 40;         // 40 种套餐
            brand[i] = i % 50;        // 50 个品牌
        }
        Map<String, long[]> cols = new LinkedHashMap<>();
        cols.put("city", city);
        cols.put("plan", plan);
        cols.put("brand", brand);

        try (ColumnStore store = new ColumnStore(dir.resolve("big"))) {
            store.recover();
            store.createFamily("cdr", cols, rows, numericCols());

            List<BitmapIndex.Predicate> preds = List.of(
                    BitmapIndex.Predicate.eq("city", 7),
                    BitmapIndex.Predicate.eq("plan", 3),
                    BitmapIndex.Predicate.eq("brand", 11));

            // 预热，避免把 JIT 编译时间算进预算
            for (int i = 0; i < 3; i++) {
                store.filterRows("cdr", preds);
            }

            // 命中行数可先验证：city%300==7 && plan%40==3 && brand%50==11
            long expectedHits = 0;
            for (int i = 0; i < rows; i++) {
                if (city[i] == 7 && plan[i] == 3 && brand[i] == 11) {
                    expectedHits++;
                }
            }
            assertEquals(expectedHits, store.filterRows("cdr", preds).size(),
                    "bitmap filter must return the exact match count");

            long best = Long.MAX_VALUE;
            for (int i = 0; i < 5; i++) {
                long t0 = System.nanoTime();
                store.filterRows("cdr", preds);
                best = Math.min(best, System.nanoTime() - t0);
            }
            long ms = best / 1_000_000L;
            assertTrue(ms <= 50,
                    "bitmap filter over " + rows + " rows took " + ms + "ms, budget 50ms");
        }
    }

    @Test
    @DisplayName("点查（单页读）在 5ms 预算内完成")
    void pointLookupMeetsLatencyBudget(@TempDir Path dir) throws IOException {
        try (ColumnStore store = new ColumnStore(dir.resolve("point"))) {
            store.recover();
            for (int p = 0; p < 64; p++) {
                store.begin().put(p, new byte[512]).commit();
            }
            // 预热
            for (int i = 0; i < 50; i++) {
                try (var tx = store.snapshot()) {
                    store.readPage(i % 64, tx);
                }
            }
            long worst = 0;
            for (int i = 0; i < 2_000; i++) {
                long t0 = System.nanoTime();
                try (var tx = store.snapshot()) {
                    store.readPage(i % 64, tx);
                }
                worst = Math.max(worst, System.nanoTime() - t0);
            }
            long us = worst / 1_000L;
            assertTrue(us <= 5_000,
                    "worst-case point lookup took " + us + "us, budget 5000us");
        }
    }
}
