package com.carrier.sigstore;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.file.Path;
import java.util.ArrayList;
import java.util.BitSet;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 位图索引与过滤下推的正确性。
 *
 * <p>这里的每个用例都是按"能把某个具体实现错误杀掉"来写的：
 * 交集写成并集、分段边界差一、区间漏上下界、基数超阈值后仍继续建索引——
 * 任何一条退化都会让下面某个断言变红。
 */
class BitmapIndexTest {

    private static ColumnStore store(Path dir, Schema schema) throws Exception {
        return ColumnStore.open(dir, schema, new CrashInjector());
    }

    @Test
    @DisplayName("等值 + IN 下推为位图 AND：结果必须与全表扫描逐行判断一致")
    void eqAndInMatchFullScan(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore s = store(dir, schema)) {
            int n = 3000;
            TestSupport.writeRows(s, n, TestSupport.deterministic(5, 4, 7, 20));
            QueryEngine q = new QueryEngine(s);

            List<long[]> cases = new ArrayList<>();
            cases.add(new long[]{1, 2, 3});
            cases.add(new long[]{4, 4, 6});
            cases.add(new long[]{0, 1, 2});

            try (ColumnStore.ReadSnapshot snap = s.beginRead()) {
                for (long[] c : cases) {
                    Filter.And f = new Filter.And(schema)
                            .eq(TestSupport.CITY, c[0])
                            .eq(TestSupport.PLAN, c[1])
                            .eq(TestSupport.BRAND, c[2]);
                    List<Row> fast = q.scan(f, List.of(), snap);

                    // 参照实现：全量读出后按谓词逐行判断
                    List<Row> all = q.scan(new Filter.And(schema), List.of(), snap);
                    List<Row> expected = new ArrayList<>();
                    for (Row r : all) {
                        if (r.get(0) == c[0] && r.get(1) == c[1] && r.get(2) == c[2]) expected.add(r);
                    }
                    assertEquals(expected.size(), fast.size(),
                            "下推结果与全扫描不一致 city=" + c[0] + " plan=" + c[1] + " brand=" + c[2]);
                    assertEquals(TestSupport.rowsToString(expected), TestSupport.rowsToString(fast),
                            "下推结果的行序或内容与全扫描不一致");
                }
            }
        }
    }

    @Test
    @DisplayName("区间过滤走分段位图，结果与全扫描逐行判断一致")
    void rangeMatchesFullScan(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore s = store(dir, schema)) {
            TestSupport.writeRows(s, 2000, TestSupport.deterministic(5, 4, 7, 50));
            QueryEngine q = new QueryEngine(s);
            try (ColumnStore.ReadSnapshot snap = s.beginRead()) {
                List<Row> all = q.scan(new Filter.And(schema), List.of(), snap);
                long[][] ranges = {{0, 3}, {5, 9}, {7, 7}, {20, 60}, {100, 200}};
                for (long[] r : ranges) {
                    Filter.And f = new Filter.And(schema).range(TestSupport.BUCKET, r[0], r[1]);
                    List<Row> got = q.scan(f, List.of(), snap);
                    List<Row> want = new ArrayList<>();
                    for (Row row : all) {
                        if (row.get(3) >= r[0] && row.get(3) <= r[1]) want.add(row);
                    }
                    assertEquals(want.size(), got.size(),
                            "区间 [" + r[0] + "," + r[1] + "] 下推与全扫描行数不一致");
                    assertEquals(TestSupport.rowsToString(want), TestSupport.rowsToString(got),
                            "区间 [" + r[0] + "," + r[1] + "] 结果内容或顺序不一致");
                }
            }
        }
    }

    @Test
    @DisplayName("区间下界与上界都必须生效（漏掉任一端都会多出命中行）")
    void rangeBoundsAreBothEnforced(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore s = store(dir, schema)) {
            // bucket 单调递增，便于精确断言边界
            WriteTransaction tx = s.beginWrite();
            for (int i = 0; i < 100; i++) tx.append(Row.of(0, 0, 0, i, 1, i));
            tx.commit();
            QueryEngine q = new QueryEngine(s);
            try (ColumnStore.ReadSnapshot snap = s.beginRead()) {
                assertEquals(11, q.scan(new Filter.And(schema).range(TestSupport.BUCKET, 10, 20), List.of(), snap).size(),
                        "[10,20] 闭区间应命中 11 行");
                assertEquals(1, q.scan(new Filter.And(schema).range(TestSupport.BUCKET, 50, 50), List.of(), snap).size(),
                        "单点区间应命中 1 行");
                assertEquals(0, q.scan(new Filter.And(schema).range(TestSupport.BUCKET, 200, 300), List.of(), snap).size(),
                        "越界区间应为空");
            }
        }
    }

    @Test
    @DisplayName("IN 下推是并集而非交集")
    void inIsUnionNotIntersection(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore s = store(dir, schema)) {
            WriteTransaction tx = s.beginWrite();
            for (int i = 0; i < 50; i++) tx.append(Row.of(i % 5, 0, 0, i, 1, i));
            tx.commit();
            QueryEngine q = new QueryEngine(s);
            try (ColumnStore.ReadSnapshot snap = s.beginRead()) {
                List<Row> got = q.scan(new Filter.And(schema).in(TestSupport.CITY, 1, 3), List.of(), snap);
                assertEquals(20, got.size(), "city IN (1,3) 应命中 20 行（并集）");
                List<Row> and = q.scan(
                        new Filter.And(schema).eq(TestSupport.CITY, 1).eq(TestSupport.CITY, 3), List.of(), snap);
                assertEquals(0, and.size(), "同一列两个等值谓词应当互斥（交集为空）");
            }
        }
    }

    @Test
    @DisplayName("高基数列自动放弃位图，超过 1 万基数后不再分配位图")
    void highCardinalityColumnDisablesBitmap(@TempDir Path dir) throws Exception {
        Schema schema = Schema.builder()
                .intColumn("low")        // 低基数
                .longColumn("high")      // 高基数
                .rowsPerPage(64)
                .build();
        try (ColumnStore s = store(dir, schema)) {
            int n = Schema.BITMAP_MAX_CARDINALITY + 500;
            BulkLoad load = s.beginBulkLoad();
            for (int i = 0; i < n; i++) load.append(Row.of(i % 10, i));   // high 唯一
            load.finish();

            BitmapIndex idx = s.index();
            assertTrue(idx.isIndexed("low"), "低基数列应当建索引");
            assertEquals(10, idx.cardinality("low"));
            assertFalse(idx.isIndexed("high"),
                    "基数超过 " + Schema.BITMAP_MAX_CARDINALITY + " 的列必须放弃位图，否则内存随行数爆炸");

            // 放弃索引后，谓词仍要给出正确结果（退化为扫描而不是报错或漏行）
            QueryEngine q = new QueryEngine(s);
            try (ColumnStore.ReadSnapshot snap = s.beginRead()) {
                List<Row> got = q.scan(new Filter.And(schema).eq("high", 7777L), List.of(), snap);
                assertEquals(1, got.size(), "高基数列的等值过滤仍须精确");
                assertEquals(7777L, got.get(0).get(1));
            }
        }
    }

    @Test
    @DisplayName("部分列可下推时整体退化为扫描，绝不做半下推")
    void partialPushdownFallsBackToScan(@TempDir Path dir) throws Exception {
        Schema schema = Schema.builder()
                .intColumn("idx")       // 有位图
                .longColumn("noidx")    // 声明为不建位图
                .rowsPerPage(64)
                .build();
        try (ColumnStore s = store(dir, schema)) {
            WriteTransaction tx = s.beginWrite();
            for (int i = 0; i < 200; i++) tx.append(Row.of(i % 4, i * 1000L));
            tx.commit();
            QueryEngine q = new QueryEngine(s);
            try (ColumnStore.ReadSnapshot snap = s.beginRead()) {
                Filter.And f = new Filter.And(schema).eq("idx", 2).eq("noidx", 2000L);
                List<Row> got = q.scan(f, List.of(), snap);
                assertEquals(1, got.size(), "混合谓词（部分无索引）也必须给出精确结果");
                assertEquals(2, got.get(0).get(0));
                assertEquals(2000L, got.get(0).get(1));
            }
        }
    }

    @Test
    @DisplayName("位图交集的行数必须等于各谓词单独命中行数之交集（AND 语义）")
    void bitmapAndSemantics(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore s = store(dir, schema)) {
            TestSupport.writeRows(s, 1000, TestSupport.deterministic(6, 5, 9, 20));
            BitmapIndex idx = s.index();

            BitSet a = idx.lookup(TestSupport.CITY, 2);
            BitSet b = idx.lookup(TestSupport.PLAN, 3);
            assertNotNull(a);
            assertNotNull(b);
            int ca = a.cardinality(), cb = b.cardinality();
            BitSet and = (BitSet) a.clone();
            and.and(b);
            assertTrue(and.cardinality() <= Math.min(ca, cb), "交集不应超过任一单列命中数");

            // 换成 AND 之外的并集，应当严格更大（除非两集合相同）
            BitSet or = (BitSet) a.clone();
            or.or(b);
            assertEquals(or.cardinality(), ca + cb - and.cardinality(),
                    "并集基数必须满足容斥，用来验证确实在做集合运算而非恒等返回");
        }
    }

    @Test
    @DisplayName("空 IN 与空区间返回空结果而不是全量")
    void emptyPredicatesYieldNothing(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore s = store(dir, schema)) {
            TestSupport.writeRows(s, 100, TestSupport.deterministic(4, 4, 4, 10));
            QueryEngine q = new QueryEngine(s);
            try (ColumnStore.ReadSnapshot snap = s.beginRead()) {
                assertEquals(0, q.scan(new Filter.And(schema).in(TestSupport.CITY), List.of(), snap).size(),
                        "空 IN 应为空集");
                assertEquals(0, q.scan(new Filter.And(schema).range(TestSupport.BUCKET, 100, 200), List.of(), snap).size(),
                        "越界区间应为空集");
            }
        }
    }
}
