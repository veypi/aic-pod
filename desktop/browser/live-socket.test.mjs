import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { serveBrowserLive } from './live-socket.mjs';
class Socket extends EventEmitter {
  chunks = [];
  setTimeout() {}
  cork() {}
  uncork() {}
  write(data) {
    this.chunks.push(Buffer.from(data));
    return true;
  }
  end(data) {
    this.write(data);
    this.destroy();
  }
  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    this.emit('close');
  }
  take() {
    const raw = Buffer.concat(this.chunks);
    this.chunks = [];
    return raw;
  }
  send(data) {
    this.emit('data', Buffer.from(JSON.stringify(data) + '\n'));
  }
}
test('slow viewer retains only the newest frame while input reaches the viewer scheduler immediately', async () => {
  const socket = new Socket(),
    events = [];
  let frame,
    close = 0,
    first;
  const held = new Promise((r) => (first = r));
  await serveBrowserLive(
    socket,
    {
      open: async (_req, emit) => {
        frame = emit;
        return {
          send: async (data) => {
            events.push(data);
            if (data.n === 1) await held;
          },
          close: () => close++,
        };
      },
    },
    {},
  );
  assert.equal(JSON.parse(socket.take()).result.ready, true);
  for (let n = 0; n < 100; n++) frame({ frame_seq: n }, Buffer.from([n]));
  assert.equal(socket.take().length, 0);
  socket.send({ type: 'input', data: { n: 1 } });
  socket.send({ type: 'input', data: { n: 2 } });
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(events, [{ n: 1 }, { n: 2 }]);
  first();
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(events, [{ n: 1 }, { n: 2 }]);
  socket.send({ type: 'pull' });
  const raw = socket.take(),
    size = raw.readUInt32BE();
  assert.equal(JSON.parse(raw.subarray(4, 4 + size)).metadata.frame_seq, 99);
  assert.deepEqual(raw.subarray(4 + size), Buffer.from([99]));
  socket.send({ type: 'pull' });
  assert.equal(socket.take().length, 0);
  frame({ frame_seq: 100 }, Buffer.from([100]));
  assert.ok(socket.take().length > 0);
  socket.destroy();
  assert.equal(close, 1);
});
test('disconnect during provider open disposes the late viewer', async () => {
  const socket = new Socket();
  let ready,
    closed = 0;
  const opening = serveBrowserLive(
    socket,
    { open: () => new Promise((r) => (ready = r)) },
    {},
  );
  socket.destroy();
  ready({ close: () => closed++ });
  await opening;
  assert.equal(closed, 1);
  assert.equal(socket.take().length, 0);
});
