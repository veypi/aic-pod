import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import path from 'node:path';
import { parse, required, commands, help, result, errorResult, envelope, UIError, fail } from '../ui/protocol.mjs';
import { CDPBrowser } from './cdp.mjs';
import { createBrowserDispatcher } from './dispatch.mjs';

const id = prefix => prefix + crypto.randomBytes(8).toString('hex');
const publicTarget = t => ({ id: t.id, kind: 'tab', title: t.title, url: t.url });
const utf8 = (s, n) => { let b = Buffer.from(s); if (b.length <= n) return s; let end = n; while (end > 0 && (b[end] & 0xc0) === 0x80) end--; return b.subarray(0, end).toString('utf8'); };
export function createBrowserHandler(adapter, Driver = CDPBrowser) {
  const sessions = new Map();
  function session(sid) {
    if (!sid) fail('invalid_argument', 'session_id is required');
    if (!sessions.has(sid)) {
      if (sessions.size >= 256) fail('resource_limit', 'too many active UI sessions');
      sessions.set(sid, { targets: new Map(), current: null });
    }
    return sessions.get(sid);
  }
  function invalidate(tab) { for (const s of sessions.values()) for (const t of s.targets.values()) if (t.native === tab) t.snapshot = null; }
  async function discover(s) {
    const tabs = await adapter.tabs.list();
    const live = new Set(tabs.map(t => t.id));
    for (const [k, t] of s.targets) if (!live.has(t.native)) { s.targets.delete(k); if (s.current === k) s.current = null; }
    for (const tab of tabs) {
      let target = [...s.targets.values()].find(t => t.native === tab.id);
      if (!target) { target = { id: id('t'), native: tab.id }; s.targets.set(target.id, target); }
      Object.assign(target, { title: tab.title, url: tab.url });
    }
    return [...s.targets.values()];
  }
  async function bounded(r, root) {
    const raw = JSON.stringify(r);
    if (Buffer.byteLength(raw) <= 12 * 1024) return r;
    let file;
    try {
    await fs.mkdir(path.join(root, '.ui'), { recursive: true, mode: 0o700 });
    file = path.join(root, '.ui', id('result-') + '.json');
    await fs.writeFile(file, raw, { mode: 0o600 });
    r.artifacts = [...(r.artifacts || []), { kind: 'result', path: file, bytes: Buffer.byteLength(raw) }];
    } catch (e) { r.warnings = [...(r.warnings || []), {code:'artifact_failed', message:e.message}]; }
    if (r.observation) {
      r.observation.text = utf8(r.observation.text || '', 6000);
      // Only expose complete element lines/refs in the compact view.
      const last = r.observation.text.lastIndexOf('\n');
      if (last >= 0) r.observation.text = r.observation.text.slice(0, last);
      delete r.observation.elements;
      r.observation.truncated = true;
    }
    if (Buffer.byteLength(JSON.stringify(r)) > 12 * 1024 && r.data !== undefined) r.data = { truncated: true, ...(file ? {path: file} : {}) };
    if (Buffer.byteLength(JSON.stringify(r)) > 12 * 1024) {
      if (r.target) for (const [k,v] of Object.entries(r.target)) if (typeof v === 'string') r.target[k] = utf8(v,1024);
      if (r.error) {r.error.message=utf8(r.error.message,1024);if(r.error.recovery)r.error.recovery=utf8(r.error.recovery,1024);}
      if (r.warnings) r.warnings=r.warnings.slice(0,16).map(w=>({...w,message:utf8(w.message,512)}));
      if (Buffer.byteLength(JSON.stringify(r)) > 12 * 1024) {r.observation={truncated:true};r.data={truncated:true,...(file?{path:file}:{})};}
    }
    return r;
  }
  async function handle(ctx, { argv }) {
    let o;
    try { o = parse('browser', argv); } catch (e) { return envelope(errorResult({ domain: 'browser', op: String(argv?.[0] || 'unknown') }, e, false), argv?.some((v,i)=>v==='--format' && argv[i+1]==='json') ? 'json' : 'text'); }
    let r = result(o), images = {}, started = false, t;
    const end = Math.min(ctx.deadline ? Date.parse(ctx.deadline) : Infinity, (ctx.startedAt || Date.now()) + o.options.timeout_ms);
    const check = () => { if (!Number.isFinite(end) || Date.now() >= end) fail('timeout', 'UI request deadline exceeded'); };
    const sleep = async ms => { check(); await new Promise(resolve => setTimeout(resolve, Math.min(ms, Math.max(1, end - Date.now())))); check(); };
    const cdp = new Driver({ ...adapter, cdp: {
      attach: async tab => { check(); await adapter.cdp.attach(tab); check(); },
      send: async (tab, method, args) => { check(); if (method === 'Runtime.evaluate') args = { ...args, timeout: Math.max(1, end - Date.now()) }; const v = await adapter.cdp.send(tab, method, args); check(); return v; },
      // Release only already-held input/temporary handles after a suspended
      // request expires. Never use this path to begin or repeat an action.
      cleanup: (tab, method, args) => adapter.cdp.send(tab, method, args),
    } });
    const root = ctx.sessionDir;
    async function observe(target, image = false, full = false, filters = {}) {
      check(); invalidate(target.native);
      const sid = id('s');
      const state = await cdp.observe(target.native);
      const refs = new Map();
      const elements = state.elements.map((e, i) => { const ref = `@${sid}:e${i + 1}`; refs.set(ref, e); const { backendNodeId, ...visible } = e; return { ref, ...visible }; });
      let selected = elements;
      if (filters.query) selected = selected.filter(e => ['name', 'role', 'value'].some(k => e[k] !== undefined && String(e[k]).toLowerCase().includes(filters.query.toLowerCase())));
      if (filters.interactive) selected = selected.filter(e => e.editable || ['button', 'link', 'checkbox', 'radio', 'combobox', 'slider', 'menuitem', 'tab'].includes(e.role));
      let text = selected.map(e => `${e.ref} ${e.role} ${JSON.stringify(e.name)}${e.value !== undefined ? ` value=${JSON.stringify(e.value)}` : ''}${e.checked !== undefined ? ` checked=${e.checked}` : ''}`).join('\n');
      if (state.text && !filters.interactive) text += '\n' + (filters.query ? state.text.split('\n').filter(line => line.toLowerCase().includes(filters.query.toLowerCase())).join('\n') : state.text);
      Object.assign(target, { title: state.page.title, url: state.page.url });
      const obs = { snapshot: sid, text, elements: selected, truncated: !!state.truncated, observed_at: new Date().toISOString(), viewport: state.page };
      const snap = { id: sid, refs, page: state.page, revision: adapter.revision(target.native), image: null };
      if (image) {
        const shot = await cdp.screenshot(target.native, full);
        const encoded = await adapter.encodeImage(shot.bytes);
        await fs.mkdir(path.join(root, '.ui'), { recursive: true, mode: 0o700 });
        const file = path.join(root, '.ui', `${sid}.${encoded.mime === 'image/png' ? 'png' : 'jpg'}`);
        await fs.writeFile(file, encoded.bytes, { mode: 0o600 });
        images = { image_data: `data:${encoded.mime};base64,${encoded.bytes.toString('base64')}`, ...(encoded.note ? { image_compressed: encoded.note } : {}) };
        obs.image = { width: encoded.width, height: encoded.height, mime: encoded.mime, path: file, coordinate_space: 'image_pixels', full_page: full };
        snap.image = { ...obs.image, logical: shot.logical };
        r.artifacts = [...(r.artifacts || []), { kind: 'image', path: file, mime: encoded.mime, bytes: encoded.bytes.length }];
      }
      target.snapshot = snap;
      return obs;
    }
    async function resolve(locator, focus = false) {
      check();
      const l = locator || {};
      if (l.ref || l.at) {
        const s = t.snapshot;
        if (!s || s.revision !== adapter.revision(t.native)) fail('stale_ref', 'observation is no longer current', `browser snapshot --target ${t.id}`);
        if (l.ref) { const e = s.refs.get(l.ref); if (!e) fail('stale_ref', 'ref does not belong to this session/target snapshot'); await cdp.inspect(t.native, e); if (s.revision !== adapter.revision(t.native)) fail('stale_ref', 'document changed while resolving the reference'); return e; }
        if (l.snapshot !== s.id || !s.image || s.image.full_page) fail('stale_ref', 'coordinates require the current viewport image snapshot');
        const page = await cdp.evaluate(t.native, '({width:innerWidth,height:innerHeight,scrollX,scrollY})');
        if (Object.keys(page).some(k => page[k] !== s.page[k])) fail('stale_ref', 'viewport geometry changed');
        const [x, y] = l.at;
        if (x < 0 || y < 0 || x >= s.image.width || y >= s.image.height) fail('invalid_argument', 'point is outside the screenshot');
        return { at: [x * s.image.logical.width / s.image.width, y * s.image.logical.height / s.image.height] };
      }
      if (l.css) return cdp.css(t.native, l.css);
      if (l.role || l.label || focus) {
        // Fresh semantic resolution must not reuse old driver refs.
        const state = await cdp.observe(t.native);
        const eq = (a, b) => l.contains ? String(a).includes(b) : a === b;
        const matches = state.elements.filter(e => focus ? e.focused : l.label !== undefined ? eq(e.name, l.label) : e.role === l.role && (l.name === undefined || eq(e.name, l.name)));
        if (!matches.length) fail(focus ? 'focus_required' : 'not_found', focus ? 'no confirmed focused control' : 'semantic locator did not match');
        if (matches.length !== 1) fail('ambiguous_target', `locator matched ${matches.length} controls`);
        return matches[0];
      }
      return null;
    }
    const action = async (fn, delivery = 'cdp_input') => { check(); started = true; ctx.actionStarted = true; try { const v = await fn(); r.action = { performed: true, delivery: delivery }; return v; } finally { if (t) invalidate(t.native); } };
    try {
      check();
      if ((ctx.grantedLevel || 0) < required(o)) fail('permission_denied', `operation requires level ${required(o)}`);
      if (o.op === 'run') fail('unsupported', 'run is orchestrated by the host exec channel, not the browser provider directly');
      if (o.options.delivery === 'foreground') fail('unsupported', 'Electron browser uses background input; foreground is not provided');
      if (o.args.depth !== undefined) fail('unsupported', 'depth projection is not implemented; use --query or --interactive');
      const s = session(ctx.sessionID);
      if (o.op === 'help') r.data = help('browser', o.args.command);
      else if (o.op === 'capabilities') r.data = { protocol: 'ui/1', backend: 'electron-cdp', commands: commands('browser'), locators: ['ref', 'role/name', 'label', 'css', 'image_pixels'], delivery: ['background'], run: 'Host JS worker with ui API; level 3, confined process, sequential calls, 256 steps, 60s default / 5m max; no Node or direct OS APIs.', dialogs: 'Explicit accept/dismiss; synchronous dialogs yield dialog_open, then report continuation in data.resumed. Electron window.prompt() is unsupported.', unsupported: ['driver-backend', 'frame-locator', 'snapshot --depth', 'window.prompt()'], session_isolation: 'bindings_and_refs' };
      else if (o.op === 'target.list') {
        if (o.args.app || o.args.pid) fail('unsupported', '--app/--pid filters are cua-only');
        r.data = (await discover(s)).map(publicTarget);
      } else {
        if (o.op === 'open') {
          let url; try { url = new URL(o.args.url); } catch { fail('invalid_argument', 'invalid URL'); }
          if (!['http:', 'https:'].includes(url.protocol) && url.href !== 'about:blank') fail('invalid_argument', 'only http/https/about:blank URLs are supported');
          const tab = await action(() => adapter.tabs.create({ url: url.href }));
          await discover(s); t = [...s.targets.values()].find(x => x.native === tab.id); s.current = t.id;
        } else {
          await discover(s);
          const targetID = o.op === 'target.use' ? o.args.id : o.target || s.current;
          if (!targetID) fail('target_required', 'select a target with target use or open');
          t = s.targets.get(targetID);
          if (!t) fail('target_closed', 'target is not available in this session');
          if (o.op === 'target.use') s.current = t.id;
        }
        r.target = publicTarget(t);
        ctx.uiTarget = r.target; ctx.nativeTarget = t.native;
        const dialog = adapter.dialog?.(t.native);
        if (dialog && !o.op.startsWith("dialog.") && !["close", "target.current"].includes(o.op)) { r.data = {dialog}; fail("dialog_open", "Page execution is paused by a JavaScript dialog.", "Use dialog accept or dialog dismiss; do not replay the interrupted operation."); }
        const op = o.op;
        if (['open', 'target.use', 'target.current'].includes(op)) { /* observation below */ }
        else if (op === 'snapshot' || op === 'screenshot') r.observation = await observe(t, op === 'screenshot' || !!o.args.image, !!o.args.full, o.args);
        else if (op === 'read' || op === 'get') {
          const e = await resolve(o.locator);
          if (e?.at) fail('unsupported', 'read/get require an element or the whole target');
          const field = op === 'read' ? 'text' : o.args.field;
          if (e) r.data = { [field]: (await cdp.inspect(t.native, e))[field] ?? null };
          else if (['title', 'url', 'text', 'bounds'].includes(field)) r.data = { [field]: await cdp.evaluate(t.native, ({ title: 'document.title', url: 'location.href', text: 'document.body?.innerText || ""', bounds: '({x:0,y:0,width:innerWidth,height:innerHeight})' })[field]) };
          else fail('invalid_argument', `${field} requires a locator`);
        } else if (['click', 'fill', 'type', 'press', 'scroll', 'move', 'set'].includes(op)) {
          const e = await resolve(o.locator, ['type', 'press'].includes(op) && !o.locator);
          if (e?.at && ['fill', 'type', 'press', 'set'].includes(op)) fail('unsupported', `${op} requires an element locator`);
          const effect = await action(() => cdp.act(t.native, op, e, o.args));
          if (effect?.delivery) r.action.delivery = effect.delivery;
        } else if (op === 'drag') {
          const a = await resolve(o.args.from ? { ref: o.args.from } : { at: o.args.from_at, snapshot: o.args.snapshot });
          const b = await resolve(o.args.to ? { ref: o.args.to } : { at: o.args.to_at, snapshot: o.args.snapshot });
          await action(() => cdp.drag(t.native, a, b));
        } else if (op === 'navigate') {
          let u; try { u = new URL(o.args.url); } catch { fail('invalid_argument', 'invalid URL'); }
          if (!['http:', 'https:'].includes(u.protocol) && u.href !== 'about:blank') fail('invalid_argument', 'unsupported URL scheme');
          await action(async () => { const v = await cdp.send(t.native, 'Page.navigate', { url: u.href }); if (v.errorText) fail('navigation_failed', v.errorText); });
        } else if (op === 'back' || op === 'forward') {
          const h = await cdp.send(t.native, 'Page.getNavigationHistory'), entry = h.entries[h.currentIndex + (op === 'back' ? -1 : 1)];
          if (!entry) fail('not_found', 'no history entry');
          await action(() => cdp.send(t.native, 'Page.navigateToHistoryEntry', { entryId: entry.id }));
        } else if (op === 'reload') await action(() => cdp.send(t.native, 'Page.reload'));
        else if (op === 'close') { await action(() => adapter.tabs.remove(t.native)); s.targets.delete(t.id); if (s.current === t.id) s.current = null; }
        else if (op.startsWith('network.') || op.startsWith('console.')) {
          await cdp.send(t.native, 'Runtime.enable');
          const [family, cmd] = op.split('.');
          if (cmd === 'clear') { adapter.events(t.native).clear(family); r.action = { performed: true }; }
          else { let data = adapter.events(t.native).read(family, o.args.id); if (data === undefined) fail('not_found', 'request ID is not in the captured history'); if (Array.isArray(data)) data = data.filter(x => !o.args.filter || JSON.stringify(x).includes(o.args.filter)).slice(-Math.min(o.args.limit || 100, 500)); r.data = data; }
        } else if (op.startsWith('dialog.')) {
          if (adapter.dialog && !adapter.dialog(t.native)) fail('not_found', 'This target has no open JavaScript dialog.');
          await action(() => cdp.send(t.native, 'Page.handleJavaScriptDialog', { accept: op.endsWith('accept'), promptText: o.args.text }));
          const resumed = await ctx.waitForResume?.(t.native);
          if (resumed) r.data = { resumed };
        }
        else if (op === 'eval') r.data = await action(() => cdp.evaluate(t.native, o.args.code), 'page_script');
        else if (op === 'upload') {
          const e = await resolve(o.locator);
          if (!e?.backendNodeId) fail('unsupported', 'upload requires an element locator');
          if (!ctx.authorizedFile || ctx.authorizedFile !== o.args.file) fail('permission_denied', 'upload file was not authorized by host');
          await action(() => cdp.send(t.native, 'DOM.setFileInputFiles', { backendNodeId: e.backendNodeId, files: [ctx.authorizedFile] }));
        } else if (op === 'download') {
          if (!ctx.downloadDir) fail('permission_denied', 'download directory was not authorized by host');
          const e = await resolve(o.locator);
          const file = await action(() => adapter.download(t.native, ctx.downloadDir, () => cdp.act(t.native, 'click', e, {}), end));
          r.artifacts = [{ kind: 'download', ...file }];
        } else if (op === 'wait') {
          if (o.args.ms !== undefined) await sleep(o.args.ms);
          else for (;;) {
            check(); let matched = false;
            try {
              if (o.args.text !== undefined) matched = String(await cdp.evaluate(t.native, 'document.body?.innerText || ""')).includes(o.args.text);
              else if (o.args.url !== undefined) matched = await cdp.evaluate(t.native, 'location.href') === o.args.url;
              else if (o.args.load !== undefined) matched = await cdp.evaluate(t.native, 'document.readyState') === o.args.load;
              else { const e = await resolve(o.locator), info = await cdp.inspect(t.native, e); matched = o.args.state !== undefined ? ({ visible: info.visible, hidden: !info.visible, enabled: info.enabled, disabled: !info.enabled })[o.args.state] : JSON.stringify(info.value) === JSON.stringify(o.args.value); }
            } catch (e) { if (e.code !== 'not_found') throw e; matched = o.args.state === 'hidden'; }
            if (matched) break;
            await sleep(100);
          }
          r.data = { matched: true };
        } else fail('unsupported', `operation ${op} is not implemented`);
        if (o.options.after !== 'none' && op !== 'close' && !ctx.yielded) {
          try { r.observation = await observe(t, o.options.after === 'screenshot'); }
          catch (e) { r.warnings = [...(r.warnings || []), { code: 'observation_failed', message: e.message }]; }
        }
        r.target = publicTarget(t);
      }
    } catch (e) {
      const failed = errorResult(o, e, started ? 'unknown' : false);
      if (e.code === 'permission_denied') failed.state = 'rejected';
      r = { ...r, ...failed, ...(t ? { target: publicTarget(t) } : {}) };
    }
    try { await bounded(r, root); } catch (e) { r.warnings = [...(r.warnings || []), { code: 'artifact_failed', message: e.message }]; }
    ctx.uiResult = r;
    return envelope(r, o.options.format, images);
  }
  handle.endSession = sid => sessions.delete(sid);
  return createBrowserDispatcher(adapter, handle, bounded);
}
