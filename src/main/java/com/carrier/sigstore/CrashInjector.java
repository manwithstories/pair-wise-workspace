package com.carrier.sigstore;

import java.util.ArrayList;
import java.util.EnumSet;
import java.util.List;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * 崩溃点注入器：在刷盘流程的<b>前 / 中 / 后</b>三个位置模拟进程猝死。
 *
 * <p>与"真的 kill -9"不同，这里用 {@link CrashSimulatedException} 中断当前线程，
 * 保证 JVM 还能继续把数据库<b>重新打开</b>并跑 {@link RecoveryEngine}——这正是我们要断言的：
 * 磁盘上的字节状态与真崩溃一致，而恢复逻辑照样能跑通。
 *
 * <p>三个注入位：
 * <ul>
 *   <li>{@link Stage#BEFORE_SYNC}——数据和日志都已写进 OS 页缓存但尚未 fsync。
 *       模拟"掉电后 OS 缓冲区丢失"，恢复必须靠已落盘的 COMMIT 划界。</li>
 *   <li>{@link Stage#MID_SYNC}——fsync 进行中抛错，模拟部分页已落盘、部分没有，
 *       且落盘的那几页可能是<b>半截</b>的。</li>
 *   <li>{@link Stage#AFTER_SYNC}——fsync 完成但页缓存尚未合并/快照未推进，
 *       模拟"数据已安全但事务收尾被打断"。</li>
 * </ul>
 *
 * <p>触发后 {@link #fired()} 只允许放行一次，之后注入器自动失效，方便测试在同一进程内做恢复。
 */
public final class CrashInjector {

    public enum Stage {
        BEFORE_SYNC, MID_SYNC, AFTER_SYNC
    }

    /** 抛出它来代表"进程猝死"。继承 RuntimeException 以便穿过 lambda。 */
    public static final class CrashSimulatedException extends RuntimeException {
        private final Stage stage;

        public CrashSimulatedException(Stage stage) {
            super("在 " + stage + " 处注入崩溃");
            this.stage = stage;
        }

        public Stage stage() {
            return stage;
        }
    }

    /** 空集合 = 永不触发。默认构造的注入器必须是惰性的。 */
    private final EnumSet<Stage> stages;
    private final AtomicInteger fireCount = new AtomicInteger(0);
    private final AtomicInteger armedCount = new AtomicInteger(0);
    private volatile boolean armed = true;
    private volatile boolean disabled;

    /**
     * 在指定阶段崩溃。
     *
     * <p><b>不传任何 stage 时构造出的注入器永不触发</b>——这是有意的：
     * {@code new CrashInjector()} 是生产路径的默认参数，它必须是惰性的，
     * 否则线上随时可能凭空崩一次。
     */
    public CrashInjector(Stage... stages) {
        this.stages = stages.length == 0
                ? EnumSet.noneOf(Stage.class)
                : EnumSet.copyOf(List.of(stages));
    }

    public static CrashInjector beforeSync() {
        return new CrashInjector(Stage.BEFORE_SYNC);
    }

    public static CrashInjector midSync() {
        return new CrashInjector(Stage.MID_SYNC);
    }

    public static CrashInjector afterSync() {
        return new CrashInjector(Stage.AFTER_SYNC);
    }

    /** 三个点全打，用于遍历所有注入组合。 */
    public static CrashInjector allStages() {
        return new CrashInjector(Stage.BEFORE_SYNC, Stage.MID_SYNC, Stage.AFTER_SYNC);
    }

    public boolean armed(Stage stage) {
        return armed && !disabled && stages.contains(stage);
    }

    public boolean isArmed() {
        return armed && !disabled && !stages.isEmpty();
    }

    /** 命中则抛异常；放行一次后自动解除武装。 */
    public void maybeCrash(Stage stage) {
        if (!armed(stage)) return;
        armed = false;
        fireCount.incrementAndGet();
        throw new CrashSimulatedException(stage);
    }

    /** 永久关闭注入，用于恢复阶段的防误触。 */
    public void disable() {
        disabled = true;
    }

    /** 重新武装，可注入下一个崩溃点。 */
    public void rearm() {
        armed = true;
        armedCount.incrementAndGet();
    }

    public int firedCount() {
        return fireCount.get();
    }

    public boolean hasFired() {
        return fireCount.get() > 0;
    }

    public List<Stage> stages() {
        return new ArrayList<>(stages);
    }

    @Override
    public String toString() {
        return "CrashInjector" + stages + (armed ? "(armed)" : "(spent)");
    }
}
