package lawfirm.vcs;

import java.time.Instant;
import java.time.ZoneOffset;
import java.time.ZonedDateTime;
import java.time.format.DateTimeFormatter;
import java.util.Locale;

/**
 * Author/committer identity lines in git's "Name &lt;email&gt; epoch tz" format.
 *
 * <p>The same string is used for author and committer by default, which is what
 * {@code git commit} does when GIT_AUTHOR_* and GIT_COMMITTER_* agree.
 */
public record Identity(String name, String email, long epochSecond, int tzOffsetMinutes) {

    private static final DateTimeFormatter STAMP =
            DateTimeFormatter.ofPattern("yyyy-MM-dd HH:mm:ss", Locale.ROOT);

    public static Identity of(String name, String email, long epochSecond, int tzOffsetMinutes) {
        return new Identity(name, email, epochSecond, tzOffsetMinutes);
    }

    public static Identity now(String name, String email) {
        return new Identity(name, email, Instant.now().getEpochSecond(), systemTz());
    }

    /**
     * Identity for this machine, matching what plain {@code git commit} produces
     * for a shell that exports no {@code GIT_*} overrides.
     *
     * <p>{@code GIT_AUTHOR_DATE} and {@code GIT_COMMITTER_DATE} are honoured so a
     * scripted sequence can be replayed reproducibly. Git accepts either a raw
     * epoch or the {@code "<epoch> <tz>"} form; the epoch is taken verbatim,
     * because it already denotes an absolute instant and the trailing offset is
     * only how git chooses to render it.
     */
    public static Identity defaultIdentity() {
        return fromEnvironment();
    }

    private static Identity fromEnvironment() {
        String name = env("GIT_AUTHOR_NAME", System.getProperty("user.name", "lawfirm-it"));
        String email = env("GIT_AUTHOR_EMAIL", System.getProperty("user.email", "lawfirm-it@localhost"));
        long epoch = System.currentTimeMillis() / 1000L;
        int tz = systemTz();

        String rawDate = System.getenv("GIT_AUTHOR_DATE");
        if (rawDate != null && !rawDate.isBlank()) {
            Parsed parsed = parseDate(rawDate);
            if (parsed != null) {
                epoch = parsed.epoch();
                tz = parsed.tzMinutes();
            }
        }
        String rawTz = System.getenv("GIT_AUTHOR_TZ");
        if (rawTz != null && !rawTz.isBlank()) {
            tz = parseTz(rawTz);
        }
        return new Identity(name, email, epoch, tz);
    }

    private static String env(String key, String fallback) {
        String v = System.getenv(key);
        return v == null || v.isEmpty() ? fallback : v;
    }

    private record Parsed(long epoch, int tzMinutes) {
    }

    /**
     * Parses git's date forms: a bare epoch, "epoch +0800", or an ISO-like
     * "yyyy-MM-dd HH:mm:ss +0800". Returns null when the input is unrecognised.
     */
    private static Parsed parseDate(String raw) {
        String text = raw.trim();
        // "<epoch> <+hhmm>"
        int space = text.indexOf(' ');
        if (space > 0) {
            String head = text.substring(0, space);
            String tail = text.substring(space + 1).trim();
            try {
                long epoch = Long.parseLong(head);
                return new Parsed(epoch, tail.isEmpty() ? systemTz() : parseTz(tail));
            } catch (NumberFormatException ignored) {
                // fall through to the ISO form
            }
        }
        try {
            return new Parsed(Long.parseLong(text), systemTz());
        } catch (NumberFormatException ignored) {
            // not an epoch
        }
        try {
            DateTimeFormatter formatter =
                    DateTimeFormatter.ofPattern("yyyy-MM-dd HH:mm:ss", Locale.ROOT);
            ZonedDateTime parsed = ZonedDateTime.parse(text, formatter);
            return new Parsed(parsed.toEpochSecond(),
                    parsed.getOffset().getTotalSeconds() / 60);
        } catch (RuntimeException ignored) {
            return null;
        }
    }

    private static int systemTz() {
        return ZoneOffset.systemDefault().getRules()
                .getOffset(Instant.now()).getTotalSeconds() / 60;
    }

    /** Parses "+0800", "-0500" or "+08:00" into minutes. */
    static int parseTz(String raw) {
        String v = raw.trim();
        boolean neg = v.startsWith("-");
        if (neg || v.startsWith("+")) {
            v = v.substring(1);
        }
        v = v.replace(":", "");
        int hours;
        int mins = 0;
        if (v.length() == 4) {
            hours = Integer.parseInt(v.substring(0, 2));
            mins = Integer.parseInt(v.substring(2));
        } else {
            hours = Integer.parseInt(v);
        }
        return (neg ? -1 : 1) * (hours * 60 + mins);
    }

    /** The exact bytes git hashes: "Name <email> 1234567890 +0800". */
    public String canonical() {
        return name + " <" + email + "> " + epochSecond + " " + formatTz(tzOffsetMinutes);
    }

    private static String formatTz(int minutes) {
        char sign = minutes < 0 ? '-' : '+';
        int abs = Math.abs(minutes);
        return String.format("%c%02d%02d", sign, abs / 60, abs % 60);
    }

    public String isoDate() {
        return STAMP.format(ZonedDateTime.ofInstant(Instant.ofEpochSecond(epochSecond),
                ZoneOffset.ofTotalSeconds(tzOffsetMinutes * 60)));
    }
}
