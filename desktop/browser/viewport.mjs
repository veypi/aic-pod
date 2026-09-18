/** Browser coordinates are CSS pixels, with one output pixel per CSS pixel. */
export const DEFAULT_VIEWPORT = Object.freeze({ width: 1280, height: 720 });
export function normalizeViewport(value) {
  const dimension = (v, fallback) => Number.isInteger(v) && v >= 320 && v <= 4096 ? v : fallback;
  return { width: dimension(value?.width, 1280), height: dimension(value?.height, 720) };
}

/** Contain the complete viewport, centered, without resizing or zooming its page. */
export function fitViewport(rect, viewport) {
  if (!rect || rect.width < 2 || rect.height < 2) return null;
  const scale = Math.min(rect.width / viewport.width, rect.height / viewport.height);
  return { x: rect.x + (rect.width - viewport.width * scale) / 2,
    y: rect.y + (rect.height - viewport.height * scale) / 2,
    width: viewport.width * scale, height: viewport.height * scale, scale };
}
export function viewportPoint(rect, x, y) {
  return { x: Math.round((x - rect.x) / rect.scale), y: Math.round((y - rect.y) / rect.scale) };
}
