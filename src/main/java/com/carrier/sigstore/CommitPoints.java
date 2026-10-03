package com.carrier.sigstore;

import java.util.Arrays;

/**
 * 提交点索引：记录"提交序号 -&gt; 该提交后的可见行数"，是一条只追加的有序表。
 *
 * <p>这是快照可见性的关键。因为行按提交顺序追加，<b>提交序号单调即行号单调</b>，
 * 所以对任意快照号 S 只需回答一个问题：
 * <blockquote>序号 &le; S 的提交一共产生了多少行？</blockquote>
 * 答案就是对提交点表做一次二分。读者由此可以无锁裁掉"快照之后"的行，
 * 既不需要读锁，也不依赖位图自身携带时间戳。
 *
 * <p>发布方式是一个不可变 {@link State}（表 + 有效长度）整体替换 volatile 引用。
 * 读者只做<b>一次</b> volatile 读，因此绝不会看到"长度已更新但数据还没写"的中间态，
 * 也不会像分别读 table/size 那样读到撕裂的组合。
 *
 * <p>刻意用有序数组而非 TreeMap：读路径是纯二分，没有任何哈希参与。
 * 这对确定性是必要的——哈希随机化绝不能泄漏到可见行数，进而泄漏到结果行数。
 */
public final class CommitPoints {

    /** 不可变的已发布状态：pairs[0..len) 有效，每两个元素是一组 (commitSeq,rowCount)。 */
    private record State(long[] pairs, int len) {
        public long[] pairs() {
            return pairs;
        }

        public int len() {
            return len;
        }
    }

    private static final State EMPTY = new State(new long[0], 0);

    private volatile State state = EMPTY;
    private final Object writeLock = new Object();

    /** 记录一次提交。必须在页版本全部安装完毕、水位线推进之前调用。 */
    public void record(long commitSeq, long rowCount) {
        synchronized (writeLock) {
            State cur = state;
            int len = cur.len();
            long[] pairs;
            if (len + 2 <= cur.pairs().length) {
                pairs = cur.pairs();
            } else {
                int cap = Math.max(8, cur.pairs().length * 2);
                pairs = new long[cap];
                System.arraycopy(cur.pairs(), 0, pairs, 0, len);
            }
            pairs[len] = commitSeq;
            pairs[len + 1] = rowCount;
            state = new State(pairs, len + 2);     // volatile 写，一次发布
        }
    }

    /**
     * 快照 S 可见的行数。二分找最后一个 {@code commitSeq <= S} 并返回其行数；
     * 没有这样的提交则返回 0。
     */
    public long visibleRows(long snapshotSeq) {
        State s = state;                 // 唯一一次 volatile 读；此后全部基于该不可变快照
        long[] p = s.pairs();
        int n = s.len() / 2;
        if (n == 0) return 0L;
        int lo = 0, hi = n - 1, found = -1;
        while (lo <= hi) {
            int mid = (lo + hi) >>> 1;
            if (p[mid * 2] <= snapshotSeq) {
                found = mid;
                lo = mid + 1;
            } else {
                hi = mid - 1;
            }
        }
        return found < 0 ? 0L : p[found * 2 + 1];
    }

    /**
     * 恢复后重建：单个提交点 (lastCommitSeq, rowCount)。
     *
     * <p>重启后无需保留历史的每一次提交——旧快照本就不跨进程存活，
     * 只需保证"当前水位线"与"可见行数"这一对自洽即可。
     */
    public void restore(long lastCommitSeq, long rowCount) {
        synchronized (writeLock) {
            long[] pairs = new long[2];
            pairs[0] = lastCommitSeq;
            pairs[1] = rowCount;
            state = new State(pairs, lastCommitSeq > 0 ? 2 : 0);
        }
    }

    public int commitCount() {
        return state.len() / 2;
    }

    public long lastCommitSeq() {
        State s = state;
        int n = s.len() / 2;
        return n == 0 ? 0L : s.pairs()[(n - 1) * 2];
    }

    /** 确定性导出：按提交序号升序的 (commitSeq-&gt;rowCount)，供测试断言。 */
    public String describe() {
        State s = state;
        StringBuilder sb = new StringBuilder("[");
        for (int i = 0; i < s.len() / 2; i++) {
            if (i > 0) sb.append(", ");
            sb.append('(').append(s.pairs()[i * 2]).append("->").append(s.pairs()[i * 2 + 1]).append(')');
        }
        return sb.append(']').toString();
    }

    /** 已提交提交点升序快照，测试用。 */
    public long[][] entries() {
        State s = state;
        int n = s.len() / 2;
        long[][] out = new long[n][2];
        for (int i = 0; i < n; i++) {
            out[i][0] = s.pairs()[i * 2];
            out[i][1] = s.pairs()[i * 2 + 1];
        }
        return out;
    }

    @Override
    public String toString() {
        return "CommitPoints" + describe();
    }
}
