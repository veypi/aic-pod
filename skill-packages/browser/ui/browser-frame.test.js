import test from 'node:test';
import assert from 'node:assert/strict';
import { createBrowserFramePainter } from './browser-frame.js';
const jpeg = new Uint8Array([255, 216, 255, 217]);
const frame = (data) => ({ data, viewport: { width: 1280, height: 720 } });
function canvas() {
  return {
    width: 1280,
    height: 720,
    drawn: [],
    getContext() {
      return { drawImage: (bitmap) => this.drawn.push(bitmap) };
    },
  };
}
test('empty and truncated frames leave the last picture intact; corrupt JPEG can recover', async () => {
  const target = canvas();
  let calls = 0,
    closed = 0;
  const painter = createBrowserFramePainter(target, async () => {
    if (++calls === 1)
      throw new DOMException(
        'The source image could not be decoded.',
        'InvalidStateError',
      );
    return { width: 1280, height: 720, close: () => closed++ };
  });
  assert.equal(await painter.paint(frame(new Uint8Array())), false);
  assert.equal(
    await painter.paint(frame(new Uint8Array([255, 216, 0, 0]))),
    false,
  );
  assert.equal(calls, 0);
  assert.equal(await painter.paint(frame(jpeg)), false);
  assert.equal(target.drawn.length, 0);
  assert.equal(target.width, 1280);
  assert.equal(await painter.paint(frame(jpeg)), true);
  assert.equal(target.drawn.length, 1);
  assert.equal(closed, 1);
});
test('a late decode result after switching tabs is released without drawing or warning', async () => {
  const target = canvas();
  let finish,
    closed = 0;
  const painter = createBrowserFramePainter(
    target,
    () => new Promise((r) => (finish = r)),
  );
  const pending = painter.paint(frame(jpeg));
  painter.dispose();
  finish({ width: 1280, height: 720, close: () => closed++ });
  assert.equal(await pending, true);
  assert.equal(target.drawn.length, 0);
  assert.equal(closed, 1);
});
test('decoder rejection after disposing a viewer needs no repaint', async () => {
  const target = canvas();
  let reject;
  const painter = createBrowserFramePainter(
    target,
    () => new Promise((_r, e) => (reject = e)),
  );
  const pending = painter.paint(frame(jpeg));
  painter.dispose();
  reject(new Error('decode failed'));
  assert.equal(await pending, true);
  assert.equal(target.drawn.length, 0);
});

test('an old stream cannot paint after it is replaced while decoding', async () => {
  const target=canvas(), abort=new AbortController();let finish,closed=0;
  const painter=createBrowserFramePainter(target,()=>new Promise(r=>finish=r));
  const pending=painter.paint({...frame(jpeg),signal:abort.signal});
  abort.abort();finish({width:1280,height:720,close:()=>closed++});
  assert.equal(await pending,true);assert.equal(target.drawn.length,0);assert.equal(closed,1);
});
