package com.carrier.sigstore;

import java.util.concurrent.ConcurrentSkipListSet;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.LongAdder;

/**
 * 无锁 MVCC 快照注册中心。
 *
 * <p>模型：
 * <ul>
 *   <li>全局只有一个单调递增的 <b>提交序号 commitSeq</b>。任何一次成功 commit 都会把它推进到新值。</li>
 *   <li>读事务通过 {@link #acquire()} 抓一个快照号 S，之后整个事务内所有读取都以 S 为准，
 *       因此同一事务多次读到的是同一份视图（快照隔离）。</li>
 *   <li>写路径从不阻塞读路径：写只往页版本链上挂新版本，读者沿链回溯找 &lt;= S 的版本。</li>
 *   <li>回收旧版本只需要知道"当前最老的活跃快照" minActiveSeq。minActiveSeq 之下的页版本
 *       除了最靠上的那一个之外都可以释放。</li>
 * </ul>
 *
 * <p>关键不变量：{@code lastCommitSeq} 必须在事务<b>所有</b>页版本和位图增量都安装完之后才推进，
 * 否则会���中途可见的半截事务暴露给快照号为新值的读者。
 */
public final class SnapshotManager implements AutoCloseable {

    /** 全局提交序号。0 保留为"空洞/未写入"哨兵。 */
    private final AtomicLong lastCommitSeq = new AtomicLong(0L);

    /** 活跃快照集合：ConcurrentSkipListSet.first() 即 minActiveSeq，且顺序确定不依赖哈希种子。 */
    private final ConcurrentSkipListSet<Long> activeSnapshots = new ConcurrentSkipListSet<>();

    private final AtomicLong nextSnapshotId = new AtomicLong(1L);

    private final LongAdder snapshotCount = new LongAdder();
    private final LongAdder closedSnapshotCount = new LongAdder();

    private volatile boolean closed;

    /** 读取当前已提交水位线，即"此刻开启的读事务能看到的最大提交号"。 */
    public long currentCommitSeq() {
        return lastCommitSeq.get();
    }

    /** 注册一个新快照；调用方必须 close()。 */
    public Snapshot acquire() {
        if (closed) throw new IllegalStateException("SnapshotManager 已关闭");
        long seq = lastCommitSeq.get();
        Snapshot s = new Snapshot(this, nextSnapshotId.getAndIncrement(), seq);
        // 先登记再返回：seq 是从 lastCommitSeq 读到的，登记动作只会让 min 更小（更保守），
        // 不会让读者提前看到未完成的提交。
        activeSnapshots.add(s.commitSeq);
        snapshotCount.increment();
        return s;
    }

    void release(Snapshot s) {
        if (activeSnapshots.remove(s.commitSeq)) {
            closedSnapshotCount.increment();
        }
    }

    /** 提交路径在页 + 位图全部落定后调用。只会单调前进。 */
    void advanceTo(long commitSeq) {
        lastCommitSeq.accumulateAndGet(commitSeq, Math::max);
    }

    /**
     * 当前最老的活跃快照号；没有活跃快照时退化为 lastCommitSeq（此时可回收除最新外的全部旧版本）。
     */
    public long minActiveSeq() {
        if (activeSnapshots.isEmpty()) {
            return lastCommitSeq.get();
        }
        return activeSnapshots.first();
    }

    public int activeSnapshotCount() {
        return activeSnapshots.size();
    }

    public long totalSnapshots() {
        return snapshotCount.sum();
    }

    public long releasedSnapshots() {
        return closedSnapshotCount.sum();
    }

    @Override
    public void close() {
        closed = true;
        activeSnapshots.clear();
    }

    /** 一个不可变的读视图句柄。commitSeq 一经分配永不改变。 */
    public static final class Snapshot implements AutoCloseable {
        private final SnapshotManager owner;
        private final long snapshotId;
        private final long commitSeq;
        private volatile boolean valid = true;

        Snapshot(SnapshotManager owner, long snapshotId, long commitSeq) {
            this.owner = owner;
            this.snapshotId = snapshotId;
            this.commitSeq = commitSeq;
        }

        public long snapshotId() {
            return snapshotId;
        }

        public long commitSeq() {
            return commitSeq;
        }

        public boolean isValid() {
            return valid;
        }

        @Override
        public void close() {
            if (valid) {
                valid = false;
                owner.release(this);
            }
        }
    }
}
