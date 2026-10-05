package lawfirm.vcs;

import java.io.IOException;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.List;

/**
 * A small machine-readable surface over the repository, used by the verification
 * script to compare against system git.
 *
 * <p>{@code git ls-tree} is the reference for tree structure, so
 * {@code vcs tree <commit>} prints the same lines in the same format:
 * {@code <mode> <type> <id>\t<path>}, one per tracked path.
 */
public final class Vcs {

    private Vcs() {
    }

    /**
     * Usage: {@code vcs <command> <repo> [arg]}. The repository always comes
     * first so the command scripts stay readable.
     */
    public static void main(String[] args) throws IOException {
        if (args.length < 2) {
            System.err.println("usage: vcs <tree|commits|objects|stage> <repo> [arg]");
            System.exit(2);
        }
        String what = args[0];
        Path repo = Paths.get(args[1]).toAbsolutePath().normalize();

        // "stage" only writes files, so it must also work in a directory that is
        // not a repository yet: the script uses it to lay out the system git
        // working tree as well.
        if (what.equals("stage")) {
            stage(repo, Integer.parseInt(args[2]));
            return;
        }

        try (Repository vcs = Repository.open(repo)) {
            switch (what) {
                case "tree" -> printTree(vcs, vcs.resolve(args[2]));
                case "objects" -> vcs.looseStore().listAll().forEach(id -> System.out.println(id.hex()));
                case "commits" -> {
                    // Oldest first, matching "git log --reverse".
                    List<ObjectId> history = vcs.history();
                    for (int i = history.size() - 1; i >= 0; i--) {
                        System.out.println(history.get(i).hex());
                    }
                }
                default -> {
                    System.err.println("unknown command: " + what);
                    System.exit(2);
                }
            }
        }
    }

    /**
     * Writes the working tree for a given phase of the verification script. Both
     * implementations are driven from here so the two sides cannot drift apart
     * because of an encoding difference in the shell.
     *
     * @param phase 1 is the initial layout, 2 edits, deletes and adds a file
     */
    private static void stage(Path root, int phase) throws IOException {
        switch (phase) {
            case 1 -> {
                write(root, "a.txt", "hello\n");
                write(root, "sub/b.txt", "world\n");
                write(root, "sub/nested/c.txt", "deep\n");
                write(root, "README.md", "# contracts\n");
                write(root, "合同/主协议.txt", "第一条　合同正文\n");
                write(root, "sub0.txt", "sibling of sub\n");
            }
            case 2 -> {
                write(root, "a.txt", "hello revised\n");
                java.nio.file.Files.deleteIfExists(root.resolve("sub/nested/c.txt"));
                write(root, "sub/added.txt", "added later\n");
            }
            default -> throw new IllegalArgumentException("unknown phase " + phase);
        }
    }

    private static void write(Path root, String rel, String content) throws IOException {
        Path p = root.resolve(rel);
        java.nio.file.Files.createDirectories(p.getParent());
        java.nio.file.Files.writeString(p, content, java.nio.charset.StandardCharsets.UTF_8);
    }

    private static void printTree(Repository vcs, ObjectId commit) throws IOException {
        TreeBuilder tree = TreeBuilder.load(vcs.treeOf(commit), vcs.looseStore());
        for (String path : tree.paths()) {
            TreeBuilder.Node node = tree.lookup(path);
            System.out.printf("%s %s %s\t%s%n",
                    padMode(node.mode()),
                    node.isTree() ? "tree" : "blob",
                    node.blobId().hex(),
                    path);
        }
    }

    /** Renders a mode the way git's ls-tree does: 100644, 100755, 120000, 040000. */
    private static String padMode(int mode) {
        String octal = Integer.toOctalString(mode);
        if (mode == GitObject.MODE_TREE) {
            return "040000";
        }
        return octal;
    }
}
