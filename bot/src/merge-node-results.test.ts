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

const { mergeNodeResults } = require('./pingachock-client') as typeof import('./pingachock-client');

const NODE_ID = 'node-1';
const ICMP_SPEC = { kind: 'icmp' as const };

function icmpCheck(target: string, status: string, run?: any) {
  return { target, status, runs: run ? [{ node_id: NODE_ID, ...run }] : [] };
}

test('a finished, successful check reports success and is not pending', () => {
  const finished = [
    {
      spec: ICMP_SPEC,
      checks: [icmpCheck('1.1.1.1', 'completed', { result: { success: true, latency_ms: 12, raw: '{"packets_sent":4,"packets_recv":4}' } })]
    }
  ];
  const [out] = mergeNodeResults(['1.1.1.1'], NODE_ID, 'router1', finished);
  assert.equal(out.status, true);
  assert.equal(out.pending, false);
  assert.equal(out.ICMP, '4 из 4, 12 ms');
});

test('a finished, failed check (real "no reply" from the agent) reports failure and is not pending', () => {
  const finished = [
    {
      spec: ICMP_SPEC,
      checks: [icmpCheck('2.2.2.2', 'failed', { result: { success: false, error_message: 'no reply', raw: '{"packets_sent":4,"packets_recv":0}' } })]
    }
  ];
  const [out] = mergeNodeResults(['2.2.2.2'], NODE_ID, 'router1', finished);
  assert.equal(out.status, false);
  assert.equal(out.pending, false);
  assert.equal(out.ICMP, '0 из 4 (нет ответа)');
});

// This is the actual regression test for the real user report: a batch
// large/slow enough that some checks are still 'running' on the agent's
// side when the bot stops waiting on them used to be completely
// indistinguishable from a genuine "no reply" - both rendered as a bare
// ❌ with no ICMP detail at all (mergeNodeResults never set out.ICMP when
// there was no result, whether that was because the agent already said no
// or because it hadn't said anything yet).
test('a still-pending check (agent hasn\'t answered yet) is marked pending, not failed', () => {
  const finished = [{ spec: ICMP_SPEC, checks: [icmpCheck('3.3.3.3', 'running')] }];
  const [out] = mergeNodeResults(['3.3.3.3'], NODE_ID, 'router1', finished);
  assert.equal(out.pending, true, 'must be flagged pending so index.ts schedules a follow-up instead of reporting this as done');
  assert.equal(out.status, false, 'not yet known to have succeeded - but distinct from a real failure via .pending');
  assert.equal(out.ICMP, 'ещё выполняется');
});

test('finalAttempt=true on a check that is still pending gives up for good: a real failure, distinct wording, not pending anymore', () => {
  const finished = [{ spec: ICMP_SPEC, checks: [icmpCheck('4.4.4.4', 'pending')] }];
  const [out] = mergeNodeResults(['4.4.4.4'], NODE_ID, 'router1', finished, true);
  assert.equal(out.pending, false, 'no third pass is coming - must not keep claiming a follow-up is still on the way');
  assert.equal(out.status, false);
  assert.equal(out.ICMP, 'нет ответа от узла (не дождались)');
});

test('a genuinely finished check with no run for this node_id behaves exactly as before (no ICMP text, not pending)', () => {
  const finished = [{ spec: ICMP_SPEC, checks: [icmpCheck('5.5.5.5', 'failed')] }]; // no runs at all
  const [out] = mergeNodeResults(['5.5.5.5'], NODE_ID, 'router1', finished);
  assert.equal(out.pending, false);
  assert.equal(out.status, false);
  assert.equal(out.ICMP, undefined);
});

test('a pending TCP port check renders "ожидание", not "closed"', () => {
  const portSpec = { kind: 'port' as const, port: '443' };
  const finished = [{ spec: portSpec, checks: [icmpCheck('6.6.6.6', 'running')] }];
  const [out] = mergeNodeResults(['6.6.6.6'], NODE_ID, 'router1', finished);
  assert.equal(out.pending, true);
  assert.equal(out.port_443, 'ожидание');
});

test('mixed batch: one target done, one still pending - only the pending one is flagged', () => {
  const finished = [
    {
      spec: ICMP_SPEC,
      checks: [
        icmpCheck('7.7.7.7', 'completed', { result: { success: true, raw: '{"packets_sent":4,"packets_recv":4}' } }),
        icmpCheck('8.8.8.8', 'running')
      ]
    }
  ];
  const results = mergeNodeResults(['7.7.7.7', '8.8.8.8'], NODE_ID, 'router1', finished);
  const [done, waiting] = results;
  assert.equal(done.pending, false);
  assert.equal(done.status, true);
  assert.equal(waiting.pending, true);
  assert.equal(waiting.status, false);
});
