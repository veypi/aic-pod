/** Regression cases for fixed viewport, live presentation and hidden rendering. */
import assert from 'node:assert/strict';
import { webContents, nativeImage } from 'electron';
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(fn, label) {
  const end = Date.now() + 5000;
  while (Date.now() < end) { if (await fn()) return; await delay(30); }
  throw Error('timed out: ' + label);
}
export async function testViewport({ win, platformView, adapter, base, root, hasViewerSource, hasVhtml }) {
  const ctl = adapter.tabControl;
  const tab = await adapter.tabs.create({ url: base + '/viewport' });
  const wc = webContents.fromId(tab.id);
  const evaluate = code => wc.executeJavaScript(code);
  await until(() => wc.getURL().endsWith('/viewport') && evaluate('document.readyState === "complete"'), 'fixture load');
  const dimensions = () => evaluate('[innerWidth,innerHeight,devicePixelRatio]');
  assert.deepEqual(await dimensions(), [1280, 720, 1]);
  await evaluate('window.resizes=0;addEventListener("resize",()=>window.resizes++)');
  const capture = async (expectedColor) => {
    const { data } = await Promise.race([
      adapter.cdp.send(tab.id, 'Page.captureScreenshot', { format: 'png' }),
      delay(5000).then(() => { throw Error('background screenshot timed out'); }),
    ]);
    const image = nativeImage.createFromBuffer(Buffer.from(data, 'base64'));
    assert.deepEqual(image.getSize(), { width: 1280, height: 720 });
    if (expectedColor) {
      const bitmap = image.toBitmap();
      const index = (600 * 1280 + 1200) * 4;
      assert.deepEqual([...bitmap.subarray(index, index + 3)], expectedColor); // BGRA
    }
  };
  // Start hidden, then resize/move, maximize, minimize, restore and hide again.
  await capture();
  win.showInactive();
  for (const [width, height] of [[800, 600], [1400, 900], [640, 480]]) {
    win.setContentSize(width, height);
    ctl.applyLayout({ rect: { x: 10, y: 20, w: width - 30, h: height - 50 }, visible: true });
    await capture();
    assert.deepEqual(await dimensions(), [1280, 720, 1]);
  }
  const [x, y] = win.getPosition(); win.setPosition(x + 10, y + 10);
  win.maximize(); await delay(250); await capture();
  win.minimize(); await until(() => win.isMinimized(), 'minimize');
  await evaluate('document.body.style.background="rgb(12,34,56)"');
  await capture([56,34,12]);
  assert.equal(win.isMinimized(), true, 'capture must not restore the viewer');
  win.restore(); await until(() => !win.isMinimized(), 'restore');
  win.unmaximize(); await delay(400); win.hide(); await until(() => !win.isVisible(), 'hide');
  await evaluate('document.body.style.background="rgb(78,90,123)"');
  await capture([123,90,78]);
  assert.equal(win.isVisible(), false, 'capture must not show the viewer');
  // A different displayed tab must not affect this target's screenshots or rAF.
  const custom = await adapter.tabs.create({ url: base + '/viewport', viewport: { width: 1920, height: 1080 } });
  const customWC = webContents.fromId(custom.id);
  await until(() => customWC.getURL().endsWith('/viewport') && customWC.executeJavaScript('document.readyState === "complete"'), 'custom tab');
  assert.deepEqual(await customWC.executeJavaScript('[innerWidth,innerHeight,devicePixelRatio]'), [1920,1080,1]);
  await capture([123,90,78]);
  await evaluate('window.tick=0;requestAnimationFrame(()=>window.tick++)');
  await until(() => evaluate('window.tick > 0'), 'background rAF');
  assert.equal(await evaluate('window.resizes'), 0, 'host layout must never resize the browser');
  const full = await adapter.cdp.send(tab.id, 'Page.captureScreenshot', {
    format: 'png', captureBeyondViewport: true, clip: { x: 0, y: 0, width: 1280, height: 1800, scale: 1 },
  });
  assert.deepEqual(nativeImage.createFromBuffer(Buffer.from(full.data, 'base64')).getSize(), { width: 1280, height: 1800 });
  assert.deepEqual(await dimensions(), [1280,720,1], 'full-page capture must retain viewport');
  await adapter.tabs.remove(custom.id);
  // Resolution can exceed the physical display; no OS window clamp may leak in.
  const large = await adapter.tabs.create({ url: base + '/viewport', viewport: { width: 3840, height: 2160 } });
  const largeWC = webContents.fromId(large.id);
  await until(() => largeWC.getURL().endsWith('/viewport') && largeWC.executeJavaScript('document.readyState === "complete"'), '4K tab');
  assert.deepEqual(await largeWC.executeJavaScript('[innerWidth,innerHeight,devicePixelRatio]'), [3840,2160,1]);
  await adapter.tabs.remove(large.id);
  ctl.setActive(tab.id);
  if (!hasViewerSource) {
    console.log('PASS: fixed viewport and background rendering. Viewer integration requires sibling aic/ui or AIC_PLATFORM_UI.');
    await adapter.tabs.remove(tab.id);
    return;
  }
  // Actual production preload + viewer module, including canvas paint and input IPC.
  win.setContentSize(900, 700);
  platformView.setBounds({ x: 0, y: 0, width: 900, height: 700 });
  win.show(); platformView.webContents.focus();
  await platformView.webContents.loadURL(base + '/viewer');
  const ui = code => platformView.webContents.executeJavaScript(code);
  await until(() => ui('!!window.viewer'), 'viewer module');
  await until(() => ui('document.querySelector("canvas").getContext("2d").getImageData(1200,600,1,1).data[0] === 78'), 'live frame');
  const picture = ctl.poolState().rect;
  assert.equal(picture.scale, 0.5);
  assert.equal(picture.y, 100); // 640x500 viewer with vertical letterboxing.
  // Letterbox clicks cannot enter the browser.
  await ctl.input.mouse({ type: 'mousedown', x: 40, y: 40, button: 0, sticky: true });
  assert.equal(await evaluate('window.presses || 0'), 0);
  const click = async (x, y) => {
    platformView.webContents.sendInputEvent({ type: 'mouseDown', x, y, button: 'left', clickCount: 1 });
    platformView.webContents.sendInputEvent({ type: 'mouseUp', x, y, button: 'left', clickCount: 1 });
    await delay(100);
  };
  await click(20 + 120 * .5, 100 + 50 * .5);
  assert.equal(await evaluate('document.activeElement.id'), 'edit');
  platformView.webContents.sendInputEvent({ type: 'keyDown', keyCode: 'A' });
  platformView.webContents.sendInputEvent({ type: 'char', keyCode: 'a' });
  platformView.webContents.sendInputEvent({ type: 'keyUp', keyCode: 'A' });
  await until(() => evaluate('document.getElementById("edit").value === "a"'), 'typed character');
  await ui(`{
    const input = document.querySelector('textarea');
    input.dispatchEvent(new CompositionEvent('compositionstart',{bubbles:true}));
    input.dispatchEvent(new CompositionEvent('compositionend',{bubbles:true,data:'中文'}));
  }`);
  await until(() => evaluate('document.getElementById("edit").value === "a中文"'), 'IME commit');
  // Drag capture delivers release outside the viewer in original page coordinates.
  await evaluate('window.lastRelease=null;addEventListener("mouseup",e=>window.lastRelease=[e.clientX,e.clientY])');
  platformView.webContents.sendInputEvent({ type: 'mouseDown', x: 80, y: 125, button: 'left', clickCount: 1 });
  platformView.webContents.sendInputEvent({ type: 'mouseMove', x: 700, y: 550, button: 'left', modifiers: ['leftButtonDown'] });
  platformView.webContents.sendInputEvent({ type: 'mouseUp', x: 700, y: 550, button: 'left', clickCount: 1 });
  await until(() => evaluate('!!window.lastRelease'), 'drag release');
  assert.deepEqual(await evaluate('window.lastRelease'), [1360,900]);
  await click(80,125);
  await evaluate('window.lastRelease=null');
  platformView.webContents.sendInputEvent({ type: 'mouseDown', x: 80, y: 125, button: 'left', clickCount: 1 });
  await delay(50);
  await ui('document.querySelector("textarea").blur()');
  await until(() => evaluate('!!window.lastRelease'), 'focus loss releases drag');
  await click(80,125);
  // OS layout shortcuts consume their key before it can reach the browser.
  await ui(`document.addEventListener('keydown', e => {if(e.altKey)e.preventDefault()})`);
  await evaluate('window.keys=[];addEventListener("keydown",e=>window.keys.push(e.key))');
  platformView.webContents.sendInputEvent({ type: 'keyDown', keyCode: 'F', modifiers: ['alt'] });
  await delay(100);
  assert.deepEqual(await evaluate('window.keys'), []);
  await ctl.input.edit('selectAll');
  await ctl.input.text('replaced');
  await until(() => evaluate('document.getElementById("edit").value === "replaced"'), 'edit selectAll');
  await ctl.input.wheel({ x: 100, y: 200, dy: 150, mode: 0 });
  await until(() => evaluate('scrollY > 0'), 'scaled scroll');
  // The presentation can be reloaded without closing or resizing the target.
  await new Promise(resolve => {platformView.webContents.once('did-finish-load',resolve);platformView.webContents.reload()});
  await until(() => ui('!!window.viewer'), 'viewer reload');
  await until(() => ui('document.querySelector("canvas").getContext("2d").getImageData(1200,600,1,1).data[0] === 78'), 'fresh frame after viewer reload');
  assert.deepEqual(await dimensions(), [1280,720,1]);
  await evaluate('scrollTo(0,0)');
  await delay(150);
  const preview = await platformView.webContents.capturePage();
  const fs = await import('node:fs/promises');
  await fs.writeFile(root + '/browser-viewer.png', preview.toPNG());
  if (hasVhtml) {
    await platformView.webContents.loadURL(base + '/component');
    await until(() => ui('!!document.querySelector(".frames canvas")'), 'actual vhtml Browser component');
    await until(() => ui('document.querySelector(".frames canvas").getContext("2d").getImageData(1200,600,1,1).data[0] === 78'), 'actual component frame');
    assert.deepEqual(await ui('(window.__vhtml_dev?.errors || []).map(e=>e.message)'), []);
    await fs.writeFile(root + '/browser-component.png', (await platformView.webContents.capturePage()).toPNG());
    await ui('window.$vhtml.destroy()');
    assert.equal(ctl.poolState(), null, 'component disposal detaches only the viewer');
    assert.deepEqual(await dimensions(), [1280,720,1]);
  }
  console.log('PASS: 720p fixed viewport, configured 1080p/4K, resize/maximize/minimize/hide, fresh background frames, full-page capture, scaled viewer, click/drag, keyboard, IME, editing, scroll, reload');
  await adapter.tabs.remove(tab.id);
  ctl.applyLayout({ visible: false });
  await platformView.webContents.loadURL('about:blank');
}
