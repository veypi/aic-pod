import { test } from 'node:test';
import assert from 'node:assert/strict';
import { normalizeViewport, fitViewport, viewportPoint } from './viewport.mjs';

test('720p default and invalid configuration fallback', () => {
  assert.deepEqual(normalizeViewport(), { width: 1280, height: 720 });
  assert.deepEqual(normalizeViewport({ width: 1920, height: 1080 }), { width: 1920, height: 1080 });
  assert.deepEqual(normalizeViewport({ width: -1, height: Infinity }), { width: 1280, height: 720 });
});
test('contain letterboxes, preserves aspect ratio and maps input independently of host size', () => {
  const viewport = normalizeViewport();
  const rect = fitViewport({ x: 20, y: 30, width: 640, height: 500 }, viewport);
  assert.deepEqual(rect, { x: 20, y: 100, width: 640, height: 360, scale: 0.5 });
  assert.deepEqual(viewportPoint(rect, 340, 280), { x: 640, y: 360 });
  const wide = fitViewport({ x: -100, y: 0, width: 1920, height: 720 }, viewport);
  assert.deepEqual(wide, { x: 220, y: 0, width: 1280, height: 720, scale: 1 });
  assert.equal(fitViewport({ width: 0, height: 0 }, viewport), null);
});
