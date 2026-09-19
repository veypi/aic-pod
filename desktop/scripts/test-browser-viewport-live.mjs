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
  await testInvalidPaintRecovery(adapter);
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
  // Viewer/RTC integration is covered by TestBrowserSDKLive in libs/rtc.
  await adapter.tabs.remove(tab.id);
  console.log('PASS: fixed 720p/1080p/4K, resize/maximize/minimize/hide, fresh background and full-page capture');
}

async function testInvalidPaintRecovery(adapter) {
  const tab = await adapter.tabs.create({ url: 'about:blank' });
  const wc = webContents.fromId(tab.id), invalidate = wc.invalidate;
  let frames = 0, repaints = 0;
  const off = adapter.watchFrames(tab.id, bytes => {
    assert.equal(nativeImage.createFromBuffer(bytes).isEmpty(), false, 'provider sent an invalid JPEG');
    frames++;
  }, () => {});
  try {
    await until(() => frames > 0, 'initial blank frame');
    await delay(200);
    wc.invalidate = function () { repaints++; return invalidate.call(this); };
    for (const image of [nativeImage.createEmpty(), { isEmpty: () => false, toJPEG: () => Buffer.from([255,216,0,0]) }]) {
      const before = frames, requests = repaints;
      wc.emit('paint', {}, {}, image);
      assert.equal(frames, before, 'invalid frame must be filtered');
      await until(() => repaints > requests && frames > before, 'static page repaint after invalid image');
    }
    assert.equal(wc.isDestroyed(), false);
    console.log('PASS: empty and truncated compositor frames trigger a fresh static-page paint');
  } finally {
    wc.invalidate = invalidate;
    off();
    await adapter.tabs.remove(tab.id);
  }
}
