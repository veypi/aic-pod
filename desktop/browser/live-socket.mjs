// Local authenticated duplex bridge. At most one frame is in flight and one
// latest frame is retained; slow viewers never build a screenshot backlog.
export async function serveBrowserLive(conn, direct, req) {
  let latest,
    credit = false,
    viewer,
    closed = false,
    buffer = '',
    pending = 0;
  const stop = () => {
    if (closed) return;
    closed = true;
    latest = null;
    viewer?.close();
    conn.destroy();
  };
  conn.setTimeout(0);
  conn.on('close', stop);
  conn.on('end', stop);
  const flush = () => {
    if (!credit || !latest || closed) return;
    credit = false;
    const { metadata, data } = latest;
    latest = null;
    const header = Buffer.from(JSON.stringify({ metadata, size: data.length }));
    const size = Buffer.alloc(4);
    size.writeUInt32BE(header.length);
    conn.cork();
    conn.write(size);
    conn.write(header);
    conn.write(data);
    conn.uncork();
  };
  conn.on('drain', flush);
  try {
    viewer = await direct.open(
      req,
      (metadata, data) => {
        if (data.length > 8 * 1024 * 1024) {
          stop();
          return;
        }
        latest = { metadata, data };
        flush();
      },
      stop,
    );
    if (closed) {
      viewer.close();
      return;
    }
    conn.write(JSON.stringify({ result: { ready: true } }) + '\n');
  } catch (error) {
    conn.end(
      JSON.stringify({
        error: {
          code: error.code || 'internal',
          message: error.message,
          retry: 'never',
          effect: 'none',
        },
      }) + '\n',
    );
    return;
  }
  conn.on('data', (chunk) => {
    buffer += chunk;
    if (Buffer.byteLength(buffer) > 128 * 1024) {
      stop();
      return;
    }
    let end;
    while ((end = buffer.indexOf('\n')) >= 0) {
      let message;
      try {
        message = JSON.parse(buffer.slice(0, end));
      } catch {
        stop();
        return;
      }
      buffer = buffer.slice(end + 1);
      if (message.type === 'pull') {
        credit = true;
        flush();
      } else if (message.type === 'input' && ++pending <= 128) {
        // The viewer validates and schedules each batch immediately, so motion
        // arriving during a CDP dispatch can merge with pending motion.
        Promise.resolve()
          .then(() => (closed ? undefined : viewer.send(message.data)))
          .catch(stop)
          .finally(() => pending--);
      } else {
        stop();
        return;
      }
    }
  });
}
