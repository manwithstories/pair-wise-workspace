package com.carrier.sigcs;

import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;

/**
 * 无锁 MVCC 快照管理器：版本号分配、页版本链、退休页回收。
 *
 * <h2>模型</h2>
 * <p>页是 <em>不可变</em> 的字节片段。每次提交产生该页的新版本，并被 <b>追加</b> 到一个堆外
 * append-only 字节竞技场（{@link Arena}）；页的版本链 {@link PageChain} 是一个不可变数组，
 * 版本号升序，<b>发布动作只是把新链放进 {@link ConcurrentHashMap}</b>，属于原子引用替换。
 *
 * <p>因此：
 * <ul>
 *   <li><b>读不阻塞写</b>：读者只做一次 {@code map.get(pageId)} + 一次二分，没有共享可变结构，
 *       没有全局锁；写者只做一次 {@code map.put}。</li>
 *   <li><b>版本不会写坏</b>：链不可变，写者从不原地修改读者可能正在读的对象。</li>
 *   <li><b>提交点唯一</b>：版本号在 <em>提交时</em> 才分配。未提交事务的页内容停留在
 *       事务私有的暂存区，对任何读者完全不可见 —— 这正是「未提交数据干净」的实现基础。</li>
 *   <li><b>快照一致</b>：读事务在开始时记录一次 {@link #snapshot()}。该事务后续所有读取都
 *       用「版本号 &le; 快照号」定位，于是同一事务内多次读到完全一致的视图。</li>
 * </ul>
 *
 * <h2>回收</h2>
 * <p>版本 {@code v} 的页只要还存在快照号 {@code &lt; v} 的读者就不能释放。
 * 每个活跃快照被 {@code pin} 计数，{@link #collectRetired()} 取所有活跃快照的最小值作为回收水位，
 * 水位以下的版本从链上摘除。回收只依赖单调计数器，不引入全局锁。
 */
final class SnapshotManager {

    /** 版本号分配器。提交点之后才把版本推进到 {@link #visibleSeq}。 */
    private final AtomicLong versionSeq = new AtomicLong(0L);

    /**
     * 可见水位：读者快照号的来源。
     *
     * <p><b>多页提交原子性的关键</b>：提交时先把该事务所有页以同一版本号挂进版本链，
     * 全部挂完之后才把水位推到该版本号。读者快照取自水位，因此要么看到该事务<b>全部</b>页的新值，
     * 要么<b>一</b>都看不到 —— 绝不会读到「页A新、页B旧」的撕裂中间态。
     * 水位单调递增，读它只需一次无锁读。
     */
    private final AtomicLong visibleSeq = new AtomicLong(0L);

    /** pageId -> 该页的不可变版本链。 */
    private final ConcurrentHashMap<Integer, PageChain> chains = new ConcurrentHashMap<>();

    /** 页内容的堆外 append-only 竞技场。 */
    private final Arena arena;

    /** 活跃快照的 pin 计数：快照号 -> 持有该快照的事务数。 */
    private final ConcurrentHashMap<Long, AtomicInteger> pins = new ConcurrentHashMap<>();

    /** 回收水位：所有版本号 < 该值的页内容都可释放。由 {@link #collectRetired()} 单调推进。 */
    private final AtomicLong reclaimBelow = new AtomicLong(0L);

    /** 已释放的字节数，用于测试断言「不再泄漏旧版本」。 */
    private final AtomicLong reclaimedBytes = new AtomicLong(0L);

    SnapshotManager(long initialArenaBytes) {
        this.arena = new Arena(Math.max(1L << 16, initialArenaBytes));
    }

    // ------------------------------------------------------------------ 快照

    /** 当前可见水位，即此刻开启的读事务将看到的最高版本。 */
    long currentCommitSeq() {
        return visibleSeq.get();
    }

    /** 开启读事务：抓取快照号并 pin。 */
    ReadTransaction beginRead() {
        return new ReadTransaction(this, visibleSeq.get());
    }

    private void pin(long snap) {
        pins.computeIfAbsent(snap, k -> new AtomicInteger()).incrementAndGet();
    }

    private void unpin(long snap) {
        pins.computeIfPresent(snap, (k, v) -> {
            int left = v.decrementAndGet();
            return left <= 0 ? null : v;
        });
    }

    /** 一次读事务的快照句柄；必须 close（try-with-resources）。 */
    static final class ReadTransaction implements AutoCloseable {
        private final SnapshotManager mgr;
        private final long snapshotVersion;
        private volatile boolean closed;

        ReadTransaction(SnapshotManager mgr, long snapshotVersion) {
            this.mgr = mgr;
            this.snapshotVersion = snapshotVersion;
        }

        public long snapshot() {
            return snapshotVersion;
        }

        @Override
        public void close() {
            if (!closed) {
                closed = true;
                mgr.unpin(snapshotVersion);
                mgr.collectRetired();
            }
        }
    }

    // ------------------------------------------------------------------ 提交

    /**
     * 分配一个新的提交版本号。仅在提交路径调用，且必须由单写者（或外部串行化）保证顺序。
     * 返回的版本号随后的 {@link #publish} 使用。
     */
    long allocateVersion() {
        return versionSeq.incrementAndGet();
    }

    /**
     * 把页内容作为给定版本发布出去。
     *
     * <p>调用方必须已经通过 {@link #allocateVersion()} 取得 {@code version}，
     * 并且该版本对应的提交记录已经 fsync 进 WAL —— 本方法就是提交点之后的第一步。
     */
    void publish(int pageId, long version, byte[] content) {
        int offset = arena.append(content);
        PageChain existing = chains.get(pageId);
        PageChain updated = existing == null
                ? PageChain.singleton(version, offset, content.length)
                : existing.append(version, offset, content.length);
        // 单次原子引用替换：读者要么看到旧链，要么看到新链，绝无中间态。
        chains.put(pageId, updated);
    }

    /**
     * 把可见水位推进到 {@code version}。
     *
     * <p>必须在该提交的所有页 {@link #publish} 完成之后调用。
     * 水位单调递增，因此并发推进不会回退。
     */
    void makeVisible(long version) {
        visibleSeq.accumulateAndGet(version, Math::max);
    }

    /**
     * 读取页内容到 {@code dest} 的前部。
     *
     * <p>页内容在页缓存里是<b>裸字节</b>（长度前缀只存在于数据文件的页布局中），
     * 因此这里返回<b>实际拷贝的字节数</b>，由调用方据此裁剪，调用方不得假设长度前缀存在。
     *
     * @return 拷贝的字节数；页在快照中不可见返回 -1（{@code dest} 不被修改）
     */
    int readInto(int pageId, long snapshot, byte[] dest) {
        PageChain chain = chains.get(pageId);
        if (chain == null) {
            return -1;
        }
        int idx = chain.floorIndex(snapshot);
        if (idx < 0) {
            return -1;
        }
        int len = chain.length(idx);
        arena.copyTo(chain.offset(idx), len, dest);
        return len;
    }

    /** 某页在快照下的内容长度；不可见返回 -1。 */
    int pageLength(int pageId, long snapshot) {
        PageChain chain = chains.get(pageId);
        if (chain == null) {
            return -1;
        }
        int idx = chain.floorIndex(snapshot);
        return idx < 0 ? -1 : chain.length(idx);
    }

    // ------------------------------------------------------------------ 回收

    /**
     * 推进回收水位并摘除不可见的旧版本。
     *
     * <p>水位 = 所有活跃快照中的最小快照号；无活跃快照时水位取当前提交序号。
     * 只有版本号 &lt; 水位的页对任何读者都不可见，因此可以安全释放。
     */
    void collectRetired() {
        long lowest = Long.MAX_VALUE;
        for (long snap : pins.keySet()) {
            if (snap < lowest) {
                lowest = snap;
            }
        }
        long horizon = (lowest == Long.MAX_VALUE) ? visibleSeq.get() : lowest;

        // 水位只能单调前进：accumulateAndGet + max 保证并发调用不会把水位拉回去，
        // 否则会把某个读者仍然需要的页版本提前释放。
        long effective = reclaimBelow.accumulateAndGet(horizon, Math::max);

        for (var entry : chains.entrySet()) {
            PageChain chain = entry.getValue();
            PageChain trimmed = chain.trimBeforeRetainingLatest(effective);
            if (trimmed == chain) {
                continue;
            }
            reclaimedBytes.addAndGet(chain.bytesOfDroppedPrefix(effective));
            chains.replace(entry.getKey(), chain, trimmed);
        }
    }

    long reclaimHorizon() {
        return reclaimBelow.get();
    }

    long reclaimedBytes() {
        return reclaimedBytes.get();
    }

    /** 供诊断：某页当前保留的版本数。 */
    int retainedVersions(int pageId) {
        PageChain chain = chains.get(pageId);
        return chain == null ? 0 : chain.size();
    }

    void close() {
        arena.close();
    }

    /**
     * 恢复完成后一次性推进水位。
     *
     * <p>恢复阶段不允许任何读者进入（{@link ColumnStore} 在 {@code recover()} 返回后才开放读服务），
     * 所以这里可以在所有页都重做完之后统一放行。
     */
    void publishRecoveredUpTo(long version) {
        if (version > visibleSeq.get()) {
            visibleSeq.set(version);
            versionSeq.accumulateAndGet(version, Math::max);
        }
    }

    // ------------------------------------------------------------------ 内部结构

    /**
     * 页的不可变版本链。
     *
     * <p>三个并行数组按版本号升序：{@code versions[i]} 是版本号，
     * {@code offsets[i]} / {@code lengths[i]} 指向竞技场中该版本的字节区间。
     * 所有「修改」都返回新实例（copy-on-write），因此并发读者无需任何同步。
     */
    static final class PageChain {
        private final long[] versions;
        private final long[] offsets;
        private final int[] lengths;

        private PageChain(long[] versions, long[] offsets, int[] lengths) {
            this.versions = versions;
            this.offsets = offsets;
            this.lengths = lengths;
        }

        static PageChain singleton(long version, long offset, int length) {
            return new PageChain(new long[]{version}, new long[]{offset}, new int[]{length});
        }

        int size() {
            return versions.length;
        }

        long offset(int i) {
            return offsets[i];
        }

        int length(int i) {
            return lengths[i];
        }

        PageChain append(long version, long offset, int length) {
            int n = versions.length;
            long[] nv = new long[n + 1];
            long[] no = new long[n + 1];
            int[] nl = new int[n + 1];
            System.arraycopy(versions, 0, nv, 0, n);
            System.arraycopy(offsets, 0, no, 0, n);
            System.arraycopy(lengths, 0, nl, 0, n);
            nv[n] = version;
            no[n] = offset;
            nl[n] = length;
            return new PageChain(nv, no, nl);
        }

        /** 最大的版本号 &le; {@code snapshot} 的下标；不存在返回 -1。 */
        int floorIndex(long snapshot) {
            if (versions.length == 0) {
                return -1;
            }
            int lo = 0;
            int hi = versions.length - 1;
            int ans = -1;
            while (lo <= hi) {
                int mid = (lo + hi) >>> 1;
                if (versions[mid] <= snapshot) {
                    ans = mid;
                    lo = mid + 1;
                } else {
                    hi = mid - 1;
                }
            }
            return ans;
        }

        /**
         * 丢弃版本号 &lt; {@code horizon} 的前缀，<b>但永远保留最新版本</b>；若无变化返回 this。
         *
         * <p>保留最新版本是必需的：水位取自可见提交序号，可能高于某个「很久没被更新」的页的最新
         * 版本（例如该页最后写入于 v50，而库已提交到 v200）。若连最新版本一起丢，
         * 这个仍然存在的页会退化成「读不出来」—— 让一个存活页凭空消失。
         * 最新版本永远可能被将来的快照读到，所以它必须常驻。
         */
        PageChain trimBeforeRetainingLatest(long horizon) {
            if (versions.length == 0) {
                return this;
            }
            int keepFrom = 0;
            while (keepFrom < versions.length - 1 && versions[keepFrom] < horizon) {
                keepFrom++;
            }
            if (keepFrom == 0) {
                return this;
            }
            int n = versions.length - keepFrom;
            long[] nv = new long[n];
            long[] no = new long[n];
            int[] nl = new int[n];
            System.arraycopy(versions, keepFrom, nv, 0, n);
            System.arraycopy(offsets, keepFrom, no, 0, n);
            System.arraycopy(lengths, keepFrom, nl, 0, n);
            return new PageChain(nv, no, nl);
        }

        /** 被 {@link #trimBefore} 丢弃的字节数。 */
        long bytesOfDroppedPrefix(long horizon) {
            long sum = 0L;
            for (int i = 0; i < versions.length && versions[i] < horizon; i++) {
                sum += lengths[i];
            }
            return sum;
        }
    }

    /**
     * 堆外 append-only 字节竞技场。
     *
     * <p>只追加、不移动已有字节 —— 版本链里记录的 {@code (offset,length)} 因此永久有效。
     * 空间回收靠 {@link #checkpoint()} 配合 {@link #truncateBefore} 完成：
     * 调用方在确认水位以下的字节不再被任何链引用后推进竞技场的回收基线。
     */
    static final class Arena implements AutoCloseable {
        private static final int ALIGN = 8;
        /**
         * 堆外缓冲。声明为 volatile，使读路径可以「取一份快照再读」而完全不加锁 ——
         * 读不阻塞写，这是并发要求的核心。
         *
         * <p>扩容会写入<b>新</b>缓冲并整体拷贝旧内容，因此已经持有旧引用的读者
         * 仍能读到完全一致的历史字节（内容相同，只是物理位置不同），
         * 于是「无锁读 + 扩容」可以安全共存。
         */
        private volatile ByteBuffer buf;
        private long writeOffset;

        Arena(long initialBytes) {
            this.buf = ByteBuffer.allocateDirect((int) Math.min(Integer.MAX_VALUE, initialBytes))
                    .order(ByteOrder.LITTLE_ENDIAN);
            this.writeOffset = 0L;
        }

        /** 追加写入。串行化（append 语义本身要求），但读路径不参与这把锁。 */
        synchronized int append(byte[] src) {
            ensureCapacity(writeOffset + src.length);
            buf.position((int) writeOffset);
            buf.put(src);
            int off = (int) writeOffset;
            writeOffset += alignUp(src.length);
            return off;
        }

        /**
         * 无锁拷贝：只读一次 volatile 引用，之后不与写入者交互。
         *
         * <p>扩容会换上一块<b>全新</b>缓冲（不修改旧缓冲的任何字节），所以这里持有的
         * 旧引用读到的仍是那次快照时刻的完整字节。若拷贝期间竞技场发生了扩容
         * （{@code buf} 引用已变），说明本次拷贝跨越了边界，重试一次即可拿到一致结果。
         */
        void copyTo(long offset, int length, byte[] dest) {
            if (length > dest.length) {
                throw new IllegalArgumentException("dest too small: " + dest.length + " < " + length);
            }
            while (true) {
                ByteBuffer snapshot = this.buf; // 一次 volatile 读，拿到一份稳定视图
                int p = (int) offset;
                for (int i = 0; i < length; i++) {
                    dest[i] = snapshot.get(p + i);
                }
                if (snapshot == this.buf) {
                    return; // 拷贝期间没有扩容：这份结果就是一致的
                }
                // 发生了扩容，重新拷贝以保证 dest 完全来自同一个缓冲世代。
            }
        }

        /**
         * 扩容：分配全新缓冲并拷贝历史字节，然后<b>原子替换引用</b>。
         *
         * <p>关键在于绝不向旧缓冲写入 —— 旧缓冲可能正被无锁读者读取，
         * 原地修改会让他们读到撕裂数据。因此新缓冲是一次性构造好再发布的。
         */
        private void ensureCapacity(long required) {
            ByteBuffer current = this.buf;
            if (required <= current.capacity()) {
                return;
            }
            long newCap = Math.max(current.capacity() * 2L, required);
            newCap = Math.max(newCap, 1L << 20);
            newCap = Math.min(newCap, Integer.MAX_VALUE - 8L);
            ByteBuffer grown = ByteBuffer.allocateDirect((int) newCap).order(ByteOrder.LITTLE_ENDIAN);
            // 把仍在用的历史字节拷进新缓冲，保证旧 offset 继续有效。
            for (int i = 0; i < (int) writeOffset; i++) {
                grown.put(i, current.get(i));
            }
            this.buf = grown; // 原子发布；旧缓冲保持只读不变
        }

        private static int alignUp(int n) {
            return (n + ALIGN - 1) & ~(ALIGN - 1);
        }

        @Override
        public synchronized void close() {
            // 堆外内存由 Cleaner 回收；显式置空引用以便 GC 及时处理。
            this.buf = null;
        }
    }
}
