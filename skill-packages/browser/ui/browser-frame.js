// A bad compositor frame must not tear down a live browser session. Leave the
// previous canvas untouched until a complete image has decoded successfully.
export function createBrowserFramePainter(
  canvas,
  decode = (blob) => createImageBitmap(blob),
) {
  let disposed = false;
  return {
    async paint(frame) {
      if (disposed || frame.signal?.aborted) return true;
      const data = frame.data;
      if (
        !(data instanceof Uint8Array) ||
        data.length < 4 ||
        data[0] !== 0xff ||
        data[1] !== 0xd8 ||
        data[data.length - 2] !== 0xff ||
        data[data.length - 1] !== 0xd9
      )
        return false;
      let bitmap;
      try {
        bitmap = await decode(new Blob([data], { type: 'image/jpeg' }));
      } catch {
        // BrowserView reopens the read stream to obtain a fresh
        // initial frame, including on a static about:blank page.
        return disposed;
      }
      try {
        if (disposed || frame.signal?.aborted) return true;
        if (!bitmap.width || !bitmap.height) return false;
        const { width, height } = frame.viewport;
        if (canvas.width !== width) canvas.width = width;
        if (canvas.height !== height) canvas.height = height;
        canvas.getContext('2d').drawImage(bitmap, 0, 0, width, height);
        return true;
      } finally {
        bitmap.close();
      }
    },
    dispose() {
      disposed = true;
    },
  };
}
