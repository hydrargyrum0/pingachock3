import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// Pure logic - see classification.test.ts's comment on why db.ts still
// needs pointing at a throwaway dir before import.
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pingachock-bot-test-'));
process.env.DB_PATH = path.join(tmpDir, 'users.db');
process.env.SETTINGS_DB_PATH = path.join(tmpDir, 'settings.db');

const { chunkTargets, runWithConcurrency, aggregateScanProgress } = require('./pingachock-client') as typeof import('./pingachock-client');

test('chunkTargets splits into groups of the given size, last group short', () => {
  assert.deepEqual(chunkTargets([1, 2, 3, 4, 5], 2), [[1, 2], [3, 4], [5]]);
});

test('chunkTargets with a size >= the whole array returns one chunk', () => {
  assert.deepEqual(chunkTargets([1, 2, 3], 200), [[1, 2, 3]]);
});

test('chunkTargets of an empty array returns no chunks, not one empty chunk', () => {
  assert.deepEqual(chunkTargets([], 200), []);
});

test('aggregateScanProgress sums done/total across every chunk, including zero chunks', () => {
  assert.deepEqual(aggregateScanProgress([]), { done: 0, total: 0 });
  assert.deepEqual(
    aggregateScanProgress([
      { done: 200, total: 200 },
      { done: 87, total: 200 },
      { done: 0, total: 31 }
    ]),
    { done: 287, total: 431 }
  );
});

test('runWithConcurrency processes every item and preserves input order regardless of finish order', async () => {
  // Deliberately resolves out of order (item 0 is the slowest) - the
  // regression this guards: a naive Promise.all(items.map(worker)) with a
  // separate ad-hoc "limit" wrapper is an easy place to accidentally end up
  // pushing results in *completion* order instead of input order.
  const delays = [30, 0, 10];
  const order: number[] = [];
  const results = await runWithConcurrency([0, 1, 2], 3, async (i) => {
    await new Promise((resolve) => setTimeout(resolve, delays[i]));
    order.push(i);
    return i * 10;
  });
  assert.deepEqual(results, [0, 10, 20]);
  assert.deepEqual(order, [1, 2, 0], 'sanity check: they really did finish out of order');
});

test('runWithConcurrency never runs more than `limit` workers at once', async () => {
  let active = 0;
  let maxActive = 0;
  const items = Array.from({ length: 10 }, (_, i) => i);
  await runWithConcurrency(items, 3, async (i) => {
    active++;
    maxActive = Math.max(maxActive, active);
    await new Promise((resolve) => setTimeout(resolve, 5));
    active--;
    return i;
  });
  assert.ok(maxActive <= 3, `maxActive = ${maxActive}, want <= 3`);
});

test('runWithConcurrency with a limit above the item count still runs every item exactly once', async () => {
  const results = await runWithConcurrency([1, 2], 10, async (i) => i);
  assert.deepEqual(results, [1, 2]);
});
