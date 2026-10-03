package com.carrier.sigstore;

import java.nio.file.Path;
import java.util.List;
import java.io.PrintStream;

/**
 * 子进程入口：用指定的 {@code -XX:hashCode} 种子打开数据库，把规范化查询结果打到 stdout。
 *
 * <p>存在意义：确定性的真正对手是<b>进程间</b>的哈希种子变化（String.hashCode 随机化）。
 * 只在同一个 JVM 内重复执行，种子不变，证明不了任何跨进程可复现性。
 */
public final class DeterminismChild {

    private DeterminismChild() {
    }

    public static void main(String[] args) throws Exception {
        Path dir = Path.of(args[0]);
        Schema schema = TestSupport.schema();
        try (ColumnStore store = ColumnStore.open(dir, schema, new CrashInjector())) {
            QueryEngine q = new QueryEngine(store);
            StringBuilder sb = new StringBuilder();
            try (ColumnStore.ReadSnapshot snap = store.beginRead()) {
                Filter.And f = new Filter.And(schema)
                        .eq(TestSupport.CITY, 2)
                        .eq(TestSupport.PLAN, 3)
                        .eq(TestSupport.BRAND, 5);
                for (Row r : q.scan(f, List.of(), snap)) sb.append(r).append('\n');
                for (QueryEngine.Bucket b : q.aggregateByBucket(f, TestSupport.BUCKET, snap)) {
                    sb.append(b.key()).append('=').append(b.count()).append('\n');
                }
                for (QueryEngine.TopEntry e : q.topN(f, TestSupport.AMOUNT, 10, snap)) {
                    sb.append(e.value()).append('@').append(e.rowId()).append('\n');
                }
                for (QueryEngine.GroupCount gc : q.groupCount(
                        f, List.of(TestSupport.CITY, TestSupport.PLAN, TestSupport.BRAND), snap)) {
                    sb.append(java.util.Arrays.toString(gc.values())).append("->").append(gc.count()).append('\n');
                }
            }
            PrintStream out = new PrintStream(System.out, true, java.nio.charset.StandardCharsets.UTF_8);
            out.print(sb);
        }
    }
}
