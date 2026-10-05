package lawfirm.vcs;

import java.nio.charset.StandardCharsets;
import java.util.Random;

/**
 * Generates contract-like text for tests.
 *
 * <p>The point is not realism for its own sake but the property that matters for
 * the chunker: long runs of repeated boilerplate punctuated by small varying
 * fields. That is what makes a clause-level edit show up as a handful of changed
 * chunks rather than a full resegmentation.
 */
final class Contracts {

    private static final String[] PARTIES = {
            "北京君合律师事务所", "上海锦天城律师事务所", "深圳金杜律师事务所", "杭州方元律师事务所"
    };

    private static final String[] SUBJECTS = {
            "技术开发合同", "股权转让协议", "设备采购合同", "保密协议", "借款合同", "建设工程施工合同"
    };

    private Contracts() {
    }

    static byte[] text(int clauses, long seed) {
        Random r = new Random(seed);
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < clauses; i++) {
            sb.append("第").append(i).append("条　").append(SUBJECTS[i % SUBJECTS.length]);
            sb.append("　甲方：").append(PARTIES[i % PARTIES.length]);
            sb.append("，乙方：").append(PARTIES[(i + 1) % PARTIES.length]);
            sb.append("，合同编号 HT-").append(2000 + i).append('-')
                    .append(String.format("%06d", r.nextInt(999_999)));
            sb.append("，签订日期 20").append(20 + i % 5).append("年").append(1 + i % 12)
                    .append("月").append(1 + i % 28).append("日。\n");
            sb.append("　双方应当按照本条约定的内容全面履行各自义务，任何一方不得擅自变更或者解除本合同。");
            sb.append("违约方应当赔偿守约方因此遭受的全部直接损失，间接损失另行协商确定。");
            sb.append("因本合同引起的争议，双方应当友好协商；协商不成的，提交")
                    .append(PARTIES[i % PARTIES.length]).append("所在地有管辖权的人民法院诉讼解决。\n");
            if (i % 5 == 4) {
                sb.append("　附件").append(i).append("：技术方案与交付清单（略）。")
                        .append("金额人民币 ").append(100_000 + (long) i * 37).append(" 元。\n");
            }
        }
        return sb.toString().getBytes(StandardCharsets.UTF_8);
    }

    /** Inserts a clause at roughly the given fraction of the document. */
    static byte[] amend(byte[] original, int clauseNumber) {
        String clause = "第" + clauseNumber + "条　【保密义务】　乙方不得向任何第三方披露本合同内容，"
                + "保密期限为合同终止后五年。\n";
        byte[] insert = clause.getBytes(StandardCharsets.UTF_8);
        int at = original.length / 3;
        byte[] out = new byte[original.length + insert.length];
        System.arraycopy(original, 0, out, 0, at);
        System.arraycopy(insert, 0, out, at, insert.length);
        System.arraycopy(original, at, out, at + insert.length, original.length - at);
        return out;
    }

    /** A 100 MB-ish contract, used by the size budget tests. */
    static byte[] large(int targetBytes) {
        byte[] unit = text(20_000, 99);
        byte[] out = new byte[targetBytes];
        int written = 0;
        int unitIndex = 0;
        while (written < targetBytes) {
            int n = Math.min(unit.length, targetBytes - written);
            System.arraycopy(unit, 0, out, written, n);
            written += n;
            unitIndex++;
        }
        return out;
    }
}
