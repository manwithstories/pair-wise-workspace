/**
 * 性能预算测试（延迟/吞吐敏感）。
 *
 * 两个关键的工程约束：
 *
 * 1) 必须串行执行，不能与其它测试文件并行（npm run test:perf 已带
 *    --test-concurrency=1）。延迟基准对 CPU/IO 争用极其敏感：
 *    同一份代码并行跑时 p99 会被拉到 30~40ms，串行约 1ms。
 *
 * 2) 每个用例必须在独立进程里测量。LevelDB 的后台压缩是异步的：
 *    前一个用例写入 5 万条事件后压缩仍在后台继续合并，这期间测下一个用例，
 *    p99 反映的是上一个用例的尾巴而非当前代码（实测会从 0.7ms 虚高到 15ms）。
 *    所以这里把每个基准用例 spawn 成独立进程，彻底隔离压缩尾流。
 *
 * 断言本身也做了工程化处理：预热排除冷启动、样本量足够（p99 至少几百个样本
 * 才有统计意义）、打印完整分布，便于区分「真回归」与「机器当时忙」。
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import nodePath from 'node:path';

const HERE = nodePath.dirname(fileURLToPath(import.meta.url));

/** 在独立进程里跑一个基准用例，返回其 stdout。 */
function runIsolated(name) {
  const r = spawnSync(
    process.execPath,
    ['--no-warnings', nodePath.join(HERE, 'perf-cases', `${name}.mjs`)],
    { encoding: 'utf8' },
  );
  if (r.status !== 0) {
    throw new Error(`基准用例 ${name} 执行失败:\n${r.stdout}\n${r.stderr}`);
  }
  return r.stdout.trim();
}

/** 解析基准进程打印的 JSON 指标行。 */
function metrics(stdout) {
  const line = stdout.split('\n').map((l) => l.trim()).filter((l) => l.startsWith('{')).pop();
  if (!line) throw new Error(`基准进程没有输出指标:\n${stdout}`);
  return JSON.parse(line);
}

test('性能预算：摄入 >= 2 万事件/秒', () => {
  const m = metrics(runIsolated('ingest'));
  console.log(
    `    [perf] ingest ${m.total} events in ${m.secs.toFixed(2)}s => ${m.rate.toFixed(0)} events/s ` +
    `(stored=${m.stored})`,
  );
  assert.equal(m.stored, m.total, '基准必须先验证条数正确，否则吞吐数字无意义');
  assert.ok(m.rate >= 20_000, `摄入吞吐 ${m.rate.toFixed(0)}/s 应 >= 20000/s`);
});

test('性能预算：参数化范围查询 p99 < 5ms', () => {
  const m = metrics(runIsolated('query'));
  console.log(
    `    [perf] query n=${m.n} p50=${m.p50}ms p90=${m.p90}ms p99=${m.p99}ms ` +
    `p99.9=${m.p999}ms max=${m.max}ms over5ms=${m.over5}/${m.n}`,
  );
  assert.ok(m.p99 < 5, `查询 p99 ${m.p99}ms 应 < 5ms`);
});

test('性能预算：写入峰值期快照读不被写者阻塞（50 并发生产者）', () => {
  const m = metrics(runIsolated('snapshot'));
  console.log(
    `    [perf] snapshot read under ${m.writers} concurrent writers: ` +
    `p50=${m.p50}ms p99=${m.p99}ms max=${m.max}ms spread(p99-p50)=${(m.p99 - m.p50).toFixed(3)}ms`,
  );
  assert.ok(m.writers >= 50, `应覆盖 50 个并发生产者，实际 ${m.writers}`);
  assert.ok(m.p99 < 5, `写入期快照读 p99 ${m.p99}ms 应 < 5ms`);
});

test('性能预算：压缩不得使读写 p99 恶化超基线两倍', () => {
  const m = metrics(runIsolated('compaction'));
  console.log(
    `    [perf] compaction baseline p50=${m.baselineP50}/p99=${m.baselineP99}ms ` +
    `during p50=${m.duringP50}/p99=${m.duringP99}ms ratio=${m.ratio}x ` +
    `scanned=${m.scanned} dropped=${m.dropped} withinBudget=${m.withinBudget}`,
  );
  // 压缩必须真的干活，否则这个比值没有意义
  assert.ok(m.scanned > 0 && m.dropped > 0, '压缩基准必须包含真实的淘汰工作量');
  assert.ok(m.ratio <= 2, `压缩期 p99 相对基线恶化 ${m.ratio}x，应 <= 2x`);
  assert.equal(m.withinBudget, true, '存储层自检应判定压缩在预算内');
});
