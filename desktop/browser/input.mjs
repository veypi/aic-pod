import { viewportPoint } from './viewport.mjs';
const inside = (r, x, y) => x >= r.x && x < r.x + r.width && y >= r.y && y < r.y + r.height;
const modifiers = m => (m.alt ? 1 : 0) | (m.control ? 2 : 0) | (m.meta ? 4 : 0) | (m.shift ? 8 : 0);

/** Human input uses the same fixed page coordinates as browser automation. */
export function createBrowserInput(getTarget) {
  let grabbed = null, pressed = null;
  const heldKeys = new Map();
  const target = () => { const t = getTarget(); return t && !t.wc.isDestroyed() ? t : null; };
  const send = (t, method, params) => t.wc.debugger.sendCommand(method, params);
  return {
    reset() {
      const mouse = pressed;
      grabbed = null; pressed = null;
      // Focus loss or hiding the viewer must not leave a drag/key held in the page.
      if (mouse && !mouse.wc.isDestroyed()) mouse.wc.debugger.sendCommand('Input.dispatchMouseEvent', {
        type: 'mouseReleased', ...mouse.point, button: mouse.button, buttons: 0, clickCount: 1,
      }).catch(() => {});
      for (const { wc, event } of heldKeys.values()) if (!wc.isDestroyed()) wc.debugger.sendCommand('Input.dispatchKeyEvent', {
        ...event, type: 'keyUp', modifiers: 0, text: undefined,
      }).catch(() => {});
      heldKeys.clear();
    },
    async mouse(m = {}) {
      const t = target(), x = Number(m.x), y = Number(m.y);
      if (!t || !Number.isFinite(x) || !Number.isFinite(y)) return;
      const type = { mousedown: 'mousePressed', mouseup: 'mouseReleased', mousemove: 'mouseMoved' }[m.type];
      if (!type) return;
      // Only a real press inside the picture grants drag capture; IPC sticky alone does not.
      if (!inside(t.rect, x, y) && grabbed !== t.wc.id) return;
      if (m.type === 'mousedown') grabbed = t.wc.id;
      if (m.type === 'mouseup') grabbed = null;
      const button = ({ 0: 'left', 1: 'middle', 2: 'right' })[m.button] || 'none';
      const point = viewportPoint(t.rect, x, y);
      if (m.type === 'mousedown') pressed = { wc: t.wc, point, button };
      else if (m.type === 'mouseup') pressed = null;
      else if (pressed) pressed.point = point;
      await send(t, 'Input.dispatchMouseEvent', { type, ...point,
        button, buttons: Number(m.buttons) || 0, clickCount: type === 'mouseMoved' ? 0 : Math.max(1, Number(m.clickCount) || 1),
        modifiers: modifiers(m) });
    },
    async wheel(m = {}) {
      const t = target(), x = Number(m.x), y = Number(m.y);
      if (!t || !Number.isFinite(x) || !Number.isFinite(y) || !inside(t.rect, x, y)) return;
      const unit = m.mode === 1 ? 40 : m.mode === 2 ? t.rect.height : 1;
      await send(t, 'Input.dispatchMouseEvent', { type: 'mouseWheel', ...viewportPoint(t.rect, x, y),
        deltaX: (Number(m.dx) || 0) * unit / t.rect.scale,
        deltaY: (Number(m.dy) || 0) * unit / t.rect.scale });
    },
    async key(m = {}) {
      const t = target();
      if (!t || !['keyDown', 'keyUp'].includes(m.type)) return;
      const enter = m.key === 'Enter' && !m.control && !m.meta && !m.alt;
      const event = { type: m.type,
        ...(enter && m.type === 'keyDown' ? { text: '\r' } : {}),
        key: String(m.key || ''), code: String(m.code || ''), windowsVirtualKeyCode: Number(m.keyCode) || 0,
        modifiers: modifiers(m), autoRepeat: !!m.repeat };
      if (m.type === 'keyDown') heldKeys.set(event.code || event.key, { wc: t.wc, event });
      else heldKeys.delete(event.code || event.key);
      await send(t, 'Input.dispatchKeyEvent', event);
    },
    async text(value) {
      const t = target();
      if (t && typeof value === 'string' && value.length <= 100000) await send(t, 'Input.insertText', { text: value });
    },
    edit(command) {
      const t = target();
      if (t && ['copy', 'cut', 'paste', 'selectAll', 'undo', 'redo'].includes(command)) t.wc[command]();
    },
  };
}
