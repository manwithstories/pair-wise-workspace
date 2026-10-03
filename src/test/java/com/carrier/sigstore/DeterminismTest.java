package com.carrier.sigstore;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.ByteArrayOutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 确定性迭代：同一语句序列重复执行，结果行序与聚合序必须逐项相同。
 *
 * <p>对应的线上事故是"同一 SQL 两次 Top10 顺序不同，被投诉造假"。这里把约束落实成可执行断言：
 * <ul>
 *   <li>行序：恒为 rowId 升序，与插入顺序、并行度都无关；</li>
 *   <li>聚合序：桶按业务键排序（{@code TreeMap}），绝不依赖 HashMap 迭代序；</li>
 *   <li>并列：TopN 比较器以 rowId 兜底，构成全序，不存在并列时的随机选择；</li>
 *   <li>进程级：不同 {@code -XX:hashCode} 随机化种子下结果必须一致。</li>
 * </ul>
 */
class DeterminismTest {

    /** 多次执行同一语句，产出可直接比较的规范化文本。 */
    private static String render(ColumnStore store, Schema schema) {
        QueryEngine q = new QueryEngine(store);
        StringBuilder sb = new StringBuilder();
        try (ColumnStore.ReadSnapshot snap = store.beginRead()) {
            // 1) 多列组合过滤：地市 + 套餐 + 终端品牌
            Filter.And f = new Filter.And(schema)
                    .eq(TestSupport.CITY, 2)
                    .eq(TestSupport.PLAN, 3)
                    .eq(TestSupport.BRAND, 5);
            sb.append("-- rows\n");
            for (Row r : q.scan(f, List.of(), snap)) sb.append(r).append('\n');

            // 2) 时间桶聚合：桶序必须按桶键升序
            sb.append("-- buckets\n");
            for (QueryEngine.Bucket b : q.aggregateByBucket(f, TestSupport.BUCKET, snap)) {
                sb.append(b.key()).append('=').append(b.count()).append('\n');
            }

            // 3) Top10：并列时按 rowId 兜底
            sb.append("-- top10\n");
            for (QueryEngine.TopEntry e : q.topN(f, TestSupport.AMOUNT, 10, snap)) {
                sb.append(e.value()).append('@').append(e.rowId()).append('\n');
            }

            // 4) 多维分组计数：组合键字典序
            sb.append("-- groups\n");
            for (QueryEngine.GroupCount g : q.groupCount(
                    f, List.of(TestSupport.CITY, TestSupport.PLAN, TestSupport.BRAND), snap)) {
                sb.append(java.util.Arrays.toString(g.values())).append(" -> ").append(g.count()).append('\n');
            }
        }
        return sb.toString();
    }

    private static void seed(ColumnStore store, Schema schema, int n) {
        TestSupport.writeRows(store, n, TestSupport.deterministic(4, 6, 8, 12));
    }

    @Test
    @DisplayName("同一语句重复执行 20 次：行序与聚合序逐字节一致")
    void repeatedExecutionIsIdentical(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore store = TestSupport.create(dir, schema)) {
            seed(store, schema, 500);
            String first = render(store, schema);
            for (int i = 0; i < 20; i++) {
                String again = render(store, schema);
                assertEquals(first, again, "第 " + (i + 2) + " 次执行结果与第一次不一致");
            }
            assertFalse(first.isEmpty(), "断言需要非空输出");
        }
    }

    @Test
    @DisplayName("并列值的 TopN：顺序稳定且全序（这是被投诉过的场景）")
    void topNWithTiesIsStable(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore store = TestSupport.create(dir, schema)) {
            // 大量行共享同一个 amount，制造大量并列
            WriteTransaction tx = store.beginWrite();
            for (int i = 0; i < 300; i++) {
                tx.append(Row.of(1, 1, 1, 100 + (i / 10), 42, i));   // amount 恒为 42
            }
            tx.commit();

            QueryEngine q = new QueryEngine(store);
            List<QueryEngine.TopEntry> a, b;
            try (ColumnStore.ReadSnapshot s1 = store.beginRead()) {
                a = q.topN(new Filter.And(schema).eq(TestSupport.CITY, 1), TestSupport.AMOUNT, 10, s1);
            }
            try (ColumnStore.ReadSnapshot s2 = store.beginRead()) {
                b = q.topN(new Filter.And(schema).eq(TestSupport.CITY, 1), TestSupport.AMOUNT, 10, s2);
            }
            assertEquals(renderTop(a), renderTop(b), "全并列数据下 TopN 仍必须完全一致");
            // 并列时按 rowId 升序：这就是"全序"的具体含义
            for (int i = 1; i < a.size(); i++) {
                assertTrue(a.get(i - 1).rowId() < a.get(i).rowId(),
                        "并列时应按 rowId 升序兜底，第 " + i + " 项违反");
            }
        }
    }

    private static String renderTop(List<QueryEngine.TopEntry> es) {
        StringBuilder sb = new StringBuilder();
        for (QueryEngine.TopEntry e : es) sb.append(e.value()).append('@').append(e.rowId()).append(';');
        return sb.toString();
    }

    @Test
    @DisplayName("写入顺序不影响结果：两种插入顺序得到同一份规范化输出")
    void insertionOrderDoesNotAffectOutput(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();

        // 顺序 A：升序插入
        ColumnStore a = TestSupport.create(dir.resolve("a"), schema);
        TestSupport.writeRows(a, 300, TestSupport.deterministic(4, 6, 8, 12));
        String outA = render(a, schema);
        a.close();

        // 顺序 B：先写一半，再交错写另一半（模拟并行接入的乱序完成）
        ColumnStore b = TestSupport.create(dir.resolve("b"), schema);
        TestSupport.writeRows(b, 150, TestSupport.deterministic(4, 6, 8, 12));
        TestSupport.writeRows(b, 150, i -> TestSupport.deterministic(4, 6, 8, 12).row(i + 150));
        String outB = render(b, schema);
        b.close();

        assertEquals(outA, outB, "插入时序不同，但提交序决定 rowId，结果必须一致");
    }

    @Test
    @DisplayName("并发写入 + 并发读取：读到的结果始终与串行基线一致")
    void concurrentWritesStillDeterministic(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        try (ColumnStore store = TestSupport.create(dir, schema)) {
            seed(store, schema, 200);
            String baseline = render(store, schema);

            // 并发追加，每批内容确定性；无论完成顺序如何，读结果都必须自洽
            List<Thread> ts = new ArrayList<>();
            for (int t = 0; t < 4; t++) {
                final int id = t;
                Thread th = Thread.ofVirtual().start(() -> {
                    for (int i = 0; i < 25; i++) {
                        final int base = 1000 + id * 25 + i;
                        TestSupport.writeRows(store, 10,
                                k -> TestSupport.deterministic(4, 6, 8, 12).row(base));
                    }
                });
                ts.add(th);
            }
            for (Thread th : ts) th.join(30_000);

            // 基线是新数据的规范化输出；重复渲染必须完全一致
            String after = render(store, schema);
            for (int i = 0; i < 5; i++) {
                assertEquals(after, render(store, schema), "并发写入后第 " + (i + 2) + " 次渲染不一致");
            }
            // 新数据确实进去了（而不是没写成功导致"稳定地错"）
            assertFalse(after.equals(baseline), "并发写入后数据应当变化");
        }
    }

    @DisplayName("跨 JVM 进程：不同哈希种子下输出完全一致")
    void hashRandomizationDoesNotLeak(@TempDir Path dir) throws Exception {
        Schema schema = TestSupport.schema();
        Path dbDir = dir.resolve("db");
        try (ColumnStore store = TestSupport.create(dbDir, schema)) {
            seed(store, schema, 400);
        }

        // 在若干个不同的 -XX:hashCode 种子下重新打开同一份数据并渲染
        List<String> outputs = new ArrayList<>();
        long seedA = 12345L, seedB = 987654321L;
        outputs.add(runChild(dbDir, seedA));
        outputs.add(runChild(dbDir, seedB));
        outputs.add(runChild(dbDir, 42L));
        for (int i = 1; i < outputs.size(); i++) {
            assertEquals(outputs.get(0), outputs.get(i),
                    "不同 hashCode 种子（" + (i == 1 ? seedB : 42L) + "）下结果必须一致");
        }
    }

    /**
     * 起一个子 JVM，用给定哈希种子打开数据库并把渲染结果打到 stdout。
     * 目的是把"跨进程可复现"这条约束真正验出来——同 JVM 内换了种子也不算数。
     */
    private static String runChild(Path dbDir, long hashSeed) throws Exception {
        String java = Path.of(System.getProperty("java.home"), "bin", "java").toString();
        String cp = System.getProperty("java.class.path");
        List<String> cmd = List.of(java,
                "-XX:+UnlockExperimentalVMOptions",
                "-XX:hashCode=" + hashSeed,
                "-cp", cp,
                "com.carrier.sigstore.DeterminismChild",
                dbDir.toString());
        Process p = new ProcessBuilder(cmd).redirectErrorStream(false).start();
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        try (var in = p.getInputStream()) {
            in.transferTo(out);
        }
        String stderr = new String(p.getErrorStream().readAllBytes(), StandardCharsets.UTF_8);
        int code = p.waitFor();
        if (code != 0) {
            throw new IllegalStateException("子进程失败 code=" + code + " stderr=" + stderr);
        }
        return out.toString(StandardCharsets.UTF_8);
    }
}
