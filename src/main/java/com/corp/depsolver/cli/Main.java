package com.corp.depsolver.cli;

import com.corp.depsolver.Interval;
import com.corp.depsolver.LockFile;
import com.corp.depsolver.Repository;
import com.corp.depsolver.Resolver;
import com.corp.depsolver.Trace;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

/**
 * Terminal entry point. No GUI, no network: everything is read from a local metadata directory and a
 * plain requirements file.
 *
 * <pre>
 *   depsolver resolve &lt;metaDir&gt; &lt;requirementsFile&gt; [-o lock.txt] [--trace]
 *   depsolver replay  &lt;metaDir&gt; &lt;requirementsFile&gt; &lt;lockFile&gt; [--trace]
 *   depsolver interval &lt;rangeA&gt; &lt;rangeB&gt; ...
 * </pre>
 *
 * Requirements file syntax, one per line: {@code group:artifact <range>} or {@code group:artifact <range> opt}.
 */
public final class Main {

    public static void main(String[] args) throws IOException {
        if (args.length < 1) {
            usage();
            System.exit(2);
        }
        String cmd = args[0];
        boolean trace = List.of(args).contains("--trace");
        try {
            switch (cmd) {
                case "resolve":
                    resolve(args, trace);
                    break;
                case "replay":
                    replay(args, trace);
                    break;
                case "interval":
                    intersect(args);
                    break;
                default:
                    usage();
                    System.exit(2);
            }
        } catch (Resolver.Conflict e) {
            System.err.println(e.report());
            System.exit(1);
        } catch (LockFile.CorruptLockFile e) {
            System.err.println("LOCK FILE REJECTED: " + e.getMessage());
            System.exit(1);
        }
    }

    private static void resolve(String[] args, boolean trace)
            throws IOException, Resolver.Conflict, LockFile.CorruptLockFile {
        if (args.length < 3) {
            usage();
            System.exit(2);
        }
        Path metaDir = Path.of(args[1]);
        List<Resolver.Requirement> roots = readRequirements(Path.of(args[2]));
        Repository repo = Repository.load(metaDir);
        System.out.println("loaded " + repo.packageCount() + " packages / "
                + repo.versionCount() + " versions from " + metaDir);
        Trace t = trace ? Trace.enabled() : Trace.disabled();
        Resolver resolver = new Resolver(repo, t);
        long t0 = System.nanoTime();
        Resolver.Resolution res = resolver.resolve(roots);
        long ms = (System.nanoTime() - t0) / 1_000_000;
        System.out.println("resolved " + res.size() + " packages in " + ms + " ms");
        System.out.print(res);
        LockFile lock = LockFile.write(res);
        System.out.println("lock sha256 " + lock.sha256());
        for (int i = 0; i + 2 < args.length; i++) {
            if (args[i].equals("-o")) {
                lock.save(Path.of(args[i + 1]));
                System.out.println("lock written to " + args[i + 1]);
            }
        }
        if (trace) {
            System.out.println("--- trace ---");
            System.out.print(t.render());
        }
    }

    private static void replay(String[] args, boolean trace)
            throws IOException, Resolver.Conflict, LockFile.CorruptLockFile {
        if (args.length < 4) {
            usage();
            System.exit(2);
        }
        Path metaDir = Path.of(args[1]);
        List<Resolver.Requirement> roots = readRequirements(Path.of(args[2]));
        LockFile lock = LockFile.read(Path.of(args[3]));
        lock.verifyLineChecksums();
        System.out.println("lock verified, sha256 " + lock.sha256());
        Repository repo = Repository.load(metaDir);
        Trace t = trace ? Trace.enabled() : Trace.disabled();
        long t0 = System.nanoTime();
        Resolver.Resolution res = new Resolver(repo, t).replay(lock.pinnedVersions(), roots);
        long ms = (System.nanoTime() - t0) / 1_000_000;
        System.out.println("replayed " + res.size() + " packages in " + ms + " ms");
        System.out.print(res);
        if (trace) {
            System.out.println("--- trace ---");
            System.out.print(t.render());
        }
    }

    private static void intersect(String[] args) {
        List<Interval> parts = new ArrayList<>();
        for (int i = 1; i < args.length; i++) {
            if (!args[i].equals("--trace")) {
                parts.add(Interval.parse(args[i]));
            }
        }
        Interval acc = Interval.any();
        for (Interval iv : parts) {
            Interval next = acc.intersect(iv);
            if (next.isEmpty()) {
                System.out.println("EMPTY: mutually exclusive");
                return;
            }
            acc = next;
        }
        System.out.println("intersection = " + acc);
    }

    private static List<Resolver.Requirement> readRequirements(Path file) throws IOException {
        List<Resolver.Requirement> out = new ArrayList<>();
        for (String line : Files.readAllLines(file, StandardCharsets.UTF_8)) {
            String t = line.trim();
            if (t.isEmpty() || t.startsWith("#")) {
                continue;
            }
            boolean optional = false;
            if (t.endsWith(" opt") || t.endsWith(" optional")) {
                optional = true;
                t = t.substring(0, t.lastIndexOf(' ')).trim();
            }
            int sp = t.indexOf(' ');
            String pkg = sp < 0 ? t : t.substring(0, sp);
            String range = sp < 0 ? "(,)" : t.substring(sp + 1).trim();
            out.add(new Resolver.Requirement("<root>", pkg, Interval.parse(range), optional));
        }
        return out;
    }

    private static void usage() {
        System.err.println("usage:");
        System.err.println("  depsolver resolve <metaDir> <requirementsFile> [-o lock.txt] [--trace]");
        System.err.println("  depsolver replay  <metaDir> <requirementsFile> <lockFile> [--trace]");
        System.err.println("  depsolver interval <rangeA> <rangeB> ...");
    }
}
