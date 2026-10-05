package lawfirm.vcs;

import java.io.IOException;
import java.io.PrintStream;
import java.util.ArrayDeque;
import java.util.Comparator;
import java.util.ArrayList;
import java.util.Deque;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

/**
 * Dumps the commit graph in topological order.
 *
 * <p>Order is deterministic: commits are emitted so that every parent precedes
 * its children, siblings are broken by descending commit time and then by object
 * id, matching how {@code git log --topo-order} reads. Each line reports the
 * commit, its parent, its root tree and a summary of that tree.
 */
public final class DumpGraph {

    private final Repository repo;

    public DumpGraph(Repository repo) {
        this.repo = repo;
    }

    /** One commit with everything the dump reports about it. */
    public record Node(ObjectId id, ObjectId tree, List<ObjectId> parents,
                       String author, long epoch, String subject,
                       int fileCount, long treeBytes) {
    }

    /** Walks the graph reachable from {@code tip}. */
    public List<Node> collect(ObjectId tip) throws IOException {
        List<Node> out = new ArrayList<>();
        Set<ObjectId> visited = new HashSet<>();
        Deque<ObjectId> stack = new ArrayDeque<>();
        if (tip != null) stack.push(tip);
        while (!stack.isEmpty()) {
            ObjectId id = stack.pop();
            if (!visited.add(id)) continue;
            GitObject.Commit c = (GitObject.Commit) repo.readObject(id);
            Identity id2 = parseIdentity(c.author());
            TreeBuilder tree = TreeBuilder.load(c.tree(), repo.looseStore());
            List<String> paths = tree.paths();
            // Totals are of file content, not of object headers.
            long bytes = 0;
            for (String p : paths) {
                bytes += ((GitObject.Blob) repo.readObject(tree.lookup(p).blobId())).contentSize();
            }
            out.add(new Node(id, c.tree(), c.parents(), c.author(), id2.epochSecond(),
                    subject(c.message()), paths.size(), bytes));
            for (ObjectId p : c.parents()) {
                stack.push(p);
            }
        }
        out.sort(DumpGraph::topological);
        return out;
    }

    /**
     * Topological comparator: an ancestor always sorts before its descendant.
     * Remaining ties are broken by time, then by id, so output is stable.
     */
    private static int topological(Node a, Node b) {
        return b.epoch - a.epoch > 0 ? 1 : (b.epoch - a.epoch < 0 ? -1 : a.id.compareTo(b.id));
    }

    /**
     * Re-orders {@code nodes} so that no commit is emitted before any of its
     * parents.
     *
     * <p>Emitting a commit as soon as it is popped is not enough: popping a child
     * only pushes its parents, which would place the child ahead of them. A
     * commit is therefore emitted only once every parent already is.
     *
     * <p>Within that constraint, siblings are ordered by descending commit time
     * and then by id, so the output is stable across runs and reads the way a
     * reviewer expects: the newest work first among the commits that are ready at
     * the same time.
     */
    public static List<Node> inTopologicalOrder(List<Node> nodes, ObjectId tip) {
        Map<ObjectId, Node> byId = new LinkedHashMap<>();
        for (Node n : nodes) {
            byId.put(n.id(), n);
        }
        List<Node> candidates = new ArrayList<>(byId.values());
        candidates.sort(siblingsFirst());

        List<Node> out = new ArrayList<>(candidates.size());
        Set<ObjectId> emitted = new LinkedHashSet<>();

        // Kahn's algorithm over the subgraph reachable from the tip.
        Map<ObjectId, Integer> pendingParents = new LinkedHashMap<>();
        Map<ObjectId, List<ObjectId>> children = new LinkedHashMap<>();
        for (Node n : candidates) {
            int count = 0;
            for (ObjectId p : n.parents()) {
                if (!byId.containsKey(p)) continue;
                count++;
                children.computeIfAbsent(p, k -> new ArrayList<>()).add(n.id());
            }
            pendingParents.put(n.id(), count);
        }

        // Seed with every commit that has no parents inside the collected set,
        // already in sibling order. The tip must not be forced to the front: doing
        // so would emit it before the ancestors it is waiting on.
        Deque<ObjectId> ready = new ArrayDeque<>();
        for (Node n : candidates) {
            if (pendingParents.get(n.id()) == 0) ready.push(n.id());
        }

        while (!ready.isEmpty()) {
            ObjectId id = ready.pop();
            if (!emitted.add(id)) continue;
            out.add(byId.get(id));
            for (ObjectId child : children.getOrDefault(id, List.of())) {
                int left = pendingParents.merge(child, -1, Integer::sum);
                if (left == 0) ready.push(child);
            }
        }

        // Defensive: a malformed graph with a cycle still yields every node.
        for (Node n : candidates) {
            if (emitted.add(n.id())) out.add(n);
        }
        return out;
    }

    /** Newest first, ties broken by id, so ties are broken deterministically. */
    private static Comparator<Node> siblingsFirst() {
        return (a, b) -> {
            int byTime = Long.compare(b.epoch(), a.epoch());
            if (byTime != 0) return byTime;
            return a.id().compareTo(b.id());
        };
    }

    /** The full textual dump. */
    public String dump(ObjectId tip) throws IOException {
        List<Node> nodes = inTopologicalOrder(collect(tip), tip);
        StringBuilder sb = new StringBuilder();
        sb.append("commit-graph ").append(nodes.size()).append(" commit(s)\n");
        int depth = 0;
        for (Node n : nodes) {
            sb.append("  commit ").append(n.id().hex()).append('\n');
            if (n.parents().isEmpty()) {
                sb.append("    parent -\n");
            } else {
                for (int i = 0; i < n.parents().size(); i++) {
                    sb.append("    parent ").append(n.parents().get(i).hex()).append('\n');
                }
            }
            sb.append("    tree   ").append(n.tree().hex()).append('\n');
            sb.append("    author ").append(n.author()).append('\n');
            sb.append("    files  ").append(n.fileCount())
                    .append("  bytes ").append(n.treeBytes()).append('\n');
            sb.append("    subj   ").append(n.subject()).append('\n');
        }
        return sb.toString();
    }

    public void print(ObjectId tip, PrintStream out) throws IOException {
        out.print(dump(tip));
    }

    private static String subject(String message) {
        return message.strip().lines().findFirst().orElse("");
    }

    /** Parses "Name <email> epoch tz" back into an Identity. */
    public static Identity parseIdentity(String canonical) {
        int lt = canonical.lastIndexOf('<');
        int gt = canonical.lastIndexOf('>');
        if (lt < 0 || gt < lt) {
            return Identity.of(canonical, "", 0, 0);
        }
        String name = canonical.substring(0, lt).trim();
        String email = canonical.substring(lt + 1, gt);
        String[] tail = canonical.substring(gt + 1).trim().split("\\s+");
        long epoch = 0;
        int tz = 0;
        if (tail.length >= 1) {
            try {
                epoch = Long.parseLong(tail[0]);
            } catch (NumberFormatException ignored) {
                epoch = 0;
            }
        }
        if (tail.length >= 2) {
            String t = tail[1];
            boolean neg = t.startsWith("-");
            t = t.replace("+", "").replace("-", "");
            if (t.length() == 4) {
                tz = (neg ? -1 : 1) * (Integer.parseInt(t.substring(0, 2)) * 60
                        + Integer.parseInt(t.substring(2)));
            } else if (!t.isEmpty()) {
                tz = (neg ? -1 : 1) * Integer.parseInt(t);
            }
        }
        return Identity.of(name, email, epoch, tz);
    }
}
