package io.corp.artifactres;

import io.corp.artifactres.core.LockFile;
import io.corp.artifactres.core.MetadataDir;
import io.corp.artifactres.core.Resolver;
import io.corp.artifactres.core.Trace;
import java.io.IOException;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * Terminal entry point. No GUI, no network: everything comes from a local metadata
 * directory and a root requirement list.
 *
 * <pre>
 *   java -cp ... io.corp.artifactres.Main &lt;metadataDir&gt; [options]
 *
 *   --root  pkg:range      declare a root requirement (repeatable, required)
 *   --lock  file           write the resolved tree to this lock file
 *   --replay file          verify and replay a lock file instead of solving
 *   --trace                print every decision, in propagation order
 * </pre>
 *
 * <p>The metadata directory holds one {@code <groupId>__<artifactId>.dep} file per
 * package; see {@link MetadataDir} for the line format.
 */
public final class Main {

    private Main() {
    }

    public static void main(String[] args) {
        if (args.length == 0) {
            usage();
            System.exit(2);
        }

        Path metadataDir = Path.of(args[0]);
        String lockOut = null;
        String replayIn = null;
        boolean traceOn = false;
        List<String> roots = new java.util.ArrayList<>();

        for (int i = 1; i < args.length; i++) {
            switch (args[i]) {
                case "--root" -> {
                    if (++i >= args.length) {
                        System.err.println("--root needs a value like com.acme:app:[1.0,2.0)");
                        System.exit(2);
                    }
                    roots.add(args[i]);
                }
                case "--lock" -> {
                    if (++i >= args.length) {
                        System.err.println("--lock needs a path");
                        System.exit(2);
                    }
                    lockOut = args[i];
                }
                case "--replay" -> {
                    if (++i >= args.length) {
                        System.err.println("--replay needs a path");
                        System.exit(2);
                    }
                    replayIn = args[i];
                }
                case "--trace" -> traceOn = true;
                case "-h", "--help" -> {
                    usage();
                    return;
                }
                default -> {
                    System.err.println("unknown option: " + args[i]);
                    usage();
                    System.exit(2);
                }
            }
        }

        try {
            TreeMap<String, Resolver.Module> repo = MetadataDir.load(metadataDir);
            System.out.printf("loaded %d package(s) from %s%n", repo.size(), metadataDir);

            if (replayIn != null) {
                replay(repo, Path.of(replayIn));
                return;
            }
            if (roots.isEmpty()) {
                System.err.println("at least one --root is required");
                System.exit(2);
            }
            resolve(repo, roots, lockOut, traceOn);

        } catch (IOException e) {
            System.err.println("io error: " + e.getMessage());
            System.exit(1);
        } catch (Resolver.Conflict c) {
            // The conflict message names the two rival constraints and the minimal repair.
            System.err.println();
            System.err.println(c.getMessage());
            System.exit(1);
        } catch (IllegalArgumentException e) {
            System.err.println("invalid input: " + e.getMessage());
            System.exit(2);
        }
    }

    private static void resolve(TreeMap<String, Resolver.Module> repo, List<String> roots,
                                String lockOut, boolean traceOn) throws IOException {
        Trace trace = traceOn ? Trace.enabled() : Trace.disabled();
        Resolver resolver = new Resolver(repo, trace);

        long t0 = System.nanoTime();
        for (String spec : roots) {
            int colon = spec.lastIndexOf(':');
            if (colon < 0) {
                throw new IllegalArgumentException("bad --root '" + spec + "', expected pkg:range");
            }
            String target = spec.substring(0, colon);
            String range = spec.substring(colon + 1);
            // "[1.0,1.0)" excludes 1.0, so it matches nothing -- a natural typo. Say so
            // rather than reporting a confusing empty-window conflict further down.
            if (range.endsWith(")") && range.startsWith("[")) {
                String body = range.substring(1, range.length() - 1);
                int comma = body.indexOf(',');
                if (comma > 0 && body.substring(0, comma).trim()
                        .equals(body.substring(comma + 1).trim())) {
                    throw new IllegalArgumentException(
                            "root range '" + range + "' for " + target + " is empty: a half-open "
                                    + "upper bound excludes the version itself. Use '[" + body.trim()
                                    + "]' to pin exactly " + body.substring(0, comma).trim());
                }
            }
            resolver.require(target, range);
        }
        Resolver.Resolution resolution = resolver.resolve();
        long ms = (System.nanoTime() - t0) / 1_000_000;

        System.out.printf("%nresolved %d package(s) in %dms (backtrack depth K=%d)%n%n",
                resolution.size(), ms, resolver.lastDepth());
        for (Map.Entry<String, io.corp.artifactres.core.Version> e : resolution.selected().entrySet()) {
            System.out.printf("  %-40s %s%n", e.getKey(), e.getValue());
        }

        for (String note : resolution.notes()) {
            System.out.println("  note: " + note);
        }

        if (lockOut != null) {
            LockFile lock = LockFile.of(resolution);
            lock.write(Path.of(lockOut));
            System.out.printf("%nwrote %s (sha256=%s)%n", lockOut, lock.sha256());
        }
        if (traceOn) {
            System.out.println();
            trace.dump(System.out);
        }
    }

    private static void replay(TreeMap<String, Resolver.Module> repo, Path lockPath)
            throws IOException {
        long t0 = System.nanoTime();
        LockFile lock = LockFile.read(lockPath);
        long readMs = (System.nanoTime() - t0) / 1_000_000;

        System.out.printf("verified %s (sha256=%s) in %dms%n",
                lockPath, lock.sha256(), readMs);
        System.out.printf("pinned %d package(s)%n", lock.size());

        t0 = System.nanoTime();
        LockFile.Replay replay = lock.replay(new Resolver(repo, Trace.disabled()));
        long ms = (System.nanoTime() - t0) / 1_000_000;

        for (String v : replay.violations) {
            System.out.println("  violation: " + v);
        }
        System.out.printf("replay %s in %dms%n",
                replay.isConsistent() ? "consistent" : "INCONSISTENT", ms);
        if (!replay.isConsistent()) {
            System.exit(1);
        }
    }

    private static void usage() {
        System.err.println(String.join(System.lineSeparator(),
                "usage: Main <metadataDir> [options]",
                "",
                "  --root  pkg:range   declare a root requirement (repeatable)",
                "  --lock  file        write the resolved tree to a lock file",
                "  --replay file       verify and replay a lock file instead of solving",
                "  --trace             print every decision in propagation order",
                "",
                "example:",
                "  Main ./repo --root 'com.acme:app:[1.0,2.0)' --lock ./resolver.lock --trace",
                "",
                "metadata layout: one <groupId>__<artifactId>.dep file per package, e.g.",
                "  # com.acme core",
                "  version 1.4.0",
                "  version 1.5.0 -> com.acme:io [1.4,2.0)",
                "  version 1.5.0 ! com.acme:legacy"));
    }
}
