package com.corp.depsolver;

import java.util.ArrayList;
import java.util.List;

/**
 * Optional decision trace. Disabled by default so the hot path allocates nothing; when enabled it
 * records, for every package decision, who triggered the requirement, which version was chosen, and why
 * a lower version was picked (a downgrade) instead of the highest satisfying one.
 *
 * <p>All writes are guarded by {@link #enabled} and by {@code null} checks, so calling any method while
 * disabled is a cheap no-op rather than an error.
 */
public final class Trace {

    /** One recorded step. {@code kind} is one of {@code decide}, {@code downgrade}, {@code conflict}. */
    public static final class Entry {
        private final int seq;
        private final String kind;
        private final String pkg;
        private final String triggeredBy;
        private final String chosen;
        private final String reason;
        private final int backtrackDepth;

        Entry(int seq, String kind, String pkg, String triggeredBy, String chosen, String reason,
              int backtrackDepth) {
            this.seq = seq;
            this.kind = kind;
            this.pkg = pkg;
            this.triggeredBy = triggeredBy;
            this.chosen = chosen;
            this.reason = reason;
            this.backtrackDepth = backtrackDepth;
        }

        public int seq() {
            return seq;
        }

        public String kind() {
            return kind;
        }

        public String pkg() {
            return pkg;
        }

        public String triggeredBy() {
            return triggeredBy;
        }

        public String chosen() {
            return chosen;
        }

        public String reason() {
            return reason;
        }

        public int backtrackDepth() {
            return backtrackDepth;
        }

        @Override
        public String toString() {
            StringBuilder sb = new StringBuilder();
            sb.append(String.format("%04d", seq)).append(' ').append(kind).append(' ').append(pkg);
            if (triggeredBy != null) {
                sb.append("  <- ").append(triggeredBy);
            }
            if (chosen != null) {
                sb.append("  => ").append(chosen);
            }
            if (reason != null) {
                sb.append("  (").append(reason).append(')');
            }
            if (backtrackDepth > 0) {
                sb.append("  [k=").append(backtrackDepth).append(']');
            }
            return sb.toString();
        }
    }

    private final boolean enabled;
    private final List<Entry> entries = new ArrayList<>();
    private final StringBuilder conflicts = new StringBuilder();
    private int seq;

    private Trace(boolean enabled) {
        this.enabled = enabled;
    }

    public static Trace disabled() {
        return new Trace(false);
    }

    public static Trace enabled() {
        return new Trace(true);
    }

    public boolean isEnabled() {
        return enabled;
    }

    /** Record a normal decision: this requirement was raised by {@code triggeredBy}. */
    public void decide(String pkg, String triggeredBy, String chosen, String reason) {
        if (!enabled) {
            return;
        }
        entries.add(new Entry(seq++, "decide", pkg, triggeredBy, chosen, reason, 0));
    }

    /** Record a downgrade: the highest candidate was unusable, a lower one was taken instead. */
    public void downgrade(String pkg, String triggeredBy, String chosen, String reason, int backtrackDepth) {
        if (!enabled) {
            return;
        }
        entries.add(new Entry(seq++, "downgrade", pkg, triggeredBy, chosen, reason, backtrackDepth));
    }

    /** Record a conflict and its causal chain. */
    public void conflict(String pkg, String chain) {
        if (!enabled) {
            return;
        }
        entries.add(new Entry(seq++, "conflict", pkg, null, null, chain, 0));
        if (conflicts.length() > 0) {
            conflicts.append('\n');
        }
        conflicts.append(pkg).append(": ").append(chain);
    }

    public List<Entry> entries() {
        return List.copyOf(entries);
    }

    public boolean isEmpty() {
        return entries.isEmpty();
    }

    /** The ordered decision log, one step per line, ready to print to the terminal. */
    public String render() {
        StringBuilder sb = new StringBuilder();
        for (Entry e : entries) {
            sb.append(e).append('\n');
        }
        return sb.toString();
    }

    /** Just the conflict sections, empty when the resolve succeeded. */
    public String conflictReport() {
        return conflicts.toString();
    }

    public void clear() {
        entries.clear();
        conflicts.setLength(0);
        seq = 0;
    }
}
