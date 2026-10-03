package io.corp.artifactres.core;

import java.io.PrintStream;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Deque;
import java.util.List;

/**
 * Optional decision trace for a resolve run.
 *
 * <p>Disabled instances are free: every record is guarded by {@link #enabled} and the
 * collector is never allocated, so leaving trace off costs the solver nothing.
 *
 * <p>Each record captures the propagation context that caused the decision, the version
 * chosen (or the reason none was available), and the constraint chain that led here:
 * who triggered the visit, which constraint narrowed the window, and whether the choice
 * forced a downgrade upstream.
 */
public final class Trace {

    /** Why a package settled on a version. */
    public enum Kind {
        /** Greedy pick succeeded on the first attempt. */
        GREEDY,
        /** Greedy pick failed; iterative deepening forced a downgrade. */
        DOWNGRADE,
        /** No candidate satisfied the intersection; the run is going to fail. */
        NO_CANDIDATE,
        /** Version came from a replayed lock file rather than from solving. */
        LOCKED
    }

    /** One immutable trace record. */
    public static final class Entry {
        /** Monotonic visit counter, i.e. propagation order. */
        public final int step;
        public final String pkg;
        public final Kind kind;
        /** Version selected, or {@code null} when {@link Kind#NO_CANDIDATE}. */
        public final Version selected;
        /** The intersected window the selection had to satisfy. */
        public final Interval window;
        /** Number of versions available for the package in the local metadata. */
        public final int available;
        /** 1-based depth of the last backtrack iteration (K) that produced this decision. */
        public final int depth;
        /** Package that triggered this visit, or {@code null} for a root requirement. */
        public final String triggeredBy;
        /** Provenance of the constraint that bound this package's window. */
        public final String constraintFrom;
        /** Human-readable explanation, always populated. */
        public final String reason;
        /** The chain of packages that led here, root first. */
        public final List<String> chain;

        Entry(int step, String pkg, Kind kind, Version selected, Interval window, int available,
              int depth, String triggeredBy, String constraintFrom, String reason, List<String> chain) {
            this.step = step;
            this.pkg = pkg;
            this.kind = kind;
            this.selected = selected;
            this.window = window;
            this.available = available;
            this.depth = depth;
            this.triggeredBy = triggeredBy;
            this.constraintFrom = constraintFrom;
            this.reason = reason;
            this.chain = List.copyOf(chain);
        }

        @Override
        public String toString() {
            StringBuilder sb = new StringBuilder(96);
            sb.append(String.format("#%04d ", step)).append(pkg).append(" -> ");
            if (selected != null) {
                sb.append(selected);
            } else {
                sb.append("<none>");
            }
            sb.append("  ").append(kind).append("(K=").append(depth).append(')');
            sb.append("  window=").append(window);
            sb.append("  avail=").append(available);
            sb.append("  by=").append(triggeredBy == null ? "<root>" : triggeredBy);
            if (constraintFrom != null) {
                sb.append("  from=").append(constraintFrom);
            }
            sb.append("  why=").append(reason);
            if (chain.size() > 1) {
                sb.append("  chain=").append(String.join(" -> ", chain));
            }
            return sb.toString();
        }
    }

    private final boolean enabled;
    private final List<Entry> entries = new ArrayList<>();
    /** Live root-to-current package chain while the resolver descends. */
    private final Deque<String> chain = new ArrayDeque<>();

    private Trace(boolean enabled) {
        this.enabled = enabled;
    }

    /** @return a tracing instance. */
    public static Trace enabled() {
        return new Trace(true);
    }

    /** @return a no-op instance; all record calls short-circuit. */
    public static Trace disabled() {
        return new Trace(false);
    }

    public boolean isEnabled() {
        return enabled;
    }

    /** Push a package onto the live decision chain as the resolver descends. */
    public void push(String pkg) {
        if (enabled) {
            chain.addLast(pkg);
        }
    }

    public void pop() {
        if (enabled) {
            chain.pollLast();
        }
    }

    /** Record a decision. Ignored when tracing is disabled. */
    public void record(String pkg, Kind kind, Version selected, Interval window, int available,
                       int depth, String triggeredBy, String constraintFrom, String reason) {
        if (!enabled) {
            return;
        }
        entries.add(new Entry(entries.size() + 1, pkg, kind, selected, window, available, depth,
                triggeredBy, constraintFrom, reason, new ArrayList<>(chain)));
    }

    /** @return the recorded decisions in propagation order. */
    public List<Entry> entries() {
        return List.copyOf(entries);
    }

    /** @return decisions touching {@code pkg}, in propagation order. */
    public List<Entry> entriesFor(String pkg) {
        List<Entry> out = new ArrayList<>();
        for (Entry e : entries) {
            if (e.pkg.equals(pkg)) {
                out.add(e);
            }
        }
        return out;
    }

    public int size() {
        return entries.size();
    }

    /** Write every decision to {@code out} in propagation order. */
    public void dump(PrintStream out) {
        if (!enabled) {
            return;
        }
        out.println("== resolve trace (" + entries.size() + " decisions) ==");
        for (Entry e : entries) {
            out.println(e);
        }
    }

    /** @return the trace as newline-separated text, empty when disabled. */
    public String render() {
        if (!enabled || entries.isEmpty()) {
            return "";
        }
        StringBuilder sb = new StringBuilder(entries.size() * 96);
        for (Entry e : entries) {
            sb.append(e).append('\n');
        }
        return sb.toString();
    }
}
