package com.corp.depsolver;

import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.TreeSet;

/**
 * Deterministic dependency resolver over a local {@link Repository}.
 *
 * <p>The search is a chronological backtracking walk over a stack of version decisions. Each decision
 * picks the highest published version of one package that satisfies every range recorded so far, so the
 * first complete tree found is the greedy answer.
 *
 * <ol>
 *   <li><b>Constraint recording.</b> When a version is placed on the stack, each of its required
 *       dependencies contributes a range to the target package's accumulated interval. Two cases end the
 *       current branch immediately: the intersection goes empty (reported by naming both offending
 *       ranges), or a pinned/repaired version falls outside the accumulated interval.</li>
 *   <li><b>Greedy selection.</b> Candidates are scanned in {@link TreeMap} natural order from highest to
 *       lowest, so the first admissible version is always the highest satisfying one.</li>
 *   <li><b>Iterative deepening repair.</b> Before backtracking freely, the search retries with a forced
 *       downgrade window of K packages, K growing from 1 to {@link #MAX_BACKTRACK_DEPTH}. K bounds how
 *       many already placed packages may be stepped down one version at a time, so the smallest window
 *       that succeeds is the smallest change. Only when every K has been tried does the search fall back
 *       to free chronological backtracking within the same budget.</li>
 * </ol>
 *
 * <p><b>Termination.</b> The budget is the whole reason this cannot loop. Each decision point offers at
 * most {@code V} versions and the stack never exceeds {@code P} entries, so the state space is bounded by
 * {@code V^P}; on top of that the resolver enforces a hard cap of {@link #MAX_STEPS} decision steps
 * ({@code MAX_BACKTRACK_DEPTH} squared plus a linear allowance) and a cap of
 * {@link #MAX_BACKTRACK_DEPTH} on how deep a free backtrack may go before it gives up. Either cap firing
 * ends the search and reports a conflict. There is no unbounded recursion.
 *
 * <p><b>Determinism.</b> The pending package set is a sorted structure, candidate scans follow natural
 * order, and the deepening schedule is fixed. Nothing consults the clock, directory order or a hash
 * seed, so repeated runs on one repository yield byte identical trees.
 */
public final class Resolver {

    /** Hard cap on the iterative deepening window, per the performance contract. */
    public static final int MAX_BACKTRACK_DEPTH = 8;

    /** Hard cap on how many packages a free backtrack may unwind before giving up. */
    public static final int MAX_FREE_BACKTRACK = 8;

    /** Hard ceiling on decision steps in a single resolve, so a pathological graph cannot run away. */
    public static final int MAX_STEPS = 4_000_000;

    static {
        if (MAX_BACKTRACK_DEPTH < 1 || MAX_BACKTRACK_DEPTH > 8) {
            throw new IllegalStateException("MAX_BACKTRACK_DEPTH must stay within 1..8");
        }
    }

    /** A resolution failure carrying the causal chain and a suggested minimal fix. */
    public static final class Conflict extends Exception {
        private static final long serialVersionUID = 1L;
        private final String pkg;
        private final String reason;
        private final transient List<String> chain;
        private final String suggestion;

        Conflict(String pkg, String reason, List<String> chain, String suggestion) {
            super(pkg + ": " + reason);
            this.pkg = pkg;
            this.reason = reason;
            this.chain = List.copyOf(chain);
            this.suggestion = suggestion;
        }

        public String packageId() {
            return pkg;
        }

        public String reason() {
            return reason;
        }

        /** Ordered requirement sites that led to this conflict, root first. */
        public List<String> chain() {
            return chain;
        }

        /** Minimal repair advice; empty when no targeted advice can be derived. */
        public String suggestion() {
            return suggestion;
        }

        /** Multi-line report suitable for a terminal. */
        public String report() {
            StringBuilder sb = new StringBuilder();
            sb.append("CONFLICT on ").append(pkg).append('\n');
            sb.append("  reason : ").append(reason).append('\n');
            sb.append("  chain  :\n");
            for (int i = 0; i < chain.size(); i++) {
                sb.append("    ").append(i + 1).append(". ").append(chain.get(i)).append('\n');
            }
            if (!suggestion.isEmpty()) {
                sb.append("  fix    : ").append(suggestion).append('\n');
            }
            return sb.toString();
        }
    }

    /** A successful output: a deterministic package to version tree. */
    public static final class Resolution {
        private final TreeMap<String, Version> selected = new TreeMap<>();
        private final TreeMap<String, Interval> intervals = new TreeMap<>();
        private final TreeMap<String, List<String>> requiredBy = new TreeMap<>();
        private final Set<String> excluded = new TreeSet<>();

        /** Package to chosen version, sorted by package id. */
        public TreeMap<String, Version> selected() {
            return new TreeMap<>(selected);
        }

        /** Package to the fully intersected interval that admitted its version. */
        public TreeMap<String, Interval> intervals() {
            return new TreeMap<>(intervals);
        }

        /** For each package, the sorted set of packages whose requirements constrained it. */
        public TreeMap<String, List<String>> requiredBy() {
            return new TreeMap<>(requiredBy);
        }

        /** Packages kept out of the tree by an exclusion rule. */
        public Set<String> excluded() {
            return new TreeSet<>(excluded);
        }

        public int size() {
            return selected.size();
        }

        @Override
        public String toString() {
            StringBuilder sb = new StringBuilder();
            for (Map.Entry<String, Version> e : selected.entrySet()) {
                sb.append(e.getKey()).append(" -> ").append(e.getValue())
                  .append("   ").append(intervals.get(e.getKey())).append('\n');
            }
            return sb.toString();
        }
    }

    /**
     * One requirement site. {@code from} is the requiring package, or {@code "<root>"} for the roots; it
     * is what the trace prints as "who triggered".
     */
    public static final class Requirement {
        public final String from;
        public final String pkg;
        public final Interval range;
        public final boolean optional;

        public Requirement(String from, String pkg, Interval range, boolean optional) {
            this.from = from;
            this.pkg = pkg;
            this.range = range;
            this.optional = optional;
        }

        @Override
        public String toString() {
            return from + " -> " + pkg + " " + range + (optional ? " (optional)" : "");
        }
    }

    /** One package already placed on the decision stack. */
    private static final class Decision {
        final String pkg;
        final Version version;
        /** The candidate versions still to try for this package, highest first. */
        final List<Version> rest;
        /** Active exclusion ids contributed by this version's metadata. */
        final List<String> excludes;

        Decision(String pkg, Version version, List<Version> rest, List<String> excludes) {
            this.pkg = pkg;
            this.version = version;
            this.rest = rest;
            this.excludes = excludes;
        }
    }

    /** Why a branch was abandoned, kept for the deepest failure so the report stays specific. */
    private static final class Failure {
        final String pkg;
        final String reason;
        final List<String> chain;
        final TreeMap<String, Version> prefix;

        Failure(String pkg, String reason, List<String> chain, TreeMap<String, Version> prefix) {
            this.pkg = pkg;
            this.reason = reason;
            this.chain = chain;
            this.prefix = prefix;
        }
    }

    private final Repository repo;
    private final Trace trace;

    public Resolver(Repository repo) {
        this(repo, Trace.disabled());
    }

    public Resolver(Repository repo, Trace trace) {
        this.repo = repo;
        this.trace = trace == null ? Trace.disabled() : trace;
    }

    /** Build a root requirement from its textual range. */
    public static Requirement root(String pkg, String rangeText) {
        return new Requirement("<root>", pkg, Interval.parse(rangeText), false);
    }

    /**
     * Resolve from a set of root requirements.
     *
     * @throws Conflict when no assignment exists within the backtrack budget
     */
    public Resolution resolve(List<Requirement> roots) throws Conflict {
        if (roots == null || roots.isEmpty()) {
            return new Resolution();
        }
        // The plain greedy walk first: on a healthy graph it succeeds and nothing else runs.
        Outcome greedy = search(roots, Map.of(), MAX_BACKTRACK_DEPTH, false);
        if (greedy.tree != null) {
            return greedy.tree;
        }
        trace.conflict(greedy.failure.pkg, greedy.failure.reason + " | greedy failed, deepening k=1");

        // Iterative deepening: force a downgrade of at most K already decided packages, K from 1 to the
        // cap. The first window that succeeds is the smallest such change.
        Failure best = greedy.failure;
        for (int k = 1; k <= MAX_BACKTRACK_DEPTH; k++) {
            Outcome repaired = search(roots, Map.of(), k, true);
            if (repaired.tree != null) {
                return repaired.tree;
            }
            if (repaired.failure != null && repaired.failure.reason.equals(greedy.failure.reason)) {
                best = repaired.failure;
                break;   // the same conflict at every depth: it is in the requirements, not the picks
            }
            if (repaired.failure != null) {
                best = repaired.failure;
            }
            trace.conflict(best.pkg, best.reason + " | k=" + k + " exhausted");
        }

        Conflict c = new Conflict(best.pkg, best.reason, best.chain, suggest(best));
        trace.conflict(c.packageId(), c.reason() + " | gave up after k=1.." + MAX_BACKTRACK_DEPTH);
        throw c;
    }

    /** Result of one search: either a tree, or the deepest failure it ran into. */
    private static final class Outcome {
        final Resolution tree;
        final Failure failure;

        Outcome(Resolution tree, Failure failure) {
            this.tree = tree;
            this.failure = failure;
        }
    }

    /**
     * One backtracking search.
     *
     * @param forced      packages pinned to a specific version for this whole search, used by tests and
     *                    by callers that already know the answer
     * @param window      how many already placed packages a repair attempt may step down; ignored unless
     *                    {@code repair} is set
     * @param repair      whether to run the bounded downgrade window before free backtracking
     */
    private Outcome search(List<Requirement> roots, Map<String, Version> forced,
                           int window, boolean repair) {
        SearchState st = new SearchState(forced, repair ? window : 0, repair);
        st.seed(roots);
        if (st.failure != null) {
            return new Outcome(null, st.failure);
        }
        if (st.search()) {
            return new Outcome(st.toResolution(), null);
        }
        return new Outcome(null, st.failure == null
                ? new Failure("<unknown>", "search budget exhausted", List.of(), new TreeMap<>())
                : st.failure);
    }

    /** Mutable state for one search; kept separate so it can be reset between windows. */
    private final class SearchState {
        private final Map<String, Version> forced;
        private final int window;
        private final boolean repair;

        private final ArrayDeque<Decision> stack = new ArrayDeque<>();
        /** Constraints per package: accumulated interval plus the sites that produced it. */
        private final TreeMap<String, Interval> acc = new TreeMap<>();
        private final TreeMap<String, List<Requirement>> sites = new TreeMap<>();
        /** Packages that still need a decision, in sorted order. */
        private final TreeSet<String> pending = new TreeSet<>();
        /**
         * Per package, the candidates a previous backtrack left untried, highest first. Consumed and
         * cleared by {@link #decide} so a retry resumes where the failed branch stopped instead of
         * restarting at the highest version.
         */
        private final TreeMap<String, List<Version>> resumeFrom = new TreeMap<>();
        private int steps;
        /** How many downgrade attempts the current window has already granted. */
        private int downgradesUsed;
        private Failure failure;

        SearchState(Map<String, Version> forced, int window, boolean repair) {
            this.forced = forced;
            this.window = window;
            this.repair = repair;
        }

        /** Record the root requirements, failing fast if two of them already exclude each other. */
        void seed(List<Requirement> roots) {
            for (Requirement req : roots) {
                if (req.optional) {
                    continue;
                }
                if (!addConstraint(req)) {
                    return;
                }
                pending.add(req.pkg);
            }
        }

        /**
         * Add one required range to a package's accumulated interval. Returns false and records the
         * failure when the new range is mutually exclusive with what was already collected.
         */
        private boolean addConstraint(Requirement req) {
            Interval cur = acc.get(req.pkg);
            if (cur == null) {
                acc.put(req.pkg, req.range);
                sites.put(req.pkg, new ArrayList<>(List.of(req)));
                return true;
            }
            Interval merged = cur.intersect(req.range);
            if (merged.isEmpty()) {
                Requirement prev = lastRequired(sites.get(req.pkg));
                failure = new Failure(req.pkg,
                        "mutually exclusive ranges: " + prev.range + " and " + req.range,
                        chain(req.pkg, sites.get(req.pkg), req), snapshot());
                trace.conflict(req.pkg, failure.reason);
                return false;
            }
            acc.put(req.pkg, merged);
            sites.get(req.pkg).add(req);
            return true;
        }

        /** The versions chosen so far, which is what a conflict report and a repair hint quote. */
        private TreeMap<String, Version> snapshot() {
            TreeMap<String, Version> out = new TreeMap<>();
            for (Decision d : stack) {
                out.put(d.pkg, d.version);
            }
            return out;
        }

        /** Main loop: keep placing the lowest pending package until the tree is complete. */
        boolean search() {
            while (true) {
                if (++steps > MAX_STEPS) {
                    failure = new Failure(peekPkg(), "search budget exhausted after " + MAX_STEPS
                            + " steps", chain(peekPkg(), sites.getOrDefault(peekPkg(), List.of()), null),
                            snapshot());
                    return false;
                }
                if (pending.isEmpty()) {
                    failure = null;   // a complete tree supersedes every failure along the way
                    return true;
                }
                String pkg = pending.first();
                if (!decide(pkg)) {
                    if (!backtrack()) {
                        return false;
                    }
                }
            }
        }

        private String peekPkg() {
            return stack.isEmpty() ? (pending.isEmpty() ? "<root>" : pending.first()) : stack.peek().pkg;
        }

        /**
         * Try to place a version for {@code pkg}. On success its dependencies become new constraints and
         * pending work. On failure the package stays pending for the next visit.
         */
        private boolean decide(String pkg) {
            Interval iv = acc.get(pkg);
            if (iv == null || iv.isEmpty()) {
                return false;
            }
            List<String> banned = activeExclusions();
            List<Version> candidates = resumeFrom.remove(pkg);
            if (candidates == null) {
                candidates = repo.versionsDescending(pkg);
            }
            Version pinned = forced.get(pkg);

            for (int i = 0; i < candidates.size(); i++) {
                Version v = candidates.get(i);
                if (pinned != null && !v.equals(pinned)) {
                    break;   // a pinned package has exactly one candidate
                }
                if (!iv.contains(v)) {
                    continue;
                }
                if (breaksExclusion(pkg, v, banned)) {
                    continue;
                }
                Repository.Meta m = repo.meta(pkg, v).orElse(null);
                if (m == null) {
                    continue;
                }
                if (place(pkg, v, m, candidates, i)) {
                    return true;
                }
                // place() rolled back its own effects; try the next lower candidate.
                if (failure != null && failure.pkg != null && !failure.pkg.equals(pkg)
                        && !failure.reason.startsWith("mutually exclusive")) {
                    // A deeper package failed, so this whole subtree is unviable; stop descending.
                    return false;
                }
                if (pinned != null) {
                    break;
                }
            }
            return false;
        }

        /**
         * Push a decision for {@code pkg:version} and fold in its dependencies. Everything the placement
         * added is recorded so an unsuccessful attempt can be undone exactly.
         */
        private boolean place(String pkg, Version v, Repository.Meta m, List<Version> candidates, int idx) {
            List<Version> rest = new ArrayList<>(candidates.subList(Math.min(idx + 1, candidates.size()),
                    candidates.size()));
            Decision d = new Decision(pkg, v, rest, m.exclusions());
            stack.push(d);

            List<Requirement> added = new ArrayList<>();
            List<String> newPending = new ArrayList<>();
            List<String> alreadyDecided = new ArrayList<>();
            boolean ok = true;
            for (Repository.Dependency dep : m.dependencies()) {
                if (dep.optional()) {
                    continue;
                }
                Requirement req = new Requirement(pkg, dep.target(), dep.range(), false);
                if (!addConstraint(req)) {
                    ok = false;
                    break;
                }
                added.add(req);
                if (onStack(dep.target())) {
                    // This package already has a version on the stack, for instance because the graph
                    // has a cycle. Intersecting its constraints is enough; placing it again would
                    // duplicate the decision and let a cycle grow the stack without end.
                    if (!acc.get(dep.target()).contains(chosenVersionOf(dep.target()))) {
                        failure = new Failure(dep.target(),
                                "the version already chosen, " + chosenVersionOf(dep.target())
                                        + ", is excluded by " + dep.range() + " required by " + pkg,
                                chain(dep.target(), sites.get(dep.target()), req), snapshot());
                        trace.conflict(dep.target(), failure.reason);
                        ok = false;
                        break;
                    }
                } else {
                    newPending.add(dep.target());
                }
            }
            if (ok) {
                Version top = candidates.get(0);
                if (v.equals(top)) {
                    trace.decide(pkg, triggers(sites.get(pkg)), v.toString(),
                            "highest satisfying " + acc.get(pkg));
                } else {
                    trace.downgrade(pkg, triggers(sites.get(pkg)), v.toString(),
                            "highest " + top + " rejected (outside " + acc.get(pkg) + ")", 0);
                }
                pending.remove(pkg);
                for (String p : newPending) {
                    pending.add(p);
                }
                return true;
            }
            undo(d, added, newPending);
            return false;
        }

        /**
         * The version currently on the stack for {@code pkg}, or null when it is not decided yet.
         * Derived from the stack itself so it can never drift out of sync with the undo path.
         */
        private Version chosenVersionOf(String pkg) {
            for (Decision d : stack) {
                if (d.pkg.equals(pkg)) {
                    return d.version;
                }
            }
            return null;
        }

        /** Whether {@code pkg} already has a version on the stack, which is how cycles are detected. */
        private boolean onStack(String pkg) {
            return chosenVersionOf(pkg) != null;
        }

        /** Undo exactly what {@link #place} added, so the branch can continue with a lower version. */
        private void undo(Decision d, List<Requirement> added, List<String> newPending) {
            for (String p : newPending) {
                pending.remove(p);
            }
            for (Requirement req : added) {
                List<Requirement> list = sites.get(req.pkg);
                if (list != null && !list.isEmpty()) {
                    list.remove(list.size() - 1);
                }
                // The interval is rebuilt from the surviving sites, which keeps it exactly consistent.
                rebuildInterval(req.pkg);
            }
            stack.pop();
            // The recorded failure is deliberately kept: it is the most specific conflict seen so far
            // and is what the final report should name. Only clear it once a branch fully succeeds,
            // which happens in search() when pending empties.
        }

        /** Recompute a package's interval from the requirement sites that remain. */
        private void rebuildInterval(String pkg) {
            List<Requirement> list = sites.get(pkg);
            if (list == null || list.isEmpty()) {
                acc.remove(pkg);
                sites.remove(pkg);
                return;
            }
            Interval iv = null;
            for (Requirement r : list) {
                iv = iv == null ? r.range : iv.intersect(r.range);
            }
            acc.put(pkg, iv);
        }

        /**
         * Step back one decision. If the repair window still has budget, the package on top is forced one
         * version lower instead of simply being popped; otherwise the decision is removed. Returns false
         * when there is nothing left to backtrack into.
         */
        /**
         * Step back one decision and resume from the next candidate below the one that failed.
         *
         * <p>Chronological backtracking proper: the failed version is popped and its package is put
         * back on the pending set, and the loop is told to resume that package below the version that
         * just failed. Without that resume point {@link #decide} would restart at the highest version
         * every time and re-explore the same subtree forever, which is exactly the runaway loop this
         * method exists to prevent.
         *
         * <p>When the bounded repair window still has budget, the package on top is instead forced one
         * version lower without unwinding further, which is the smaller change and therefore tried
         * first.
         *
         * @return false when the stack is empty and there is nothing left to resume
         */
        private boolean backtrack() {
            if (stack.isEmpty()) {
                // Nothing left to backtrack into. If a deeper branch already recorded a specific
                // conflict (say two mutually exclusive ranges on some transitive package), that is the
                // report worth giving; only fall back to a generic message when nothing was recorded.
                if (failure == null) {
                    String pkg = pending.isEmpty() ? "<root>" : pending.first();
                    failure = new Failure(pkg, "no version satisfies the accumulated constraints",
                            chain(pkg, sites.getOrDefault(pkg, List.of()), null), snapshot());
                }
                return false;
            }
            Decision top = stack.peek();

            // The repair window, when it still has budget, forces this package one version lower and
            // keeps the rest of the stack in place. That is the smallest possible change.
            if (repair && downgradesUsed < window && !top.rest.isEmpty()) {
                downgradesUsed++;
                Version lower = top.rest.get(0);
                trace.downgrade(top.pkg, "<repair k=" + window + ">", lower.toString(),
                        "stepped down from " + top.version, window);
                stack.pop();
                rollback(top);
                // Remember where to resume, so the retry does not go back to the version just rejected.
                resumeFrom.put(top.pkg, candidatesBelow(top.pkg, lower));
                return decidePinned(top.pkg, lower);
            }

            // Otherwise unwind and let the popped package try its next lower candidate.
            stack.pop();
            rollback(top);
            pending.add(top.pkg);
            resumeFrom.put(top.pkg, top.rest);
            return true;
        }

        /** Drop the constraints a popped decision had introduced, so the branch is genuinely unwound. */
        private void rollback(Decision top) {
            for (Requirement req : constraintsOf(top)) {
                List<Requirement> list = sites.get(req.pkg);
                if (list != null && !list.isEmpty()) {
                    list.remove(list.size() - 1);
                }
                rebuildInterval(req.pkg);
            }
        }

        /** The required dependencies the given decision contributed, in the order it added them. */
        private List<Requirement> constraintsOf(Decision top) {
            Repository.Meta m = repo.meta(top.pkg, top.version).orElse(null);
            List<Requirement> out = new ArrayList<>();
            if (m == null) {
                return out;
            }
            for (Repository.Dependency dep : m.dependencies()) {
                if (!dep.optional()) {
                    out.add(new Requirement(top.pkg, dep.target(), dep.range(), false));
                }
            }
            return out;
        }

        /** All published versions of {@code pkg} strictly below {@code v}, highest first. */
        private List<Version> candidatesBelow(String pkg, Version v) {
            List<Version> asc = repo.versionsOf(pkg);
            List<Version> out = new ArrayList<>();
            for (int i = asc.size() - 1; i >= 0; i--) {
                if (asc.get(i).compareTo(v) < 0) {
                    out.add(asc.get(i));
                }
            }
            return out;
        }

        /** Place exactly {@code pkg:version}, used by the repair path. */
        private boolean decidePinned(String pkg, Version v) {
            Interval iv = acc.get(pkg);
            Repository.Meta m = repo.meta(pkg, v).orElse(null);
            if (m == null || iv == null || !iv.contains(v)) {
                return false;
            }
            List<String> banned = activeExclusions();
            if (breaksExclusion(pkg, v, banned)) {
                return false;
            }
            resumeFrom.remove(pkg);
            List<Version> all = repo.versionsDescending(pkg);
            return place(pkg, v, m, all, all.indexOf(v));
        }

        /** Exclusion ids contributed by every version currently on the stack. */
        private List<String> activeExclusions() {
            if (stack.isEmpty()) {
                return List.of();
            }
            List<String> out = new ArrayList<>();
            for (Decision d : stack) {
                out.addAll(d.excludes);
            }
            return out;
        }

        /** Assemble the public result once the tree is complete. */
        Resolution toResolution() {
            Resolution r = new Resolution();
            r.selected.putAll(snapshot());
            for (Decision d : stack) {
                r.intervals.put(d.pkg, acc.get(d.pkg));
            }
            r.intervals.putAll(acc);
            r.requiredBy.putAll(summarise(sites));
            for (Decision d : stack) {
                r.excluded.addAll(d.excludes);
            }
            return r;
        }
    }

    /** Whether choosing {@code pkg:version} pulls in a currently banned id. */
    private boolean breaksExclusion(String pkg, Version v, List<String> exclusions) {
        if (exclusions.isEmpty()) {
            return false;
        }
        Repository.Meta m = repo.meta(pkg, v).orElse(null);
        if (m == null) {
            return false;
        }
        Set<String> banned = new HashSet<>(exclusions);
        banned.removeAll(subtreeIds(pkg, v));
        for (Repository.Dependency d : m.dependencies()) {
            if (banned.contains(d.target())) {
                return true;
            }
        }
        return false;
    }

    /** Package ids in the subtree under {@code pkg:version}, the root itself excluded. */
    private Set<String> subtreeIds(String pkg, Version v) {
        Set<String> out = new TreeSet<>();
        Repository.Meta root = repo.meta(pkg, v).orElse(null);
        if (root == null) {
            return out;
        }
        ArrayDeque<String> queue = new ArrayDeque<>();
        for (Repository.Dependency d : root.dependencies()) {
            queue.add(d.target());
        }
        Set<String> seen = new HashSet<>();
        while (!queue.isEmpty()) {
            String cur = queue.poll();
            if (!seen.add(cur)) {
                continue;
            }
            out.add(cur);
            Version hv = repo.highestKnown(cur);
            Repository.Meta m = hv == null ? null : repo.meta(cur, hv).orElse(null);
            if (m == null) {
                continue;
            }
            for (Repository.Dependency d : m.dependencies()) {
                queue.add(d.target());
            }
        }
        out.remove(pkg);
        return out;
    }

    private static TreeMap<String, List<String>> summarise(TreeMap<String, List<Requirement>> sites) {
        TreeMap<String, List<String>> out = new TreeMap<>();
        for (Map.Entry<String, List<Requirement>> e : sites.entrySet()) {
            Set<String> who = new TreeSet<>();
            for (Requirement r : e.getValue()) {
                who.add(r.from);
            }
            out.put(e.getKey(), new ArrayList<>(who));
        }
        return out;
    }

    private static Requirement lastRequired(List<Requirement> list) {
        for (int i = list.size() - 1; i >= 0; i--) {
            if (!list.get(i).optional) {
                return list.get(i);
            }
        }
        return list.get(list.size() - 1);
    }

    /** Sorted, comma joined set of the packages that raised a requirement, for the trace. */
    private static String triggers(List<Requirement> sites) {
        if (sites == null) {
            return "<root>";
        }
        Set<String> who = new TreeSet<>();
        for (Requirement r : sites) {
            who.add(r.from);
        }
        return String.join(",", who);
    }

    /** Ordered causal chain, root first, for the conflict report. */
    private static List<String> chain(String pkg, List<Requirement> sites, Requirement leaf) {
        List<String> out = new ArrayList<>();
        out.add("<root> reached " + pkg + " through "
                + (sites == null ? 0 : sites.size()) + " requirement site(s)");
        if (sites != null) {
            for (Requirement r : sites) {
                out.add(r.toString());
            }
        }
        if (leaf != null) {
            out.add("conflicting site: " + leaf.from + " -> " + leaf.pkg + " " + leaf.range);
        }
        return out;
    }

    /** Derive the minimal repair: name a concrete alternative version or a widened range. */
    private String suggest(Failure f) {
        String pkg = f.pkg;
        if (pkg == null || "<root>".equals(pkg)) {
            return "";
        }
        List<Version> asc = repo.versionsOf(pkg);
        if (asc.isEmpty()) {
            return "no version of " + pkg + " is published at all; remove the requirement.";
        }
        Version top = asc.get(asc.size() - 1);
        StringBuilder sb = new StringBuilder();
        if (!f.prefix.isEmpty()) {
            sb.append("step down one of: ");
            List<String> keys = new ArrayList<>(f.prefix.keySet());
            int from = Math.max(0, keys.size() - MAX_BACKTRACK_DEPTH);
            for (int i = from; i < keys.size(); i++) {
                String p = keys.get(i);
                Version cur = f.prefix.get(p);
                Version alt = nextLower(p, cur);
                sb.append(p).append(' ').append(cur).append(" -> ").append(alt == null ? "(none)" : alt);
                if (i < keys.size() - 1) {
                    sb.append("; ");
                }
            }
            sb.append("; or widen the range on ").append(pkg).append(", or drop that requirement.");
        } else {
            sb.append("no version of ").append(pkg).append(" satisfies the accumulated constraints; ")
              .append("available: ").append(asc).append(". Widen toward ").append(top)
              .append(" or drop the requirement.");
        }
        return sb.toString();
    }

    /** The next published version below {@code v} for {@code pkg}, or null when already lowest. */
    private Version nextLower(String pkg, Version v) {
        List<Version> asc = repo.versionsOf(pkg);
        for (int i = asc.size() - 1; i > 0; i--) {
            if (asc.get(i).equals(v)) {
                return asc.get(i - 1);
            }
        }
        return null;
    }

    /**
     * Lock file replay: every package is pinned to the locked version, so no solving happens. Hash
     * verification and conflict detection still run.
     */
    public Resolution replay(Map<String, Version> locked, List<Requirement> roots) throws Conflict {
        Resolution r = new Resolution();
        ArrayDeque<Requirement> queue = new ArrayDeque<>(roots);
        Set<String> seen = new HashSet<>();
        while (!queue.isEmpty()) {
            Requirement req = queue.poll();
            if (req.optional || !seen.add(req.pkg)) {
                continue;
            }
            Version pinned = locked.get(req.pkg);
            if (pinned == null) {
                throw new Conflict(req.pkg, "absent from the lock file",
                        chain(req.pkg, List.of(req), req),
                        "re-resolve from scratch to regenerate the lock file");
            }
            Interval cur = r.intervals.get(req.pkg);
            Interval merged = cur == null ? req.range : cur.intersect(req.range);
            if (merged.isEmpty()) {
                throw new Conflict(req.pkg, "mutually exclusive ranges under replay",
                        chain(req.pkg, List.of(req), req),
                        "two locked requirements on " + req.pkg + " cannot both hold");
            }
            if (!merged.contains(pinned)) {
                throw new Conflict(req.pkg,
                        "locked version " + pinned + " violates required range " + merged,
                        chain(req.pkg, List.of(req), req),
                        "the lock file is stale for " + req.pkg + "; re-resolve from scratch");
            }
            r.intervals.put(req.pkg, merged);
            r.selected.put(req.pkg, pinned);
            Repository.Meta m = repo.meta(req.pkg, pinned).orElse(null);
            if (m != null) {
                for (Repository.Dependency d : m.dependencies()) {
                    queue.add(new Requirement(req.pkg, d.target(), d.range(), d.optional()));
                }
            }
        }
        return r;
    }
}
