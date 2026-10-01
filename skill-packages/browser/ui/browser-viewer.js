import { createBrowserFramePainter } from './browser-frame.js';

// The canvas is a view of a device resource. Its DOM size never reaches the host.
export function containPoint(rect, viewport, x, y, captured = false) {
  const scale = Math.min(
    rect.width / viewport.width,
    rect.height / viewport.height,
  );
  if (!(scale > 0)) return null;
  const left = rect.x + (rect.width - viewport.width * scale) / 2,
    top = rect.y + (rect.height - viewport.height * scale) / 2;
  if (
    !captured &&
    (x < left ||
      y < top ||
      x >= left + viewport.width * scale ||
      y >= top + viewport.height * scale)
  )
    return null;
  return {
    x: Math.max(0, Math.min(viewport.width - 1, Math.round((x - left) / scale))),
    y: Math.max(0, Math.min(viewport.height - 1, Math.round((y - top) / scale))),
    scale,
  };
}

// A wheel gesture keeps the hit target from its first event. Small pointer
// movements near a scrollbar/letterbox must not alternate remote and local scroll.
export function createBrowserWheel({ point, send, viewport, enabled, now = () => performance.now() }) {
  let gesture;
  const reset = () => { gesture = null; };
  const active = () => !!gesture && gesture.owner === enabled() && now() - gesture.time < 180;
  return {
    reset, active,
    jitter(e) {
      if (!active()) return false;
      if (Math.hypot(e.clientX - gesture.x, e.clientY - gesture.y) <= 8) return true;
      reset(); // Deliberate pointer movement starts a new hit test/gesture.
      return false;
    },
    handle(e) {
      if (!enabled()) { reset(); return; }
      const modifiers = [e.altKey, e.ctrlKey, e.metaKey, e.shiftKey].join(',');
      if (!active() || gesture.modifiers !== modifiers) {
        const p = point(e);
        if (!p) { reset(); return; }
        gesture = {point:p, modifiers, owner:enabled(), x:e.clientX, y:e.clientY};
      }
      gesture.time = now();
      e.preventDefault();
      e.stopPropagation();
      const p = gesture.point;
      // Pixel/line deltas are in the displayed canvas; page units already refer
      // to the remote viewport and must not include the surrounding letterbox.
      const unit = (e.deltaMode === 1 ? 40 : 1) / p.scale;
      if (e.deltaX || e.deltaY) send('wheel', {
        x:p.x, y:p.y,
        dx:e.deltaX * (e.deltaMode === 2 ? viewport().width : unit),
        dy:e.deltaY * (e.deltaMode === 2 ? viewport().height : unit),
        alt:e.altKey, control:e.ctrlKey, meta:e.metaKey, shift:e.shiftKey,
      });
    },
  };
}
export function createBrowserViewer({ el, view }) {
  const doc = el.ownerDocument,
    win = doc.defaultView,
    canvas = doc.createElement('canvas'),
    input = doc.createElement('textarea');
  canvas.style.cssText =
    'position:absolute;inset:0;width:100%;height:100%;object-fit:contain;';
  canvas.setAttribute('aria-label', 'Device browser window');
  input.style.cssText =
    'position:absolute;width:1px;height:1px;opacity:0;padding:0;border:0;resize:none;';
  input.setAttribute('aria-label', 'Device browser keyboard');
  input.autocomplete = 'off';
  input.spellcheck = false;
  input.tabIndex = -1;
  el.append(canvas, input);
  const painter = createBrowserFramePainter(canvas);
  let viewport = view.target.viewport,
    disposed = false,
    grab = false,
    composing = false;
  const listeners = [],
    on = (node, type, fn, options) => {
      node.addEventListener(type, fn, options);
      listeners.push(() => node.removeEventListener(type, fn, options));
    };
  const point = (e, captured = false) => {
    if (!view.interactive || doc.visibilityState !== 'visible') return null;
    const top = doc.elementFromPoint(e.clientX, e.clientY);
    if (!captured && top !== el && !el.contains(top)) return null;
    return containPoint(
      el.getBoundingClientRect(),
      viewport,
      e.clientX,
      e.clientY,
      captured,
    );
  };
  const mods = (e) => ({
    alt: e.altKey,
    control: e.ctrlKey,
    shift: e.shiftKey,
    meta: e.metaKey,
  });
  const mouse = (type, e, p) =>
    view.send('mouse', {
      type,
      x: p.x,
      y: p.y,
      button: e.button,
      buttons: e.buttons,
      clickCount: e.detail || 1,
      ...mods(e),
    });
  const wheel = createBrowserWheel({
    point, send:(...args) => view.send(...args), viewport:() => viewport,
    enabled:() => view.interactive && doc.visibilityState === 'visible' && view,
  });
  on(
    win,
    'mousedown',
    (e) => {
      wheel.reset();
      const p = point(e);
      if (!p) return;
      grab = true;
      e.preventDefault();
      input.focus({ preventScroll: true });
      mouse('mousedown', e, p);
    },
    true,
  );
  on(
    win,
    'mousemove',
    (e) => {
      // Trackpad jitter must not interleave hover events with an active scroll.
      // Real drags keep their complete press/move/release path.
      if (!grab && wheel.jitter(e)) return;
      const p = point(e, grab);
      if (p) mouse('mousemove', e, p);
    },
    true,
  );
  on(
    win,
    'mouseup',
    (e) => {
      if (!grab) return;
      const p = point(e, true);
      grab = false;
      if (p) {
        e.preventDefault();
        mouse('mouseup', e, p);
      }
    },
    true,
  );
  on(el, 'contextmenu', (e) => {
    if (point(e)) e.preventDefault();
  });
  on(
    win,
    'wheel',
    wheel.handle,
    { passive: false, capture:true },
  );
  const key = (type, e) =>
    view.send('key', {
      type,
      key: e.key,
      code: e.code,
      keyCode: e.keyCode,
      repeat: e.repeat,
      ...mods(e),
    });
  on(win, 'keydown', (e) => {
    if (
      e.target !== input ||
      e.defaultPrevented ||
      e.isComposing ||
      composing ||
      e.keyCode === 229
    )
      return;
    const modified = (e.metaKey || e.ctrlKey) && !e.altKey,
      name = e.key.toLowerCase();
    // Paste uses this device's clipboard event; never the remote OS clipboard.
    if (modified && ['v', 'c', 'x'].includes(name)) return;
    key('keyDown', e);
    if (e.key.length !== 1) e.preventDefault();
  });
  on(win, 'keyup', (e) => {
    if (
      e.target === input &&
      !e.defaultPrevented &&
      !e.isComposing &&
      !composing
    )
      key('keyUp', e);
  });
  on(input, 'paste', (e) => {
    e.preventDefault();
    const text = e.clipboardData?.getData('text/plain');
    if (text) view.send('text', text);
  });
  on(input, 'compositionstart', () => {
    composing = true;
  });
  on(input, 'compositionend', (e) => {
    composing = false;
    if (e.data) view.send('text', e.data);
    input.value = '';
  });
  on(input, 'beforeinput', (e) => {
    if (composing || e.isComposing) return;
    e.preventDefault();
    if (e.inputType === 'insertText' && e.data) view.send('text', e.data);
    input.value = '';
  });
  on(input, 'blur', () => {
    wheel.reset();
    grab = false;
    composing = false;
    input.value = '';
    view.send('reset');
  });
  on(win, 'blur', () => {
    wheel.reset();
    grab = false;
    view.send('reset');
  });
  const visible = () => {
    wheel.reset();
    const r = el.getBoundingClientRect();
    view.setVisible(
      doc.visibilityState === 'visible' && r.width > 1 && r.height > 1,
    );
  };
  on(doc, 'visibilitychange', visible);
  const resize = new ResizeObserver(visible);
  resize.observe(el);
  visible();
  return {
    async paint(frame) {
      const painted = await painter.paint(frame);
      if (painted && !disposed) {
        if (viewport.width !== frame.viewport.width || viewport.height !== frame.viewport.height ||
            view.document !== frame.document_id) wheel.reset();
        viewport = frame.viewport;
      }
      return painted;
    },
    dispose() {
      disposed = true;
      wheel.reset();
      painter.dispose();
      resize.disconnect();
      for (const off of listeners) off();
      canvas.remove();
      input.remove();
      view.close();
    },
  };
}
