package com.carrier.sigcs;

import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;

/**
 * 崩溃点注入器：在刷盘流水线的任意位置抛出 {@link CrashException}，模拟进程猝死。
 *
 * <h2>为什么不直接用真崩溃</h2>
 * <p>真崩溃需要 fork 子进程，测试慢且难定位。真实故障的 <em>本质</em> 是
 * 「某一步的副作用只做了一半，且进程立刻消失，没有机会做清理」。
 * 因此这里用异常在指定步骤「半路抛出」来精确保留这个状态：
 * 步骤之前的效果保留，之后的效果完全不发生。
 *
 * <h2>注入点</h2>
 * <p>刷盘一次要经过三个物理步骤，每步之间都可以断：
 * <ol>
 *   <li>{@link Step#WAL_APPEND} —— 已写入页缓存，尚未 fsync WAL；</li>
 *   <li>{@link Step#WAL_FSYNC_DONE} —— WAL 已落盘，页缓存还未改；</li>
 *   <li>{@link Step#PAGE_DIRTY} —— 页缓存已改，尚未回写数据文件；</li>
 *   <li>{@link Step#PAGE_FSYNC_DONE} —— 数据页已落盘。</li>
 * </ol>
 * 另外 {@link Step#PRE_COMMIT} 用来卡在提交点之前，验证「未提交事务绝不出现在 WAL 可见区」。
 */
final class CrashInjector {

    /** 崩溃模拟信号。抛出它就等价于进程此刻消失。 */
    static final class CrashException extends RuntimeException {
        private static final long serialVersionUID = 1L;
        final Step at;

        CrashException(Step at) {
            super("injected crash at " + at, null, false, false);
            this.at = at;
        }
    }

    /** 可注入崩溃的刷盘步骤。 */
    enum Step {
        /** WAL 记录写进 OS 页缓存、fsync 之前。 */
        WAL_APPEND,
        /** WAL fsync 完成之后、页缓存改动之前。 */
        WAL_FSYNC_DONE,
        /** 页缓存已改、数据文件尚未回写。 */
        PAGE_DIRTY,
        /** 数据页 fsync 完成。 */
        PAGE_FSYNC_DONE,
        /** 提交点之前：版本号已分配但尚未提交。 */
        PRE_COMMIT
    }

    /** 不注入时的空实现。 */
    static final CrashInjector DISABLED = new CrashInjector();

    /** 命中次数计数（诊断用）。 */
    private final AtomicLong trips = new AtomicLong();

    /** 当前将在命中第 n 次调用时崩溃的步骤；null 表示不在该步骤崩溃。 */
    private final AtomicReference<Rule> armed = new AtomicReference<>();

    private record Rule(Step step, long skipFirst, boolean oneShot) {
    }

    /** 默认实例不注入任何崩溃。 */
    CrashInjector() {
    }

    boolean enabled() {
        return armed.get() != null;
    }

    /**
     * 在 {@code step} 之后的第 {@code skipFirst} 次命中时崩溃一次。
     *
     * @param step      目标步骤
     * @param skipFirst 先放过多少次命中（0 = 第一次命中就崩）
     * @param oneShot   true = 崩一次后自动解除武装（重启场景）
     */
    CrashInjector arm(Step step, long skipFirst, boolean oneShot) {
        armed.set(new Rule(step, skipFirst, oneShot));
        return this;
    }

    /** 解除武装。 */
    void disarm() {
        armed.set(null);
        trips.set(0L);
    }

    /** 实际已触发的崩溃次数。 */
    long tripCount() {
        return trips.get();
    }

    /**
     * 流水线经过某步骤时调用。若命中规则则抛出 {@link CrashException}。
     */
    void checkpoint(Step step) {
        while (true) {
            Rule rule = armed.get();
            if (rule == null || rule.step() != step) {
                return;
            }
            long seen = perStepSeen(step).incrementAndGet();
            if (seen <= rule.skipFirst()) {
                return; // 还没到注入点，正常放行
            }
            if (rule.oneShot()) {
                // CAS 解除武装后再抛，保证「一次崩溃后引擎可继续运行」
                if (armed.compareAndSet(rule, null)) {
                    trips.incrementAndGet();
                    throw new CrashException(step);
                }
            } else {
                trips.incrementAndGet();
                throw new CrashException(step);
            }
        }
    }

    /** 每步骤的独立计数，避免 skipFirst 在不同步骤间串扰。 */
    private final AtomicReference<java.util.EnumMap<Step, AtomicLong>> counters = new AtomicReference<>(newCounters());

    private static java.util.EnumMap<Step, AtomicLong> newCounters() {
        var m = new java.util.EnumMap<Step, AtomicLong>(Step.class);
        for (Step s : Step.values()) {
            m.put(s, new AtomicLong());
        }
        return m;
    }

    private AtomicLong perStepSeen(Step step) {
        // 只增不减；用 CAS 保证并发下不会重复建表。
        java.util.EnumMap<Step, AtomicLong> current = counters.get();
        AtomicLong c = current.get(step);
        if (c != null) {
            return c;
        }
        java.util.EnumMap<Step, AtomicLong> fresh = newCounters();
        fresh.putAll(current);
        fresh.put(step, new AtomicLong());
        if (counters.compareAndSet(current, fresh)) {
            return fresh.get(step);
        }
        return counters.get().get(step);
    }
}
