package com.corp.depsolver;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.DirectoryStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.TreeMap;

/**
 * Offline metadata catalog backed by a plain directory tree. No network, no third party library.
 *
 * <p>Layout, one file per published version:
 * <pre>
 *   &lt;root&gt;/&lt;groupId&gt;/&lt;artifactId&gt;/&lt;version&gt;.meta
 * </pre>
 * and inside each file a line oriented record:
 * <pre>
 *   id      com.acme:widget          # optional, inferred from the path when absent
 *   version 1.2.0                    # optional, inferred from the file name when absent
 *   dep     com.acme:core [1.2,1.5)  # required dependency with a range
 *   dep     com.acme:legacy (,1.4]   # upper bounded only
 *   dep     com.acme:opt [1.0,) opt   # optional dependency, contributes no constraint
 *   exclude com.acme:legacy          # exclusion, keeps the id out of the subtree
 * </pre>
 *
 * <p>Versions are kept in a {@link TreeMap} so the resolver can walk them in natural order, highest
 * first, without sorting on the hot path.
 */
public final class Repository {

    /**
     * One requirement edge, either required (constrains) or optional (does not). Constructed by
     * programmatic callers feeding {@link #of(Map)}; metadata files are parsed by the loader.
     */
    public static final class Dependency {
        private final String target;
        private final Interval range;
        private final boolean optional;

        public Dependency(String target, Interval range, boolean optional) {
            this.target = target;
            this.range = range;
            this.optional = optional;
        }

        public String target() {
            return target;
        }

        public Interval range() {
            return range;
        }

        public boolean optional() {
            return optional;
        }

        @Override
        public String toString() {
            return target + " " + range + (optional ? " (optional)" : "");
        }
    }

    /** The published metadata of a single version of a single package. */
    public static final class Meta {
        private final String id;
        private final Version version;
        private final List<Dependency> deps;
        private final List<String> excludes;

        public Meta(String id, Version version, List<Dependency> deps, List<String> excludes) {
            this.id = id;
            this.version = version;
            this.deps = List.copyOf(deps);
            this.excludes = List.copyOf(excludes);
        }

        public String id() {
            return id;
        }

        public Version version() {
            return version;
        }

        public List<Dependency> dependencies() {
            return deps;
        }

        public List<String> exclusions() {
            return excludes;
        }

        @Override
        public String toString() {
            return id + ":" + version + " deps=" + deps + " exclude=" + excludes;
        }
    }

    private final Path root;
    /** package id -> versions in natural order. */
    private final TreeMap<String, TreeMap<Version, Meta>> packages = new TreeMap<>();
    private final Map<String, Meta> byKey = new LinkedHashMap<>();

    private Repository(Path root) {
        this.root = root;
    }

    /** Load every {@code .meta} file found under {@code root}, scanning depth first in name order. */
    public static Repository load(Path root) throws IOException {
        Repository repo = new Repository(root);
        repo.scan(root);
        return repo;
    }

    /** Build a repository from already parsed entries, used by tests and by programmatic callers. */
    public static Repository of(Map<String, List<Meta>> entries) {
        Repository repo = new Repository(null);
        for (Map.Entry<String, List<Meta>> e : entries.entrySet()) {
            for (Meta m : e.getValue()) {
                repo.put(m);
            }
        }
        return repo;
    }

    private void scan(Path dir) throws IOException {
        if (!Files.isDirectory(dir)) {
            throw new IOException("metadata directory not found: " + dir);
        }
        List<Path> children = new ArrayList<>();
        try (DirectoryStream<Path> ds = Files.newDirectoryStream(dir)) {
            for (Path p : ds) {
                children.add(p);
            }
        }
        Collections.sort(children);
        for (Path p : children) {
            if (Files.isDirectory(p)) {
                scan(p);
            } else if (p.getFileName().toString().endsWith(".meta")) {
                parseFile(p);
            }
        }
    }

    private void parseFile(Path file) throws IOException {
        List<String> lines = Files.readAllLines(file, StandardCharsets.UTF_8);
        String inferredId = inferId(file);
        String id = inferredId;
        Version version = null;
        List<Dependency> deps = new ArrayList<>();
        List<String> excludes = new ArrayList<>();
        for (String rawLine : lines) {
            String line = rawLine.trim();
            if (line.isEmpty() || line.startsWith("#")) {
                continue;
            }
            int sp = firstSpace(line);
            String key = sp < 0 ? line : line.substring(0, sp);
            String rest = sp < 0 ? "" : line.substring(sp + 1).trim();
            switch (key) {
                case "id":
                    id = rest;
                    break;
                case "version":
                    version = Version.parse(rest);
                    break;
                case "dep":
                    deps.add(parseDep(rest));
                    break;
                case "exclude":
                    excludes.add(rest);
                    break;
                default:
                    throw new IOException("unknown metadata key '" + key + "' in " + file);
            }
        }
        if (version == null) {
            String fn = file.getFileName().toString();
            version = Version.parse(fn.substring(0, fn.length() - ".meta".length()));
        }
        if (id == null) {
            throw new IOException("cannot infer package id for " + file);
        }
        put(new Meta(id, version, deps, excludes));
    }

    private static Dependency parseDep(String rest) {
        String body = rest;
        boolean optional = false;
        if (body.endsWith(" opt") || body.endsWith(" optional")) {
            optional = true;
            body = body.substring(0, body.lastIndexOf(' ')).trim();
        }
        int sp = body.indexOf(' ');
        if (sp < 0) {
            return new Dependency(body, Interval.any(), optional);
        }
        String target = body.substring(0, sp).trim();
        String range = body.substring(sp + 1).trim();
        return new Dependency(target, range.isEmpty() ? Interval.any() : Interval.parse(range), optional);
    }

    private static int firstSpace(String s) {
        for (int i = 0; i < s.length(); i++) {
            if (s.charAt(i) == ' ') {
                return i;
            }
        }
        return -1;
    }

    /** Derive {@code group:artifact} from {@code <root>/<group>/<artifact>/<version>.meta}. */
    private String inferId(Path file) {
        Path p = file.getParent();          // artifact
        Path g = p == null ? null : p.getParent();  // group
        if (p == null || g == null) {
            return null;
        }
        Path base = root;
        String artifact = p.getFileName().toString();
        StringBuilder group = new StringBuilder(g.getFileName().toString());
        Path cur = g.getParent();
        while (cur != null && !cur.equals(base)) {
            group.insert(0, cur.getFileName().toString() + ".");
            cur = cur.getParent();
        }
        return group + ":" + artifact;
    }

    private void put(Meta m) {
        packages.computeIfAbsent(m.id(), k -> new TreeMap<>()).put(m.version(), m);
        byKey.put(key(m.id(), m.version()), m);
    }

    public static String key(String id, Version v) {
        return id + ":" + v;
    }

    /** All known package ids, in natural (lexicographic) order. */
    public List<String> packageIds() {
        return new ArrayList<>(packages.keySet());
    }

    public boolean hasPackage(String id) {
        return packages.containsKey(id);
    }

    /** Published versions of {@code id} in ascending natural order. */
    public List<Version> versionsOf(String id) {
        TreeMap<Version, Meta> m = packages.get(id);
        return m == null ? List.of() : new ArrayList<>(m.keySet());
    }

    /** Descending iteration over the versions of {@code id}, highest first. */
    public List<Version> versionsDescending(String id) {
        List<Version> vs = versionsOf(id);
        Collections.reverse(vs);
        return vs;
    }

    public Optional<Meta> meta(String id, Version v) {
        return Optional.ofNullable(byKey.get(key(id, v)));
    }

    /** Highest published version of {@code id}, or null when the package is unknown. */
    public Version highestKnown(String id) {
        TreeMap<Version, Meta> m = packages.get(id);
        return m == null || m.isEmpty() ? null : m.lastKey();
    }

    public int packageCount() {
        return packages.size();
    }

    public int versionCount() {
        return byKey.size();
    }
}
