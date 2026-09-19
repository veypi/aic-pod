import test from 'node:test';
import assert from 'node:assert/strict';
import { createDirectBrowser } from './direct.mjs';
import { createBrowserDispatcher } from './dispatch.mjs';
function fixture() {
  const calls = [],
    watches = new Set(),
    tabs = [
      {
        id: 11,
        title: 'AI-created',
        url: 'https://example.test',
        viewport: { width: 1280, height: 720 },
      },
    ];
  let resets = 0;
  const adapter = {
    tabs: {
      list: async () => tabs,
      get: async (id) => {
        const tab = tabs.find((t) => t.id === id);
        if (!tab) throw Error('closed');
        return tab;
      },
      remove: async () => tabs.splice(0),
    },
    cdp: {
      send: async (...args) => {
        calls.push(args);
        return {};
      },
    },
    invalidateFrame: (id) => calls.push(['invalidate', id]),
    inputFor: () => ({
      reset: () => resets++,
      wheel: async (value) => calls.push(['wheel', value]),
      text: async (value) => calls.push(['text', value]),
    }),
    watchFrames: (_id, frame) => {
      watches.add(frame);
      return () => watches.delete(frame);
    },
  };
  const browser = createDirectBrowser(adapter);
  const req = (method, args = {}, session = 's', connection = 'c') => ({
    method,
    args,
    session_id: session,
    connection_id: connection,
    deadline_ms: Date.now() + 5000,
  });
  const run = (method, args, session, connection) =>
    browser.run(req(method, args, session, connection));
  const open = async (session = 's', connection = 'c', frame = () => {}) => {
    const target = (await run('list')).windows[0];
    return browser.open(
      req(
        'view',
        { id: target.id, surface_epoch: target.surface_epoch },
        session,
        connection,
      ),
      frame,
    );
  };
  return {
    browser,
    adapter,
    run,
    open,
    req,
    calls,
    tabs,
    watches,
    get resets() {
      return resets;
    },
  };
}
test('human viewers and AI can act concurrently without acquiring control or observing a frame', async () => {
  const f = fixture();
  let finish;
  const dispatcher = createBrowserDispatcher(
    f.adapter,
    () => new Promise((r) => (finish = r)),
    async () => {},
  );
  try {
    const ai = dispatcher(
      { grantedLevel: 3 },
      { argv: ['eval', '--code', '1', '--after', 'none'] },
    );
    await new Promise((r) => setImmediate(r));
    const a = await f.open(),
      b = await f.open('s2', 'c2');
    for (let i = 0; i < 20; i++)
      await a.send({
        events: [{ kind: 'wheel', value: { x: 40, y: 40, dy: 10 } }],
      });
    await b.send({ events: [{ kind: 'text', value: '你好' }] });
    const target = (await f.run('list')).windows[0];
    await f.run('reload', {
      id: target.id,
      surface_epoch: target.surface_epoch,
    });
    assert.equal(f.calls.filter((c) => c[0] === 'wheel').length, 20);
    assert.equal(f.calls.filter((c) => c[0] === 'text').length, 1);
    assert.equal(f.adapter.control, undefined);
    finish({ state: 'completed' });
    await ai;
    await f.run('session.release');
    assert.equal(f.resets, 1);
    assert.equal(f.tabs.length, 1);
    await assert.rejects(
      a.send({ events: [{ kind: 'text', value: 'late' }] }),
      { code: 'expired' },
    );
    await b.send({ events: [{ kind: 'text', value: 'still connected' }] });
    await f.run('connection.release', {}, 's2', 'c2');
    assert.equal(f.resets, 2);
    assert.equal(f.watches.size, 0);
  } finally {
    finish?.({});
    f.browser.dispose();
    dispatcher.dispose();
  }
});
test('window identity and validated input survive removal of stale-frame gating', async () => {
  const f = fixture();
  try {
    const frames = [],
      v = await f.open('s', 'c', (...frame) => frames.push(frame));
    for (const frame of f.watches) frame(Buffer.from('jpeg'));
    assert.equal(frames[0][0].viewport.width, 1280);
    await assert.rejects(
      v.send({ events: [{ kind: 'execute', value: 'no' }] }),
      { code: 'invalid_argument' },
    );
    await assert.rejects(
      v.send({
        events: [{ kind: 'mouse', value: { type: 'mousemove', x: Infinity } }],
      }),
      { code: 'invalid_argument' },
    );
    await assert.rejects(
      f.browser.open(
        f.req('view', { id: '11', surface_epoch: 'old' }),
        () => {},
      ),
      { code: 'stale_surface' },
    );
    await assert.rejects(f.run('control.acquire', {}), { code: 'unsupported' });
    v.close();
    v.close();
    assert.equal(f.resets, 1);
  } finally {
    f.browser.dispose();
  }
});
test('disconnect fences a late viewer open and releases no device window', async () => {
  const f = fixture();
  let finish;
  try {
    const target = (await f.run('list')).windows[0];
    f.adapter.tabs.get = () => new Promise((r) => (finish = r));
    const pending = f.browser.open(
      f.req('view', { id: target.id, surface_epoch: target.surface_epoch }),
      () => {},
    );
    await f.run('connection.release');
    finish(f.tabs[0]);
    await assert.rejects(pending, { code: 'expired' });
    assert.equal(f.watches.size, 0);
    assert.equal(f.tabs.length, 1);
  } finally {
    f.browser.dispose();
  }
});

test('repaint requests refresh a static target without closing or resizing it', async () => {
  const f = fixture();
  try {
    const viewer = await f.open();
    await viewer.send({ refresh: true });
    assert.deepEqual(f.calls, [['invalidate', 11]]);
    assert.equal(f.watches.size, 1);
    assert.deepEqual(f.tabs[0].viewport, { width: 1280, height: 720 });
    await assert.rejects(viewer.send({ refresh: 'yes' }), {
      code: 'invalid_argument',
    });
    await assert.rejects(viewer.send({ refresh: true, events: [] }), {
      code: 'invalid_argument',
    });
    assert.equal(f.calls.length, 1);
    viewer.close();
    await assert.rejects(viewer.send({ refresh: true }), { code: 'expired' });
  } finally {
    f.browser.dispose();
  }
});
