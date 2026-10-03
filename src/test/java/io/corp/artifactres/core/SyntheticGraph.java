package io.corp.artifactres.core;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Random;
import java.util.TreeMap;

/**
 * Deterministic synthetic dependency graphs for stress testing.
 *
 * <p>Everything is derived from a seed, so a failing run can be reproduced exactly by
 * replaying the same seed. Graphs are always generated in package-name order, which keeps
 * the resolver's traversal order (and therefore its trace and lock output) identical from
 * run to run.
 */
final class SyntheticGraph {

    /** Number of versions published per package. */
    static final int VERSIONS_PER_PACKAGE = 6;

    private SyntheticGraph() {
    }

    /** A generated graph plus the counts the performance budget is stated against. */
    static final class Graph {
        final TreeMap<String, Resolver.Module> repo;
        final List<String> roots;
        final int packages;
        final int constraints;

        Graph(TreeMap<String, Resolver.Module> repo, List<String> roots, int packages, int constraints) {
            this.repo = repo;
            this.roots = roots;
            this.packages = packages;
            this.constraints = constraints;
        }
    }

    /**
     * Build a connected graph of {@code packages} packages carrying {@code constraints}
     * dependency edges.
     *
     * <p>Edges are emitted per package so each builder is finalised exactly once, and the
     * declared window is widened for the majority of edges so the graph resolves without
     * needing backtracking. That keeps the measurement a measurement of <em>solving</em>
     * rather than of search.
     */
    static Graph build(int packages, int constraints, long seed) {
        Random rnd = new Random(seed);
        String[] ids = new String[packages];
        for (int i = 0; i < packages; i++) {
            ids[i] = "com.acme.g" + (i % 20) + ":m" + i;
        }

        // Accumulate edges per package/version before building, so a version's edges are
        // known before its module is constructed.
        Map<String, Map<String, List<String>>> edges = new LinkedHashMap<>();
        for (String id : ids) {
            edges.put(id, new TreeMap<>());
        }
        for (int i = 0; i < packages; i++) {
            for (int v = 1; v <= VERSIONS_PER_PACKAGE; v++) {
                edges.get(ids[i]).put(v + ".0", new ArrayList<>());
            }
        }

        // Chain i -> i+1 first so every package is reachable from the first root.
        int made = 0;
        for (int i = 0; i + 1 < packages && made < constraints; i++) {
            edges.get(ids[i]).get(VERSIONS_PER_PACKAGE + ".0")
                    .add(ids[i + 1] + " [" + VERSIONS_PER_PACKAGE + ".0,9.0)");
            made++;
        }
        // Remaining edges: random targets, windows widened enough to stay satisfiable.
        while (made < constraints) {
            int src = rnd.nextInt(packages);
            int dst = rnd.nextInt(packages);
            if (src == dst) {
                continue;
            }
            int major = rnd.nextInt(VERSIONS_PER_PACKAGE) + 1;
            // [major.0, major+3.0) always admits the major it was derived from, so the
            // edge constrains the solver without making the graph unsatisfiable.
            edges.get(ids[src]).get(major + ".0")
                    .add(ids[dst] + " [" + major + ".0," + (major + 3) + ".0)");
            made++;
        }

        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        for (Map.Entry<String, Map<String, List<String>>> e : edges.entrySet()) {
            Resolver.Module.Builder b = Resolver.Module.builder(e.getKey());
            for (Map.Entry<String, List<String>> v : e.getValue().entrySet()) {
                if (v.getValue().isEmpty()) {
                    b.version(v.getKey());
                    continue;
                }
                for (String dep : v.getValue()) {
                    int sp = dep.indexOf(' ');
                    b.version(v.getKey(), dep.substring(0, sp), dep.substring(sp + 1));
                }
            }
            repo.put(e.getKey(), b.build());
        }

        List<String> roots = List.of(ids[0]);
        return new Graph(repo, roots, packages, made);
    }

    /** @return a resolver primed with every root of {@code g}. */
    static Resolver resolverFor(Graph g, Trace trace) {
        Resolver r = new Resolver(g.repo, trace);
        for (String root : g.roots) {
            r.require(root, "[1.0,9.0)");
        }
        return r;
    }
}
