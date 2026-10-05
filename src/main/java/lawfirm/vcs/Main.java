package lawfirm.vcs;

import java.io.IOException;
import java.io.PrintStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Set;

/**
 * Terminal entry point. No GUI, no daemon: every command parses its arguments,
 * does the work, prints, and exits.
 *
 * <p>Argument convention: flags first, then positional arguments. For commands
 * that act on a revision the revision comes first and an optional repository
 * directory second, so {@code vcs checkout HEAD~1 /srv/contracts} reads the way it
 * is written.
 */
public final class Main {

    private static final Set<String> VALUE_FLAGS = Set.of("-m", "-n");

    public static void main(String[] args) throws Exception {
        System.exit(run(args, System.out, System.err));
    }

    static int run(String[] args, PrintStream out, PrintStream err) throws IOException {
        if (args.length == 0) {
            usage(err);
            return 2;
        }
        String command = args[0];
        String[] rest = Arrays.copyOfRange(args, 1, args.length);
        return switch (command) {
            case "init" -> init(rest, out);
            case "add" -> add(rest, out, err);
            case "add-all" -> addAll(rest, out);
            case "rm" -> rm(rest, out, err);
            case "commit" -> commit(rest, out, err);
            case "log" -> log(rest, out);
            case "checkout" -> checkout(rest, out, err);
            case "cat-file" -> catFile(rest, out, err);
            case "dump-graph" -> dumpGraph(rest, out, err);
            case "status" -> status(rest, out);
            case "hash-object" -> hashObject(rest, out, err);
            case "chunk-stats" -> chunkStats(rest, out, err);
            case "help", "--help", "-h" -> {
                usage(out);
                yield 0;
            }
            default -> {
                err.println("unknown command: " + command);
                usage(err);
                yield 2;
            }
        };
    }

    /** Positional arguments, with flags and flag values removed. */
    private static List<String> positional(String[] args) {
        List<String> out = new ArrayList<>();
        for (int i = 0; i < args.length; i++) {
            String a = args[i];
            if (a.startsWith("-")) {
                if (VALUE_FLAGS.contains(a)) i++;
                continue;
            }
            out.add(a);
        }
        return out;
    }

    private static Path cwd() {
        return Paths.get(".").toAbsolutePath().normalize();
    }

    /**
     * A resolved invocation target: the revision named on the command line and
     * the repository directory, which may appear in either order.
     */
    private record Target(String revision, Path directory) {
    }

    private static Target target(List<String> pos) {
        String revision = null;
        Path directory = null;
        for (String a : pos) {
            if (revision == null && isRevision(a)) {
                revision = a;
                continue;
            }
            if (directory == null) {
                Path p = Paths.get(a).toAbsolutePath().normalize();
                if (Files.isDirectory(p)) directory = p;
            }
        }
        return new Target(revision == null ? "HEAD" : revision,
                directory == null ? cwd() : directory);
    }

    /** True when the argument names a revision rather than a directory. */
    private static boolean isRevision(String arg) {
        return arg.startsWith("HEAD") || (arg.length() == 40 && arg.chars().allMatch(c ->
                (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')));
    }

    /** The repository directory: the given path, else the current directory. */
    private static Path dir(List<String> pos, int index) {
        return index < pos.size() ? Paths.get(pos.get(index)).toAbsolutePath().normalize() : cwd();
    }

    private static int init(String[] rest, PrintStream out) throws IOException {
        Path root = dir(positional(rest), 0);
        Repository repo = Repository.init(root);
        out.println("initialized empty repository in " + repo.directory());
        return 0;
    }

    /**
     * Splits positionals into the repository directory and the paths to act on:
     * the first argument naming an existing directory is the repository, so
     * {@code vcs add a.txt} works from inside a repository and
     * {@code vcs add a.txt /srv/contracts} names one explicitly.
     */
    private static PathAndPaths splitDirectory(List<String> pos) {
        List<String> paths = new ArrayList<>();
        Path dir = null;
        for (String a : pos) {
            Path p = Paths.get(a).toAbsolutePath().normalize();
            if (dir == null && Files.isDirectory(p)) {
                dir = p;
            } else {
                paths.add(a);
            }
        }
        return new PathAndPaths(dir == null ? cwd() : dir, paths);
    }

    private record PathAndPaths(Path directory, List<String> paths) {
    }

    private static int add(String[] rest, PrintStream out, PrintStream err) throws IOException {
        PathAndPaths split = splitDirectory(positional(rest));
        if (split.paths().isEmpty()) {
            err.println("usage: vcs add <file>… [<dir>]");
            return 2;
        }
        try (Repository repo = Repository.open(split.directory())) {
            for (String f : split.paths()) {
                ObjectId id = repo.add(repo.root().resolve(f));
                out.println("added " + f + " " + id.hex());
            }
        }
        return 0;
    }

    private static int addAll(String[] rest, PrintStream out) throws IOException {
        try (Repository repo = Repository.open(dir(positional(rest), 0))) {
            out.println("staged " + repo.addAll() + " file(s)");
        }
        return 0;
    }

    private static int rm(String[] rest, PrintStream out, PrintStream err) throws IOException {
        PathAndPaths split = splitDirectory(positional(rest));
        if (split.paths().isEmpty()) {
            err.println("usage: vcs rm <path>… [<dir>]");
            return 2;
        }
        try (Repository repo = Repository.open(split.directory())) {
            for (String path : split.paths()) {
                repo.remove(path);
                out.println("removed " + path);
            }
        }
        return 0;
    }

    private static int commit(String[] rest, PrintStream out, PrintStream err) throws IOException {
        String message = flagValue(rest, "-m");
        if (message == null) {
            err.println("usage: vcs commit [-m \"<message>\"] [<dir>]");
            return 2;
        }
        try (Repository repo = Repository.open(dir(positional(rest), 0))) {
            ObjectId id = repo.commit(message, Identity.defaultIdentity());
            out.println("[" + repo.branch() + " " + id.abbrev(7) + "] " + message);
            out.println(" " + repo.history().size() + " commit(s)");
        }
        return 0;
    }

    private static String flagValue(String[] args, String flag) {
        for (int i = 0; i < args.length - 1; i++) {
            if (args[i].equals(flag)) return args[i + 1];
        }
        return null;
    }

    private static int log(String[] rest, PrintStream out) throws IOException {
        String n = flagValue(rest, "-n");
        int limit = n == null ? 20 : Integer.parseInt(n);
        try (Repository repo = Repository.open(dir(positional(rest), 0))) {
            out.print(repo.log(limit));
        }
        return 0;
    }

    private static int checkout(String[] rest, PrintStream out, PrintStream err) throws IOException {
        Target t = target(positional(rest));
        try (Repository repo = Repository.open(t.directory())) {
            out.println("checked out " + t.revision()
                    + " (" + repo.checkout(t.revision()) + " file(s))");
        }
        return 0;
    }

    private static int catFile(String[] rest, PrintStream out, PrintStream err) throws IOException {
        Target t = target(positional(rest));
        try (Repository repo = Repository.open(t.directory())) {
            out.print(repo.catFile(t.revision()));
        }
        return 0;
    }

    private static int dumpGraph(String[] rest, PrintStream out, PrintStream err) throws IOException {
        List<String> pos = positional(rest);
        Target t = target(positional(rest));
        try (Repository repo = Repository.open(t.directory())) {
            new DumpGraph(repo).print(repo.resolve(t.revision()), out);
        }
        return 0;
    }

    private static int status(String[] rest, PrintStream out) throws IOException {
        try (Repository repo = Repository.open(dir(positional(rest), 0))) {
            List<String> s = repo.status();
            if (s.isEmpty()) {
                out.println("clean");
            } else {
                s.forEach(out::println);
            }
        }
        return 0;
    }

    private static int hashObject(String[] rest, PrintStream out, PrintStream err) throws IOException {
        List<String> pos = positional(rest);
        if (pos.isEmpty()) {
            err.println("usage: vcs hash-object <file>");
            return 2;
        }
        out.println(GitObject.blob(Files.readAllBytes(Paths.get(pos.get(0)))).id().hex());
        return 0;
    }

    private static int chunkStats(String[] rest, PrintStream out, PrintStream err) throws IOException {
        List<String> pos = positional(rest);
        if (pos.isEmpty()) {
            err.println("usage: vcs chunk-stats <file>");
            return 2;
        }
        byte[] content = Files.readAllBytes(Paths.get(pos.get(0)));
        RabinChunker chunker = new RabinChunker();
        List<RabinChunker.Chunk> chunks = chunker.chunk(content);
        out.println("bytes    " + content.length);
        out.println("chunks   " + chunks.size());
        out.printf("average  %.0f%n", content.length / (double) Math.max(chunks.size(), 1));
        out.println("min      " + chunks.stream().mapToLong(RabinChunker.Chunk::length).min().orElse(0));
        out.println("max      " + chunks.stream().mapToLong(RabinChunker.Chunk::length).max().orElse(0));
        return 0;
    }

    private static void usage(PrintStream out) {
        out.println("lawfirm contract version store - git compatible objects, chunked large blobs");
        out.println();
        out.println("  vcs init [<dir>]                   create or open a repository");
        out.println("  vcs add <file>…                    stage individual files");
        out.println("  vcs add-all [<dir>]                stage every file under the tree");
        out.println("  vcs rm <path>                      drop a path from the staged tree");
        out.println("  vcs commit -m \"<message>\" [<dir>] record a commit over the staged tree");
        out.println("  vcs log [-n N] [<dir>]             show recent commits");
        out.println("  vcs checkout <revision> [<dir>]    materialise a tree byte for byte");
        out.println("  vcs cat-file <revision> [<dir>]    readable dump of one object");
        out.println("  vcs dump-graph [<revision>] [<dir>] commit graph in topological order");
        out.println("  vcs status [<dir>]                 staged differences against HEAD");
        out.println("  vcs hash-object <file>             the git blob id this file would get");
        out.println("  vcs chunk-stats <file>             Rabin chunking summary");
    }
}
