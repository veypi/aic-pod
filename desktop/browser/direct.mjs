import crypto from 'node:crypto';
import { createBrowserInputQueue } from './input-queue.mjs';

const fail = (code, message) => {
  throw Object.assign(new Error(message), { code });
};
const fields = (args, names) => {
  if (
    !args ||
    Array.isArray(args) ||
    typeof args !== 'object' ||
    Object.keys(args).some((k) => !names.includes(k))
  )
    fail('invalid_argument', 'Invalid browser arguments');
};
const url = (value) => {
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    fail('invalid_argument', 'A valid URL is required');
  }
  if (
    !['http:', 'https:'].includes(parsed.protocol) &&
    parsed.href !== 'about:blank'
  )
    fail('invalid_argument', 'Only HTTP, HTTPS and about:blank are supported');
  return parsed.href;
};

/** Device-owned windows are shared by human viewers and AI. Each live viewer
 * owns only its held input and frame subscription, never exclusive control. */
export function createDirectBrowser(adapter, { now = Date.now } = {}) {
  const epoch = crypto.randomBytes(16).toString('hex'),
    viewers = new Set(),
    revoked = new Map();
  let disposed = false;
  const surface = (tab) => `${epoch}_${tab.id}`;
  const describe = (tab) => ({
    id: String(tab.id),
    title: tab.title,
    url: tab.url,
    viewport: tab.viewport,
    surface_epoch: surface(tab),
    dialog: adapter.dialog?.(Number(tab.id)) || null,
  });
  const check = (req) => {
    if (
      disposed ||
      revoked.has('s:' + req.session_id) ||
      revoked.has('c:' + req.connection_id)
    )
      fail('expired', 'Browser viewer disconnected');
  };
  const admit = (req) => {
    if (
      (!req.session_id && req.method !== 'connection.release') ||
      (!req.connection_id && req.method !== 'session.release')
    )
      fail('unauthorized', 'Device session required');
    if (!Number.isFinite(req.deadline_ms) || req.deadline_ms <= now())
      fail('deadline_exceeded', 'Browser request expired');
    check(req);
  };
  const target = async (args) => {
    if (typeof args.id !== 'string' || !/^\d+$/.test(args.id))
      fail('invalid_argument', 'Browser target ID required');
    let tab;
    try {
      tab = await adapter.tabs.get(Number(args.id));
    } catch {
      fail('not_found', 'Browser window closed');
    }
    if (args.surface_epoch !== surface(tab))
      fail(
        'stale_surface',
        'Browser instance changed; refresh the window list',
      );
    return tab;
  };
  const timer = setInterval(() => {
    for (const [key, until] of revoked) if (until <= now()) revoked.delete(key);
  }, 5000);
  timer.unref?.();
  async function open(req, emit, ended = () => {}) {
    admit(req);
    if (req.method !== 'view')
      fail('unsupported', 'Unknown live browser method');
    fields(req.args, ['id', 'surface_epoch']);
    const tab = await target(req.args);
    check(req);
    const input = adapter.inputFor(tab.id);
    let closed = false,
      off,
      seq = 0;
    const viewer = {
      session: req.session_id,
      connection: req.connection_id,
      async send(args) {
        if (closed) fail('expired', 'Browser viewer closed');
        check(req);
        fields(args, ['events', 'refresh']);
        if (args.refresh !== undefined && args.refresh !== true)
          fail('invalid_argument', 'Invalid repaint request');
        if (
          args.events !== undefined &&
          (!Array.isArray(args.events) ||
            !args.events.length ||
            args.events.length > 64)
        )
          fail('invalid_argument', 'Invalid input batch');
        if (!args.refresh && !args.events)
          fail('invalid_argument', 'Input or repaint request required');
        for (const event of args.events || []) validateEvent(event);
        if (args.refresh) adapter.invalidateFrame(tab.id);
        return inputs.send(args.events || []);
      },
      close(error) {
        if (closed) return;
        closed = true;
        inputs.close(error);
        off?.();
        input.reset();
        viewers.delete(viewer);
        ended(error);
      },
    };
    const inputs = createBrowserInputQueue(
      ({ kind, value }) => {
        check(req);
        return input[kind](value);
      },
      (error) => viewer.close(error),
    );
    viewers.add(viewer);
    try {
      off = adapter.watchFrames(
        tab.id,
        (data) => {
          if (!closed)
            emit(
              {
                id: String(tab.id),
                surface_epoch: surface(tab),
                viewport: tab.viewport,
                frame_seq: ++seq,
                media_type: 'image/jpeg',
              },
              data,
            );
        },
        () =>
          viewer.close(
            Object.assign(new Error('Browser window closed'), {
              code: 'not_found',
            }),
          ),
      );
    } catch (error) {
      viewer.close(error);
      throw error;
    }
    return viewer;
  }
  async function run(req) {
    admit(req);
    const a = req.args,
      method = req.method;
    if (method === 'connection.release' || method === 'session.release') {
      fields(a, []);
      const key = method === 'session.release' ? 'session' : 'connection';
      revoked.set(
        (key === 'session' ? 's:' : 'c:') + req[key + '_id'],
        now() + 15000,
      );
      for (const viewer of [...viewers])
        if (viewer[key] === req[key + '_id']) viewer.close();
      return { released: true };
    }
    if (method === 'list') {
      fields(a, []);
      return { windows: (await adapter.tabs.list()).map(describe) };
    }
    if (method === 'create') {
      fields(a, ['url', 'viewport']);
      const address = url(a.url || 'about:blank');
      if (a.viewport) {
        fields(a.viewport, ['width', 'height']);
        for (const v of [a.viewport.width, a.viewport.height])
          if (!Number.isInteger(v) || v < 320 || v > 4096)
            fail(
              'invalid_argument',
              'Viewport dimensions must be 320–4096 pixels',
            );
      }
      return describe(
        await adapter.tabs.create({ url: address, viewport: a.viewport }),
      );
    }
    if (
      !['navigate', 'close', 'reload', 'back', 'forward', 'dialog'].includes(
        method,
      )
    )
      fail('unsupported', 'Unknown browser method');
    fields(a, [
      'id',
      'surface_epoch',
      ...(method === 'navigate'
        ? ['url']
        : method === 'dialog'
          ? ['accept', 'text']
          : []),
    ]);
    const tab = await target(a);
    check(req);
    if (method === 'close') {
      await adapter.tabs.remove(tab.id);
      return { closed: true };
    }
    if (method === 'navigate') {
      const result = await adapter.cdp.send(tab.id, 'Page.navigate', {
        url: url(a.url),
      });
      if (result.errorText) fail('navigation_failed', result.errorText);
    }
    if (method === 'reload') await adapter.cdp.send(tab.id, 'Page.reload');
    if (method === 'dialog') {
      if (
        typeof a.accept !== 'boolean' ||
        (a.text !== undefined &&
          (typeof a.text !== 'string' || a.text.length > 10000))
      )
        fail('invalid_argument', 'Invalid dialog response');
      await adapter.cdp.send(tab.id, 'Page.handleJavaScriptDialog', {
        accept: a.accept,
        promptText: a.text,
      });
    }
    if (method === 'back' || method === 'forward') {
      const h = await adapter.cdp.send(tab.id, 'Page.getNavigationHistory'),
        entry = h.entries[h.currentIndex + (method === 'back' ? -1 : 1)];
      if (!entry) fail('not_found', 'No history entry');
      await adapter.cdp.send(tab.id, 'Page.navigateToHistoryEntry', {
        entryId: entry.id,
      });
    }
    return describe(await adapter.tabs.get(tab.id));
  }
  return {
    run,
    open,
    dispose() {
      disposed = true;
      clearInterval(timer);
      for (const viewer of [...viewers]) viewer.close();
      revoked.clear();
    },
  };
}
function validateEvent(event) {
  fields(event, ['kind', 'value']);
  const { kind, value } = event;
  if (kind === 'reset') {
    if (value !== undefined && value !== null)
      fail('invalid_argument', 'Invalid reset');
    return;
  }
  if (kind === 'text') {
    if (typeof value !== 'string' || value.length > 10000)
      fail('invalid_argument', 'Text exceeds input limit');
    return;
  }
  // Clipboard belongs to the viewing device, never to the remote machine.
  if (kind === 'edit') {
    if (!['selectAll', 'undo', 'redo'].includes(value))
      fail('unsupported', 'Unsupported edit command');
    return;
  }
  if (!['mouse', 'wheel', 'key'].includes(kind))
    fail('invalid_argument', 'Invalid input event');
  fields(value, [
    'type',
    'x',
    'y',
    'button',
    'buttons',
    'clickCount',
    'dx',
    'dy',
    'mode',
    'key',
    'code',
    'keyCode',
    'repeat',
    'alt',
    'control',
    'shift',
    'meta',
  ]);
  for (const k of [
    'x',
    'y',
    'button',
    'buttons',
    'clickCount',
    'dx',
    'dy',
    'mode',
    'keyCode',
  ])
    if (
      value[k] !== undefined &&
      (!Number.isFinite(value[k]) || Math.abs(value[k]) > 100000)
    )
      fail('invalid_argument', 'Invalid input number');
  for (const k of ['key', 'code', 'type'])
    if (
      value[k] !== undefined &&
      (typeof value[k] !== 'string' || value[k].length > 128)
    )
      fail('invalid_argument', 'Invalid input string');
  if (
    kind === 'mouse' &&
    !['mousedown', 'mouseup', 'mousemove'].includes(value.type)
  )
    fail('invalid_argument', 'Invalid mouse event');
  if (kind === 'key' && !['keyDown', 'keyUp'].includes(value.type))
    fail('invalid_argument', 'Invalid key event');
}
