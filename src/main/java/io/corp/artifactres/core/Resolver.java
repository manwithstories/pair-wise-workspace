package io.corp.artifactres.core;

import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * Deterministic, offline dependency resolver.
 *
 * <p>Data flow: root requirement &rarr; interval intersection &rarr; greedy
 * highest-version selection &rarr; iterative-deepening local repair &rarr; lock file write
 * or replay.
 *
 * <h2>Determinism</h2>
 * The frontier is a {@link TreeMap} keyed by package name, so packages are always visited
 * in natural name order. Candidate versions are compared with {@link Version#compareTo}
 * and the highest satisfying one is taken first. Nothing consults hash order, wall-clock
 * time, or thread scheduling, so two runs over identical metadata produce byte-identical
 * results.
 *
 * <h2>Bounded search</h2>
 * Each pass runs a depth-first search over the frontier. For the package it is currently
 * on, it tries candidates highest-first and recurses into whichever one commits. The
 * parameter K bounds how many decisions may settle on something other than their greedy
 * version; anything past that is not attempted. A failed branch is rewound exactly, so a
 * sibling branch never observes residue from it.
 *
 * <p>K grows from 1 to {@link #MAX_DEPTH} across passes and the first pass that succeeds
 * wins, so the reported fix is the one that changes the fewest decisions. Each pass is
 * additionally capped by a placement budget, so total work is bounded independently of the
 * shape of the input graph: the resolver always terminates with either a tree or an
 * actionable conflict, never an unbounded search.
 *
 * <h2>Backtracking bookkeeping</h2>
 * State is not snapshotted per node (that is O(packages) and blows the heap budget on a
 * 2000-package graph). Instead every mutation is pushed onto a trail holding the previous
 * value of the key it touched, so rewinding costs time proportional to the work the branch
 * actually did.
 *
 * <h2>Exclusions</h2>
 * An exclusion on {@code owner@version} forbids the target from appearing anywhere in that
 * owner's subtree. Because the frontier is processed in package-name order, an ancestor is
 * placed before any of its descendants, so the check is a walk of the recorded ancestor
 * chain.
 */
public final class Resolver {

    /** Maximum backtrack depth K. Beyond this the resolver reports a conflict. */
    public static final int MAX_DEPTH = 8;

    /** Default placement cap per pass; keeps each pass provably finite. */
    private static final int DECISIONS_PER_PACKAGE_FACTOR = 4;
    private static final int DECISIONS_PER_PASS_FLOOR = 256;

    // ------------------------------------------------------------ value types

    /** One package (groupId:artifactId) and the versions present in local metadata. */
    public static final class Module {
        public final String id;
        /** Available versions, ascending in natural order. */
        public final List<Version> versions;
        /** version canonical text -&gt; the edges that version declares. */
        private final Map<String, List<Requirement>> edges;

        Module(String id, List<Version> versions, Map<String, List<Requirement>> edges) {
            this.id = id;
            this.versions = versions;
            this.edges = edges;
        }

        /** @return requirements declared by exactly this version, in a stable order. */
        public List<Requirement> depsOf(Version v) {
            List<Requirement> l = edges.get(v.toString());
            return l == null ? Collections.emptyList() : l;
        }

        /** @return targets excluded at exactly this version, ascending. */
        public List<String> exclusionsOf(Version v) {
            List<Requirement> l = depsOf(v);
            if (l.isEmpty()) {
                return Collections.emptyList();
            }
            List<String> out = new ArrayList<>(2);
            for (Requirement r : l) {
                if (r.exclusion) {
                    out.add(r.target);
                }
            }
            Collections.sort(out);
            return out;
        }

        @Override
        public String toString() {
            return id;
        }

        /** Fluent builder for local metadata. */
        public static Builder builder(String id) {
            return new Builder(id);
        }

        /** Accumulates versions and edges for one package. */
        public static final class Builder {
            private final String id;
            private final TreeMap<Version, Boolean> versions = new TreeMap<>();
            private final Map<String, List<Requirement>> edges = new HashMap<>();
            private final Map<String, Boolean> depSeen = new HashMap<>();

            Builder(String id) {
                this.id = id;
            }

            /** Declare a version with no direct edges. */
            public Builder version(String v) {
                versions.put(Version.parse(v), Boolean.TRUE);
                edges.computeIfAbsent(Version.parse(v).toString(), k -> new ArrayList<>());
                return this;
            }

            /** Declare a version plus the requirement window it imposes on {@code target}. */
            public Builder version(String v, String target, String range) {
                Version ver = Version.parse(v);
                versions.put(ver, Boolean.TRUE);
                return dep(ver, target, range, false);
            }

            /** Declare an exclusion from {@code target} at this version. */
            public Builder exclude(String v, String target) {
                Version ver = Version.parse(v);
                versions.put(ver, Boolean.TRUE);
                return dep(ver, target, null, true);
            }

            private Builder dep(Version v, String target, String range, boolean exclusion) {
                List<Requirement> l = edges.computeIfAbsent(v.toString(), k -> new ArrayList<>());
                String rangeKey = exclusion ? "!excl" : range;
                // The dedupe key MUST include the declaring version: two versions of the
                // same package can legitimately declare the same edge.
                String dedupe = v + "|" + target + "|" + rangeKey;
                if (Boolean.TRUE.equals(depSeen.get(dedupe))) {
                    return this;
                }
                depSeen.put(dedupe, Boolean.TRUE);
                Interval iv = exclusion
                        ? Interval.none()
                        : Interval.parse(range, id + "@" + v + " -> " + target + " " + range);
                l.add(new Requirement(id + "@" + v, target, iv, exclusion));
                return this;
            }

            public Module build() {
                List<Version> vs = new ArrayList<>(versions.keySet());
                List<Version> sorted = new ArrayList<>(vs);
                sorted.sort(Comparator.naturalOrder());
                Map<String, List<Requirement>> copy = new HashMap<>(edges.size() * 2);
                for (Map.Entry<String, List<Requirement>> e : edges.entrySet()) {
                    List<Requirement> l = new ArrayList<>(e.getValue());
                    // Stable edge order keeps lock output and trace byte-identical.
                    l.sort(Comparator.comparing((Requirement r) -> r.target)
                            .thenComparing(r -> r.range.toString()));
                    copy.put(e.getKey(), l);
                }
                return new Module(id, Collections.unmodifiableList(sorted), copy);
            }
        }
    }

    /** A declared dependency edge with the provenance needed for conflict attribution. */
    public static final class Requirement {
        /** "owner@version" or "&lt;root&gt;". */
        public final String from;
        public final String target;
        public final Interval range;
        public final boolean exclusion;

        public Requirement(String from, String target, Interval range, boolean exclusion) {
            this.from = from;
            this.target = target;
            this.range = range;
            this.exclusion = exclusion;
        }

        @Override
        public String toString() {
            return exclusion
                    ? from + " excludes " + target
                    : from + " -> " + target + " " + range;
        }
    }

    /** The resolved tree: package -> version, plus the window each choice had to satisfy. */
    public static final class Resolution {
        private final TreeMap<String, Version> selected;
        private final TreeMap<String, Interval> windows;
        private final TreeMap<String, String> lockLines;
        private final List<String> notes;

        Resolution(TreeMap<String, Version> selected, TreeMap<String, Interval> windows,
                   TreeMap<String, String> lockLines, List<String> notes) {
            this.selected = selected;
            this.windows = windows;
            this.lockLines = lockLines;
            this.notes = List.copyOf(notes);
        }

        public Version versionOf(String id) {
            return selected.get(id);
        }

        /** @return the resolution in package-name order. */
        public Map<String, Version> selected() {
            return Collections.unmodifiableMap(selected);
        }

        public Map<String, Interval> windows() {
            return Collections.unmodifiableMap(windows);
        }

        /** @return deterministic lock lines, one per package. */
        public TreeMap<String, String> lockLines() {
            return lockLines;
        }

        /** @return downgrades and other non-fatal observations, in decision order. */
        public List<String> notes() {
            return notes;
        }

        public int size() {
            return selected.size();
        }

        /** @return the collapsed window set per package, folded and stably ordered. */
        public Map<String, List<Interval>> collapsedWindows() {
            Map<String, List<Interval>> out = new TreeMap<>();
            for (Map.Entry<String, Interval> e : windows.entrySet()) {
                out.put(e.getKey(), Interval.collapse(List.of(e.getValue())));
            }
            return out;
        }

        @Override
        public String toString() {
            StringBuilder sb = new StringBuilder(selected.size() * 44);
            for (Map.Entry<String, Version> e : selected.entrySet()) {
                sb.append(e.getKey()).append(" -> ").append(e.getValue()).append('\n');
            }
            return sb.toString();
        }
    }

    /** Raised when no assignment exists within {@link #MAX_DEPTH} backtrack levels. */
    public static final class Conflict extends RuntimeException {
        private static final long serialVersionUID = 1L;

        /** Package with no satisfiable version. */
        public final String pkg;
        /** The window all constraints collapsed to (empty when mutually exclusive). */
        public final Interval window;
        /** The constraint supplying the surviving lower edge. */
        public final String conflictA;
        /** The constraint supplying the surviving upper edge, or the offending incoming one. */
        public final String conflictB;
        /** The smallest change that repairs the conflict. */
        public final String repair;
        /** Deepest K that was attempted. */
        public final int depth;

        Conflict(String pkg, Interval window, String a, String b, String repair, int depth) {
            super(message(pkg, window, a, b, repair, depth));
            this.pkg = pkg;
            this.window = window;
            this.conflictA = a;
            this.conflictB = b;
            this.repair = repair;
            this.depth = depth;
        }

        private static String message(String pkg, Interval window, String a, String b,
                                      String repair, int depth) {
            StringBuilder sb = new StringBuilder(192);
            sb.append("resolution conflict on '").append(pkg).append("' after ")
                    .append(depth).append(" backtrack level(s)");
            sb.append("\n  collapsed window: ").append(window);
            sb.append("\n  mutually exclusive constraints:");
            sb.append("\n    - ").append(a == null ? "<none>" : a);
            sb.append("\n    - ").append(b == null ? "<none>" : b);
            sb.append("\n  minimal repair: ").append(repair);
            return sb.toString();
        }
    }

    // ------------------------------------------------------------- solver state

    private final TreeMap<String, Module> repo;
    private final Trace trace;
    private final List<Requirement> roots = new ArrayList<>();

    /** Assigned packages. TreeMap so the result is always in package-name order. */
    private final TreeMap<String, Version> assign = new TreeMap<>();
    /** Pending constraints for packages that are not yet assigned. */
    private final TreeMap<String, List<Requirement>> frontier = new TreeMap<>();
    /** Current intersected window per assigned package. */
    private final Map<String, Interval> window = new HashMap<>();
    /** Constraint that supplied the lower edge of each package's window. */
    private final Map<String, Requirement> lowSrc = new HashMap<>();
    /** Constraint that supplied the upper edge of each package's window. */
    private final Map<String, Requirement> highSrc = new HashMap<>();
    /** Placement-order ancestor chain per assigned package. */
    private final Map<String, List<String>> ancestors = new HashMap<>();
    /** Ancestor chain for each queued package, frozen when it first enters the frontier. */
    private final Map<String, List<String>> ancestorStacks = new HashMap<>();

    private int lastDepth;
    private int lastDecisions;
    private final List<String> notes = new ArrayList<>();

    public Resolver(Map<String, Module> repo, Trace trace) {
        TreeMap<String, Module> copy = new TreeMap<>();
        if (repo != null) {
            copy.putAll(repo);
        }
        this.repo = copy;
        this.trace = trace == null ? Trace.disabled() : trace;
    }

    public Trace trace() {
        return trace;
    }

    /** Deepest K attempted by the last {@link #resolve()}. */
    public int lastDepth() {
        return lastDepth;
    }

    /** Placements performed by the successful pass; 0 when the run failed. */
    public int lastDecisions() {
        return lastDecisions;
    }

    public Resolver require(String target, String range) {
        Interval iv = Interval.parse(range, "<root> -> " + target + " " + range);
        roots.add(new Requirement("<root>", target, iv, false));
        return this;
    }

    public Resolver require(String target, Interval range) {
        roots.add(new Requirement("<root>", target,
                range.withOrigin("<root> -> " + target + " " + range), false));
        return this;
    }

    // ------------------------------------------------------------------ resolve

    /**
     * Resolve the declared roots.
     *
     * @throws Conflict when no assignment exists within {@link #MAX_DEPTH} backtrack levels
     */
    public Resolution resolve() {
        int perPass = Math.max(DECISIONS_PER_PASS_FLOOR, repo.size() * DECISIONS_PER_PACKAGE_FACTOR);
        Conflict deepest = null;

        // Iterative deepening: the first pass that succeeds used the fewest repairs.
        for (int k = 1; k <= MAX_DEPTH; k++) {
            lastDepth = k;
            Pass pass = runPass(k, perPass);
            if (pass.resolution != null) {
                lastDecisions = pass.decisions;
                return pass.resolution;
            }
            if (deepest == null || pass.conflict != null) {
                // Keep the first (most specific) conflict; later passes reuse it.
                if (deepest == null) {
                    deepest = pass.conflict;
                }
            }
        }
        throw deepest != null
                ? deepest
                : new Conflict("<unknown>", Interval.any(), null, null,
                        "declare a root requirement", MAX_DEPTH);
    }

    /** Outcome of one bounded pass. */
    private static final class Pass {
        Resolution resolution;
        Conflict conflict;
        int decisions;
    }

    /**
     * One recorded mutation, held on a trail so a failed branch can be undone in time
     * proportional to the work it did rather than to the size of the graph.
     *
     * <p>A full snapshot per decision is O(packages), which over a 2000-package graph is
     * O(packages^2) allocation and blows the 64MB ceiling. The trail records the previous
     * value of only the keys a branch actually writes, so memory stays proportional to the
     * depth of the current path rather than to the graph.
     */
    private interface Undo {
        void undo();
    }

    /** Trail of pending mutations; {@link #mark()} / {@link #rewind(int)} bracket a branch. */
    private final List<Undo> trail = new ArrayList<>();

    private int mark() {
        return trail.size();
    }

    /** Undo every mutation recorded after {@code m}. */
    private void rewind(int m) {
        while (trail.size() > m) {
            trail.remove(trail.size() - 1).undo();
        }
    }

    /** Record {@code map.put(key, value)} as reversible. */
    private <V> void putTracked(Map<String, V> map, String key, V value) {
        final boolean hadKey = map.containsKey(key);
        final V prev = map.put(key, value);
        trail.add(() -> {
            if (hadKey) {
                map.put(key, prev);
            } else {
                map.remove(key);
            }
        });
    }

    /** Remove {@code key}, recording the prior value for undo. */
    private <V> void removeTracked(Map<String, V> map, String key) {
        if (!map.containsKey(key)) {
            return;
        }
        V prev = map.remove(key);
        trail.add(() -> map.put(key, prev));
    }

    /** Clear all mutable solver state before a pass. */
    private void reset() {
        assign.clear();
        frontier.clear();
        window.clear();
        lowSrc.clear();
        highSrc.clear();
        ancestors.clear();
        ancestorStacks.clear();
        trail.clear();
        notes.clear();
    }

    /**
     * One depth-bounded pass.
     *
     * <p>{@code k} is the number of downgrades the search may force, counted as the number
     * of decision points at which a candidate other than the greedy (highest) one is
     * committed. With {@code k = 0} the pass is pure greedy; each increment buys one more
     * forced downgrade. Because candidates are always tried highest-first, the first pass
     * that succeeds used the fewest possible downgrades — that is the "minimum fix".
     *
     * <p>The search recurses on a mutable assignment guarded by an undo log, and every
     * commit/rollback is exact, so the traversal is exhaustive below the bound and
     * terminating above it.
     */
    private Pass runPass(int k, int perPass) {
        reset();
        Pass pass = new Pass();
        Budget budget = new Budget(k);
        for (Requirement r : roots) {
            if (!accept(r, null, pass)) {
                return pass;
            }
        }
        if (search(budget, pass, perPass)) {
            Conflict bad = validateAssignment();
            if (bad != null) {
                // Defensive: search() must never report success on an inconsistent
                // assignment. Surfacing it beats emitting a silently wrong tree.
                pass.resolution = null;
                pass.conflict = bad;
                return pass;
            }
            pass.resolution = build();
            pass.decisions = budget.decisions;
        }
        return pass;
    }

    /**
     * Verify the finished assignment: every committed edge's window must admit the
     * version actually chosen for its target, and no exclusion may be violated.
     *
     * @return null when consistent, else the first violation as a Conflict
     */
    private Conflict validateAssignment() {
        for (Map.Entry<String, Version> e : assign.entrySet()) {
            Module m = repo.get(e.getKey());
            if (m == null) {
                continue;
            }
            for (Requirement r : m.depsOf(e.getValue())) {
                Version tv = assign.get(r.target);
                if (tv == null) {
                    continue;
                }
                if (r.exclusion) {
                    return new Conflict(r.target, Interval.any(),
                            e.getKey() + "@" + e.getValue() + " excludes " + r.target, null,
                            "remove the exclusion of " + r.target + " from " + e.getKey(),
                            lastDepth);
                }
                if (!r.range.contains(tv)) {
                    return new Conflict(r.target, r.range,
                            e.getKey() + "@" + e.getValue() + " requires " + r.target + " " + r.range,
                            null, "the chosen " + tv + " does not satisfy " + r.range, lastDepth);
                }
            }
        }
        return null;
    }

    /** Counts forced downgrades and hard-capped total placements. */
    private static final class Budget {
        final int downgrades;
        int used;
        int decisions;

        Budget(int downgrades) {
            this.downgrades = downgrades;
        }
    }

    /**
     * Recursive search over the frontier.
     *
     * @return true when every constrained package is assigned consistently
     */
    private boolean search(Budget budget, Pass pass, int perPass) {
        if (budget.decisions >= perPass) {
            pass.conflict = new Conflict(frontier.isEmpty() ? "<done>" : frontier.firstKey(),
                    Interval.any(), null, null,
                    "reduce the size of the dependency graph (placement budget exhausted)",
                    budget.downgrades);
            return false;
        }
        if (frontier.isEmpty()) {
            return true;
        }

        // Visit the first constrained package in name order: reproducible traversal.
        String pkg = frontier.firstKey();
        List<Requirement> reqs = frontier.get(pkg);
        removeTracked(frontier, pkg);

        Module m = repo.get(pkg);
        if (m == null) {
            // Nothing local to choose from: a metadata gap, not a version conflict.
            trace.record(pkg, Trace.Kind.NO_CANDIDATE, null, Interval.any(), 0,
                    budget.downgrades, reqs.isEmpty() ? null : reqs.get(0).from, null,
                    "package is absent from the local metadata directory");
            putTracked(frontier, pkg, reqs);
            pass.conflict = new Conflict(pkg, Interval.any(), null, null,
                    "publish " + pkg + " to the local metadata directory", budget.downgrades);
            return false;
        }

        Level level = openLevel(pkg, reqs, m, budget, pass);
        if (level == null) {
            // Mutually exclusive constraints, or every candidate excluded: openLevel has
            // already recorded the conflict. Propagate it straight up.
            return false;
        }

        // Bracket every candidate: each failed one is rewound to exactly this point, so a
        // sibling branch can never observe residue from a failed one.
        int base = mark();

        // Candidates highest-first: the first iteration is the pure greedy choice.
        for (int idx = 0; idx < level.viable.size(); idx++) {
            Version v = level.viable.get(level.viable.size() - 1 - idx);
            boolean isDowngrade = idx > 0 || budget.used > 0;
            if (isDowngrade && budget.used >= budget.downgrades) {
                break; // out of forced-downgrade budget for this pass
            }

            rewind(base);
            if (isDowngrade) {
                budget.used++;
            }

            trace.push(pkg);
            if (commit(pkg, v, level.window, level.lowFrom, level.highFrom,
                    level.ancestors, pass)) {
                budget.decisions++;
                trace.record(pkg, isDowngrade ? Trace.Kind.DOWNGRADE : Trace.Kind.GREEDY,
                        v, level.window, level.candidateCount, budget.downgrades,
                        level.trigger, level.lowFromText,
                        isDowngrade
                                ? "forced downgrade: a higher candidate broke a constraint"
                                : "highest version satisfying " + level.window);
                if (search(budget, pass, perPass)) {
                    trace.pop();
                    return true;
                }
            }
            trace.pop();
            if (isDowngrade) {
                budget.used--; // this downgrade did not pan out
            }
        }

        // Every candidate failed under the current budget. Keep the most specific
        // diagnosis seen anywhere below, then restore for the caller.
        if (pass.conflict == null) {
            trace.record(pkg, Trace.Kind.NO_CANDIDATE, null, level.window, level.candidateCount,
                    budget.downgrades, level.trigger, level.lowFromText,
                    level.candidateCount == 0
                            ? "no versions published for this package"
                            : "no published version satisfies " + level.window
                              + " within K=" + budget.downgrades + " downgrades");
            pass.conflict = conflict(pkg, budget.downgrades, null);
        }
        rewind(base);
        return false;
    }

    /** The resolved window, ancestor chain and ordered candidate list for one package. */
    private static final class Level {
        final String pkg;
        final Interval window;
        final Requirement lowFrom;
        final Requirement highFrom;
        final String lowFromText;
        final String trigger;
        final List<String> ancestors;
        /** Viable versions in ascending order; tried highest-first. */
        final List<Version> viable;
        final int candidateCount;

        Level(String pkg, Interval window, Requirement lowFrom, Requirement highFrom,
              String lowFromText, String trigger, List<String> ancestors,
              List<Version> viable, int candidateCount) {
            this.pkg = pkg;
            this.window = window;
            this.lowFrom = lowFrom;
            this.highFrom = highFrom;
            this.lowFromText = lowFromText;
            this.trigger = trigger;
            this.ancestors = ancestors;
            this.viable = viable;
            this.candidateCount = candidateCount;
        }
    }

    /**
     * Compute the intersected window, the exclusion-aware ancestor chain and the ordered
     * candidate list for one package.
     *
     * @return null when this package cannot be placed at all (mutually exclusive
     *         constraints, or every in-window version excluded by an ancestor); in that case
     *         the conflict is recorded on {@code pass} and the caller must propagate it
     */
    private Level openLevel(String pkg, List<Requirement> reqs, Module m,
                            Budget budget, Pass pass) {
        Interval w = Interval.any();
        Requirement loFrom = null;
        Requirement hiFrom = null;
        for (Requirement r : reqs) {
            if (r.exclusion) {
                continue;
            }
            Interval next = w.intersect(r.range);
            if (next.isEmptySet()) {
                trace.record(pkg, Trace.Kind.NO_CANDIDATE, null, w, 0, budget.downgrades,
                        r.from, loFrom == null ? null : loFrom.toString(),
                        "intersection became empty");
                putTracked(frontier, pkg, reqs);
                pass.conflict = new Conflict(pkg, next,
                        loFrom == null ? null : loFrom.toString(), r.toString(),
                        "relax one of the two conflicting windows so they overlap",
                        budget.downgrades);
                return null;
            }
            if (r.range.lo() != null && (w.lo() == null || r.range.lo().compareTo(w.lo()) > 0)) {
                loFrom = r;
            }
            if (r.range.hi() != null && (w.hi() == null || r.range.hi().compareTo(w.hi()) < 0)) {
                hiFrom = r;
            }
            w = next;
        }

        List<String> anc = new ArrayList<>(
                ancestorStacks.getOrDefault(pkg, Collections.emptyList()));
        for (Requirement r : reqs) {
            if (!r.exclusion && r.from != null && r.from.indexOf('@') > 0) {
                String owner = r.from.substring(0, r.from.indexOf('@'));
                if (!anc.contains(owner)) {
                    anc.add(owner);
                }
            }
        }

        List<Version> viable = new ArrayList<>();
        int blocked = 0;
        for (Version v : m.versions) {
            if (!w.contains(v)) {
                continue;
            }
            if (excludedByAncestor(anc, pkg, v)) {
                blocked++;
                continue;
            }
            viable.add(v);
        }

        if (viable.isEmpty() && blocked > 0) {
            // Every in-window version is excluded by an ancestor: name the exclusion,
            // because "no version satisfies the window" points the reader the wrong way.
            int mark = mark();
            String owner = excludingAncestor(anc, pkg, w);
            String edge = owner + "@" + assign.get(owner) + " excludes " + pkg;
            trace.record(pkg, Trace.Kind.NO_CANDIDATE, null, w, m.versions.size(),
                    budget.downgrades, owner, edge,
                    "every version inside the window is excluded by an ancestor");
            putTracked(frontier, pkg, reqs);
            pass.conflict = new Conflict(pkg, w, null, edge,
                    "stop depending on " + pkg + " under " + owner
                            + ", or drop its exclusion of " + pkg,
                    budget.downgrades);
            rewind(mark);
            return null;
        }

        return new Level(pkg, w, loFrom, hiFrom,
                loFrom == null ? null : loFrom.range.origin(),
                reqs.isEmpty() ? null : reqs.get(0).from,
                anc, viable, m.versions.size());
    }

    /**
     * Commit a placement: assign {@code v}, record provenance, and fold its edges in.
     *
     * @return false when the placement is immediately inconsistent; the caller restores
     *         the snapshot, so nothing needs unwinding here
     */
    private boolean commit(String pkg, Version v, Interval w, Requirement loFrom, Requirement hiFrom,
                           List<String> anc, Pass pass) {
        putTracked(assign, pkg, v);
        putTracked(window, pkg, w);
        putTracked(lowSrc, pkg, loFrom);
        putTracked(highSrc, pkg, hiFrom);
        putTracked(ancestors, pkg, new ArrayList<>(anc));

        Module m = repo.get(pkg);
        List<Requirement> edges = m.depsOf(v);

        // An exclusion naming an already-assigned package is fatal: both can never coexist.
        for (Requirement e : edges) {
            if (e.exclusion && assign.containsKey(e.target)) {
                trace.record(pkg, Trace.Kind.NO_CANDIDATE, null, w, countCandidates(pkg),
                        lastDepth, e.from, e.range.origin(),
                        "excludes already-resolved package " + e.target);
                pass.conflict = new Conflict(pkg, w, null, e.toString(),
                        "remove the exclusion of " + e.target + " from " + e.from
                                + ", or stop depending on " + e.target + " elsewhere",
                        lastDepth);
                return false;
            }
        }

        for (Requirement e : edges) {
            if (!e.exclusion && !assign.containsKey(e.target) && !frontier.containsKey(e.target)) {
                // First sighting: freeze the ancestor chain for later exclusion checks.
                List<String> childAnc = new ArrayList<>(anc);
                childAnc.add(pkg);
                putTracked(ancestorStacks, e.target, childAnc);
            }
            if (!accept(e, pkg, pass)) {
                return false;
            }
        }
        return true;
    }

    /**
     * Fold one requirement into the resolver state.
     *
     * <p>A requirement for an already-assigned package is validated against the current
     * assignment rather than re-placed, which is what makes cyclic graphs terminate
     * instead of looping.
     *
     * @return false when the requirement cannot be satisfied
     */
    private boolean accept(Requirement r, String via, Pass pass) {
        if (r.exclusion) {
            // Evaluated at commit time against the assignment; nothing pending here.
            return true;
        }
        String target = r.target;
        Version have = assign.get(target);

        if (have != null) {
            Interval cur = window.get(target);
            Interval merged = cur.intersect(r.range);
            if (merged.isEmptySet()) {
                trace.record(target, Trace.Kind.NO_CANDIDATE, have, cur, countCandidates(target),
                        lastDepth, r.from, r.range.origin(),
                        "already resolved to " + have + ", which violates " + r.range);
                pass.conflict = new Conflict(target, merged,
                        lowSrc.get(target) == null ? null : lowSrc.get(target).toString(),
                        r.toString(),
                        "relax " + r.from + "'s window on " + target + " to admit " + have
                                + ", or drop it",
                        lastDepth);
                return false;
            }
            putTracked(window, target, merged);
            putTracked(lowSrc, target, r);
            return true;
        }

        List<Requirement> list = frontier.get(target);
        if (list == null) {
            list = new ArrayList<>(2);
            putTracked(frontier, target, list);
        }
        for (Requirement existing : list) {
            if (existing.exclusion == r.exclusion && existing.range.equals(r.range)) {
                return true; // duplicate edge, already tracked
            }
        }
        // Copy-on-write the bucket so rewinding the trail restores the exact prior list.
        List<Requirement> copy = new ArrayList<>(list);
        copy.add(r);
        putTracked(frontier, target, copy);
        return true;
    }

    /** @return the number of locally published versions for {@code pkg}. */
    private int countCandidates(String pkg) {
        Module m = repo.get(pkg);
        return m == null ? 0 : m.versions.size();
    }

    /** @return the first ancestor in {@code anc} that excludes {@code pkg}, or null. */
    private String excludingAncestor(List<String> anc, String pkg, Interval w) {
        for (String a : anc) {
            Version av = assign.get(a);
            if (av == null) {
                continue;
            }
            Module am = repo.get(a);
            if (am == null) {
                continue;
            }
            for (Requirement e : am.depsOf(av)) {
                if (e.exclusion && e.target.equals(pkg)) {
                    return a;
                }
            }
        }
        return null;
    }

    /** @return true when any placed ancestor excludes {@code pkg} at its chosen version. */
    private boolean excludedByAncestor(List<String> anc, String pkg, Version v) {
        for (String a : anc) {
            Version av = assign.get(a);
            if (av == null) {
                continue;
            }
            Module am = repo.get(a);
            if (am == null) {
                continue;
            }
            for (Requirement e : am.depsOf(av)) {
                if (e.exclusion && e.target.equals(pkg)) {
                    return true;
                }
            }
        }
        return false;
    }

    /** Build the conflict for an unsatisfiable package, naming the two rival constraints. */
    private Conflict conflict(String pkg, int k, String overrideRepair) {
        Interval w = window.getOrDefault(pkg, Interval.any());
        if (w == null) {
            w = Interval.any();
        }
        Module m = repo.get(pkg);
        String a = lowSrc.get(pkg) == null ? null : lowSrc.get(pkg).toString();
        String b = highSrc.get(pkg) == null ? null : highSrc.get(pkg).toString();
        String repair;
        if (overrideRepair != null) {
            repair = overrideRepair;
        } else if (m == null || m.versions.isEmpty()) {
            repair = "publish " + pkg + " to the local metadata directory";
        } else {
            Version near = nearestPublished(pkg, w);
            repair = "widen the window on " + pkg + " to include "
                    + (near == null ? "a published version" : near.toString())
                    + ", or publish that version";
        }
        return new Conflict(pkg, w, a, b, repair, k);
    }

    /**
     * @return the nearest published version just outside {@code w} -- the first one above
     *         it, or else the highest one below, so a repair hint always names something
     *         that actually exists locally
     */
    private Version nearestPublished(String pkg, Interval w) {
        Module m = repo.get(pkg);
        if (m == null) {
            return null;
        }
        Version below = null;
        for (Version v : m.versions) {
            if (w.hi() != null && v.compareTo(w.hi()) > 0) {
                return v; // first version above the window: the cheapest way to widen
            }
            if (w.lo() == null || v.compareTo(w.lo()) < 0) {
                below = v; // otherwise track the highest version below the window
            }
        }
        return below;
    }

    private Resolution build() {
        TreeMap<String, Interval> w = new TreeMap<>(window);
        TreeMap<String, String> lines = new TreeMap<>();
        for (Map.Entry<String, Version> e : assign.entrySet()) {
            Module m = repo.get(e.getKey());
            List<Requirement> edges = m == null ? Collections.emptyList() : m.depsOf(e.getValue());
            List<String> parts = new ArrayList<>(edges.size());
            for (Requirement r : edges) {
                if (r.exclusion) {
                    parts.add("!" + r.target);
                } else {
                    Version dv = assign.get(r.target);
                    parts.add(r.target + "=" + (dv == null ? "?" : dv.toString()));
                }
            }
            lines.put(e.getKey(), e.getValue() + "|" + w.get(e.getKey()) + "|" + String.join(",", parts));
        }
        return new Resolution(new TreeMap<>(assign), w, lines, new ArrayList<>(notes));
    }
}
