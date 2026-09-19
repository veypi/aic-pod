import test from 'node:test';
import assert from 'node:assert/strict';
import { createBrowserInputQueue } from './input-queue.mjs';
const tick = () => new Promise((resolve) => setImmediate(resolve));
const wheel = (dy, extra = {}) => ({
  kind: 'wheel',
  value: { x: 20, y: 30, dx: 0, dy, mode: 0, ...extra },
});
const move = (x, extra = {}) => ({
  kind: 'mouse',
  value: { type: 'mousemove', x, y: 30, buttons: 0, ...extra },
});
function heldQueue() {
  const seen = [],
    errors = [];
  let release;
  const held = new Promise((resolve) => {
    release = resolve;
  });
  const queue = createBrowserInputQueue(
    async (event) => {
      seen.push(event);
      if (seen.length === 1) await held;
    },
    (error) => errors.push(error),
  );
  return { queue, seen, errors, release };
}
test('high-rate wheel batches retain all displacement without queuing every CDP call', async () => {
  const { queue, seen, release } = heldQueue();
  const pending = [queue.send([wheel(10)])];
  for (let n = 0; n < 120; n++) pending.push(queue.send([wheel(2)]));
  assert.equal(seen.length, 3);
  release();
  await Promise.all(pending);
  assert.deepEqual(
    seen.map((event) => event.value.dy),
    [10, 2, 2, 236],
  );
});
test('motion preserves direction, target, modifiers and discrete event order', async () => {
  const { queue, seen, release } = heldQueue();
  const first = queue.send([{ kind: 'text', value: 'held' }]);
  const events = [
    wheel(10),
    wheel(20),
    wheel(-5),
    wheel(-5, { x: 50 }),
    wheel(-5, { x: 50, control: true }),
    wheel(-5, { x: 50, control: true, mode: 1 }),
    move(1),
    move(2),
    { kind: 'mouse', value: { type: 'mousedown', x: 2, y: 30, buttons: 1 } },
    move(3, { buttons: 1 }),
    move(4, { buttons: 1 }),
    { kind: 'mouse', value: { type: 'mouseup', x: 4, y: 30, buttons: 0 } },
    { kind: 'key', value: { type: 'keyDown', key: 'A' } },
    { kind: 'text', value: 'a' },
    { kind: 'key', value: { type: 'keyUp', key: 'A' } },
    { kind: 'reset' },
  ];
  const original = structuredClone(events);
  const pending = queue.send(events);
  release();
  await Promise.all([first, pending]);
  assert.deepEqual(events, original, 'caller-owned events must not be mutated');
  assert.deepEqual(seen.slice(1), [
    wheel(30),
    ...events.slice(2, 6),
    move(2),
    events[8],
    move(4, { buttons: 1 }),
    ...events.slice(11),
  ]);
});
test('closing rejects pending callers and never replays their input after a late dispatch', async () => {
  const { queue, seen, release } = heldQueue();
  const first = queue.send([wheel(10)]),
    second = queue.send([{ kind: 'text', value: 'discard' }]);
  const rejected = Promise.all(
    [first, second].map((p) => assert.rejects(p, { code: 'expired' })),
  );
  queue.close();
  await rejected;
  release();
  await tick();
  assert.equal(seen.length, 1);
});
test('non-coalescible overload closes the queue without partial batch dispatch', async () => {
  const { queue, seen, errors, release } = heldQueue();
  const first = assert.rejects(queue.send([wheel(1)]), { code: 'overloaded' });
  await assert.rejects(
    queue.send(
      Array.from({ length: 129 }, (_, i) => ({
        kind: 'text',
        value: String(i),
      })),
    ),
    { code: 'overloaded' },
  );
  await first;
  release();
  await tick();
  assert.equal(seen.length, 1);
  assert.equal(errors.length, 1);
});
test('coalescing respects the validated wheel magnitude limit', async () => {
  const { queue, seen, release } = heldQueue();
  const first = queue.send([wheel(1)]),
    pending = queue.send([wheel(80000), wheel(80000), wheel(10000)]);
  release();
  await Promise.all([first, pending]);
  assert.deepEqual(
    seen.map((event) => event.value.dy),
    [1, 80000, 90000],
  );
});
test('dispatch failure stops all queued input and reports it once', async () => {
  let reject;
  const events = [],
    errors = [];
  const queue = createBrowserInputQueue(
    (event) => {
      events.push(event);
      return new Promise((_resolve, fail) => {
        reject = fail;
      });
    },
    (error) => errors.push(error),
  );
  const first = queue.send([wheel(1)]),
    second = queue.send([{ kind: 'text', value: 'next' }]);
  const checks = Promise.all(
    [first, second].map((p) => assert.rejects(p, /CDP failed/)),
  );
  reject(new Error('CDP failed'));
  await checks;
  assert.equal(events.length, 1);
  assert.equal(errors.length, 1);
});

test('motion pipeline is bounded and a click waits for every earlier motion reply', async () => {
  const seen = [],
    replies = [];
  const queue = createBrowserInputQueue(
    (event) => {
      seen.push(event);
      return new Promise((resolve) => replies.push(resolve));
    },
    (error) => {
      throw error;
    },
  );
  const promises = [1, 2, 3, 4, 5].map((x) => queue.send([move(x)]));
  const click = {
    kind: 'mouse',
    value: { type: 'mousedown', x: 5, y: 30, buttons: 1 },
  };
  promises.push(queue.send([click]));
  assert.deepEqual(
    seen.map((e) => e.value.x),
    [1, 2, 3],
  );
  replies[2]();
  await tick();
  assert.equal(seen.length, 3);
  replies[0]();
  await tick();
  assert.deepEqual(
    seen.map((e) => e.value.x),
    [1, 2, 3, 5],
  );
  replies[1]();
  await tick();
  assert.equal(
    seen.length,
    4,
    'click must wait for the last in-flight movement',
  );
  replies[3]();
  await tick();
  assert.deepEqual(seen.at(-1), click);
  replies[4]();
  await Promise.all(promises);
});
