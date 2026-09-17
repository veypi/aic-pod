import { fail } from '../ui/protocol.mjs';

const keys = {
  Enter: ['Enter', 13], Tab: ['Tab', 9], Escape: ['Escape', 27], Backspace: ['Backspace', 8], Delete: ['Delete', 46],
  ArrowLeft: ['ArrowLeft', 37], ArrowUp: ['ArrowUp', 38], ArrowRight: ['ArrowRight', 39], ArrowDown: ['ArrowDown', 40],
  Home: ['Home', 36], End: ['End', 35], PageUp: ['PageUp', 33], PageDown: ['PageDown', 34], Space: ['Space', 32],
};
export function keyEvent(chord, platform = process.platform) {
  const parts = chord.split('+'), key = parts.pop(); let modifiers = 0;
  for (let m of parts) {
    if (m === 'ControlOrMeta') m = platform === 'darwin' ? 'Meta' : 'Control';
    const bit = { Alt: 1, Control: 2, Meta: 4, Shift: 8 }[m];
    if (!bit || modifiers & bit) fail('invalid_argument', `invalid key modifier ${m}`);
    modifiers |= bit;
  }
  let code, vk, text;
  if (keys[key]) [code, vk] = keys[key];
  else if (/^F(?:[1-9]|1[0-2])$/.test(key)) { code = key; vk = 111 + Number(key.slice(1)); }
  else if (/^[a-zA-Z0-9]$/.test(key)) { code = /^[0-9]$/.test(key) ? `Digit${key}` : `Key${key.toUpperCase()}`; vk = key.toUpperCase().charCodeAt(0); text = key; }
  else fail('invalid_argument', `unsupported key ${key}`);
  const result = { key: key === 'Space' ? ' ' : key, code, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk, modifiers };
  if (!modifiers || modifiers === 8) { if (text) result.text = modifiers === 8 ? text.toUpperCase() : text; if (key === 'Space') result.text = ' '; if (key === 'Enter') result.text = '\r'; }
  return result;
}
export class CDPBrowser {
  constructor(adapter) { this.adapter = adapter; }
  async send(tab, method, params = {}) { await this.adapter.cdp.attach(tab); return this.adapter.cdp.send(tab, method, params); }
  async cleanup(tab, method, params) { return this.adapter.cdp.cleanup ? this.adapter.cdp.cleanup(tab, method, params) : this.adapter.cdp.send(tab, method, params); }
  async evaluate(tab, expression) {
    const r = await this.send(tab, 'Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) fail('execution_failed', r.exceptionDetails.exception?.description || r.exceptionDetails.text);
    return r.result?.value;
  }
  async node(tab, backendNodeId, fn, args = []) {
    const { object } = await this.send(tab, 'DOM.resolveNode', { backendNodeId });
    if (!object?.objectId) fail('stale_ref', 'node is no longer available');
    try {
      const r = await this.send(tab, 'Runtime.callFunctionOn', { objectId: object.objectId, functionDeclaration: fn.toString(), arguments: args.map(value => ({ value })), returnByValue: true, awaitPromise: true });
      if (r.exceptionDetails) fail('stale_ref', r.exceptionDetails.exception?.description || r.exceptionDetails.text);
      return r.result?.value;
    } finally { await this.cleanup(tab, 'Runtime.releaseObject', { objectId: object.objectId }).catch(() => {}); }
  }
  async observe(tab) {
    // Request the tree so DOM mutation events cover existing descendants.
    await this.send(tab, 'DOM.getDocument', { depth: -1, pierce: true });
    const { nodes = [] } = await this.send(tab, 'Accessibility.getFullAXTree');
    const elements = [], text = [];
    for (const n of nodes) {
      if (n.ignored) continue;
      const role = n.role?.value || 'generic', name = n.name?.value || '';
      const properties = Object.fromEntries((n.properties || []).map(p => [p.name, p.value?.value]));
      if (['StaticText', 'InlineTextBox'].includes(role)) { if (role === 'StaticText' && name) text.push(name); continue; }
      if (!n.backendDOMNodeId || ['RootWebArea', 'generic', 'none'].includes(role)) continue;
      elements.push({ role, name, value: n.value?.value, checked: properties.checked === 'true' ? true : properties.checked === 'false' ? false : properties.checked, enabled: !properties.disabled, focused: !!properties.focused, editable: !!properties.editable || ['textbox', 'searchbox', 'combobox'].includes(role), backendNodeId: n.backendDOMNodeId });
    }
    const page = await this.evaluate(tab, `({title:document.title,url:location.href,width:innerWidth,height:innerHeight,scrollX,scrollY,ready:document.readyState})`);
    return { elements, text: text.join('\n'), page, truncated: nodes.length >= 10000 };
  }
  async css(tab, selector) {
    const { root } = await this.send(tab, 'DOM.getDocument', { depth: 0 });
    const { nodeIds } = await this.send(tab, 'DOM.querySelectorAll', { nodeId: root.nodeId, selector });
    if (!nodeIds.length) fail('not_found', 'CSS locator did not match');
    if (nodeIds.length !== 1) fail('ambiguous_target', `CSS locator matched ${nodeIds.length} nodes`);
    const { node } = await this.send(tab, 'DOM.describeNode', { nodeId: nodeIds[0] });
    return { backendNodeId: node.backendNodeId };
  }
  async inspect(tab, element) {
    return this.node(tab, element.backendNodeId, function () {
      if (!this.isConnected) throw Error('detached node');
      const b = this.getBoundingClientRect(), style = getComputedStyle(this);
      return { text: this.innerText ?? this.textContent ?? '', value: this.value ?? this.textContent ?? '', checked: !!this.checked, enabled: !this.disabled && this.getAttribute('aria-disabled') !== 'true', visible: b.width > 0 && b.height > 0 && style.visibility !== 'hidden' && style.display !== 'none', editable: this.isContentEditable || (['INPUT', 'TEXTAREA'].includes(this.tagName) && !this.readOnly && !this.disabled), focused: this === this.ownerDocument.activeElement, bounds: { x: b.x, y: b.y, width: b.width, height: b.height }, tag: this.tagName, type: this.type, multiple: !!this.multiple };
    });
  }
  async focus(tab, e) {
    const info = await this.inspect(tab, e);
    if (!info.enabled) fail('not_actionable', 'element is disabled');
    await this.send(tab, 'DOM.focus', { backendNodeId: e.backendNodeId });
    if (!(await this.inspect(tab, e)).focused) fail('focus_required', 'could not confirm focus on the requested element');
    return info;
  }
  async point(tab, e, scroll = true) {
    if (e.at) return e.at;
    if (scroll) await this.send(tab, 'DOM.scrollIntoViewIfNeeded', { backendNodeId: e.backendNodeId });
    const info = await this.inspect(tab, e);
    if (!info.visible || !info.enabled) fail('not_actionable', 'element is hidden or disabled');
    const { bounds: b } = info;
    const p = [b.x + b.width / 2, b.y + b.height / 2];
    const hit = await this.node(tab, e.backendNodeId, function (x, y) { const n = this.ownerDocument.elementFromPoint(x, y); return !!n && (n === this || this.contains(n)); }, p);
    if (!hit) fail('not_actionable', 'element is covered by another surface');
    return p;
  }
  async press(tab, chord) {
    const event = keyEvent(chord);
    // Chromium editor commands implement familiar modifier shortcuts consistently on each OS.
    const command = event.modifiers === (process.platform === 'darwin' ? 4 : 2) && event.code === 'KeyA' ? ['selectAll'] : undefined;
    try { await this.send(tab, 'Input.dispatchKeyEvent', { type: 'keyDown', ...event, ...(command ? { commands: command } : {}) }); }
    finally { await this.cleanup(tab, 'Input.dispatchKeyEvent', { type: 'keyUp', ...event, text: undefined }); }
  }
  async scroll(tab, x, y, dx, dy) {
    // Wheel acknowledgement precedes compositor updates. Arm before dispatch,
    // then wait for delivery and stable rendered positions (including nested
    // scrollers). No fixed sleep and no page-global bookkeeping are needed.
    const { result } = await this.send(tab, 'Runtime.evaluate', { expression: `(() => {
      let frame, timer, finish, stable = 0, previous;
      const promise = new Promise(resolve => { finish = resolve; });
      const dispose = () => { clearTimeout(timer); cancelAnimationFrame(frame); window.removeEventListener('wheel', wheel, true); };
      const done = settled => { dispose(); finish(settled); };
      const wheel = event => {
        window.removeEventListener('wheel', wheel, true);
        const nodes = [...new Set([...event.composedPath().filter(n => n instanceof Element), document.scrollingElement])].filter(Boolean);
        const position = () => nodes.map(n => [n.scrollLeft, n.scrollTop]).flat().join(',');
        previous = position();
        const tick = () => {
          const current = position(); stable = current === previous ? stable + 1 : 0; previous = current;
          if (stable >= 3) done(true); else frame = requestAnimationFrame(tick);
        };
        frame = requestAnimationFrame(tick);
      };
      window.addEventListener('wheel', wheel, {capture:true,passive:true});
      timer = setTimeout(() => done(false), 1500);
      return {promise, dispose};
    })()`, returnByValue: false });
    const objectId = result?.objectId;
    if (!objectId) fail('execution_failed', 'could not observe wheel delivery');
    try {
      await this.send(tab, 'Input.dispatchMouseEvent', { type: 'mouseWheel', x, y, deltaX: dx, deltaY: dy });
      const settled = await this.send(tab, 'Runtime.callFunctionOn', { objectId, functionDeclaration: 'function() { return this.promise; }', awaitPromise: true, returnByValue: true });
      if (settled.result?.value !== true) fail('verification_failed', 'scroll did not settle within the observation window');
    } finally {
      await this.cleanup(tab, 'Runtime.callFunctionOn', { objectId, functionDeclaration: 'function() { this.dispose(); }' }).catch(() => {});
      await this.cleanup(tab, 'Runtime.releaseObject', { objectId }).catch(() => {});
    }
  }
  async act(tab, op, e, args) {
    if (op === 'click' || op === 'move') {
      const [x, y] = await this.point(tab, e), button = args.button || 'left', count = Number(args.count || 1);
      await this.send(tab, 'Input.dispatchMouseEvent', { type: 'mouseMoved', x, y });
      if (op === 'click') for (let i = 1; i <= count; i++) {
        try { await this.send(tab, 'Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button, clickCount: i }); }
        finally { await this.cleanup(tab, 'Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button, clickCount: i }); }
      }
    } else if (op === 'fill' || op === 'type') {
      const info = await this.focus(tab, e);
      if (!info.editable) fail('unsupported', 'element does not support text editing');
      if (op === 'fill') { await this.press(tab, 'ControlOrMeta+A'); await this.press(tab, 'Backspace'); }
      if (args.text) await this.send(tab, 'Input.insertText', { text: args.text });
      if (op === 'fill' && (await this.inspect(tab, e)).value !== args.text) fail('verification_failed', 'field value differs from requested text');
    } else if (op === 'press') {
      await this.focus(tab, e); await this.press(tab, args.key);
    } else if (op === 'scroll') {
      const [x, y] = e ? await this.point(tab, e) : await this.evaluate(tab, '[innerWidth/2,innerHeight/2]');
      await this.scroll(tab, x, y, args.dx || 0, args.dy || 0);
    } else if (op === 'set') {
      const info = await this.inspect(tab, e);
      if (!info.enabled || !info.visible) fail('not_actionable', 'element is disabled or hidden');
      if (['checkbox', 'radio'].includes(info.type) && typeof args.value === 'boolean') {
        if (info.type === 'radio' && !args.value) fail('unsupported', 'a radio button cannot be unchecked independently');
        if (info.checked !== args.value) await this.act(tab, 'click', e, {});
        if ((await this.inspect(tab, e)).checked !== args.value) fail('verification_failed', 'checkbox state did not change');
      } else if (info.tag === 'SELECT' && (typeof args.value === 'string' || Array.isArray(args.value))) {
        const outcome = await this.node(tab, e.backendNodeId, function (value) {
          const values = Array.isArray(value) ? value : [value];
          if (!this.multiple && values.length !== 1) return 'select is not multiple';
          if (values.some(v => ![...this.options].some(o => o.value === v && !o.disabled))) return 'option not available';
          for (const o of this.options) o.selected = values.includes(o.value);
          this.dispatchEvent(new Event('input', { bubbles: true })); this.dispatchEvent(new Event('change', { bubbles: true }));
          return JSON.stringify([...this.selectedOptions].map(o => o.value).sort()) === JSON.stringify([...new Set(values)].sort()) ? null : 'selection did not persist';
        }, [args.value]);
        if (outcome) fail('not_actionable', outcome);
        return { delivery: 'dom_event' };
      } else if (info.type === 'range' && typeof args.value === 'number') {
        const error = await this.node(tab, e.backendNodeId, function (v) {
          const min = Number(this.min || 0), max = Number(this.max || 100);
          if (v < min || v > max) return 'slider value is out of range';
          const step = this.step === 'any' ? null : Number(this.step || 1);
          if (step && Math.abs((v - min) / step - Math.round((v - min) / step)) > 1e-8) return 'slider step cannot represent the requested value';
          Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(this, String(v));
          this.dispatchEvent(new Event('input', { bubbles: true })); this.dispatchEvent(new Event('change', { bubbles: true }));
          return Number(this.value) === v ? null : 'slider step cannot represent the requested value';
        }, [args.value]);
        if (error) fail('verification_failed', error);
        return { delivery: 'dom_event' };
      } else fail('unsupported', 'unsupported control/value combination');
    } else fail('unsupported', `unsupported input ${op}`);
    return { delivery: 'cdp_input' };
  }
  async drag(tab, from, to) {
    const a = await this.point(tab, from, false), b = await this.point(tab, to, false);
    try {
      await this.send(tab, 'Input.dispatchMouseEvent', { type: 'mousePressed', x: a[0], y: a[1], button: 'left', clickCount: 1 });
      for (let i = 1; i <= 10; i++) await this.send(tab, 'Input.dispatchMouseEvent', { type: 'mouseMoved', x: a[0] + (b[0] - a[0]) * i / 10, y: a[1] + (b[1] - a[1]) * i / 10, button: 'left', buttons: 1 });
    } finally { await this.cleanup(tab, 'Input.dispatchMouseEvent', { type: 'mouseReleased', x: b[0], y: b[1], button: 'left', clickCount: 1 }); }
  }
  async screenshot(tab, full = false) {
    const metrics = await this.send(tab, 'Page.getLayoutMetrics');
    const viewport = metrics.cssLayoutViewport || metrics.layoutViewport;
    const size = full ? metrics.cssContentSize || metrics.contentSize : { x: 0, y: 0, width: viewport.clientWidth, height: viewport.clientHeight };
    const { data } = await this.send(tab, 'Page.captureScreenshot', { format: 'png', captureBeyondViewport: full, ...(full ? { clip: { ...size, scale: 1 } } : {}) });
    return { bytes: Buffer.from(data, 'base64'), logical: { width: size.width, height: size.height }, full };
  }
}
