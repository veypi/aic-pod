const failure = (code, message) => Object.assign(new Error(message), { code });

// Coalesce only adjacent, compatible motion that has not reached Chromium yet.
// Buttons, keys, text, target changes and wheel direction changes are barriers.
function merge(previous, next) {
  if (!previous || previous.kind !== next.kind) return false;
  const a = previous.value,
    b = next.value;
  const wheel = next.kind === 'wheel';
  if (
    !wheel &&
    !(next.kind === 'mouse' && a.type === 'mousemove' && b.type === 'mousemove')
  )
    return false;
  const changing = wheel ? ['dx', 'dy'] : ['x', 'y'];
  if (
    [...new Set([...Object.keys(a), ...Object.keys(b)])].some(
      (key) => !changing.includes(key) && a[key] !== b[key],
    )
  )
    return false;
  if (wheel) {
    const dx = (a.dx || 0) + (b.dx || 0),
      dy = (a.dy || 0) + (b.dy || 0);
    if (
      (a.dx || 0) * (b.dx || 0) < 0 ||
      (a.dy || 0) * (b.dy || 0) < 0 ||
      Math.abs(dx) > 100000 ||
      Math.abs(dy) > 100000
    )
      return false;
    previous.value = { ...b, dx, dy };
  } else previous.value = { ...b };
  return true;
}

export function createBrowserInputQueue(dispatch, onError) {
  const queue = [],
    active = [];
  let stopped;
  const settle = (job, error) => {
    for (const waiter of job?.waiters || [])
      error ? waiter.reject(error) : waiter.resolve();
    if (job) job.waiters = [];
  };
  const close = (error = failure('expired', 'Browser viewer closed')) => {
    if (stopped) return;
    stopped = error;
    for (const job of active.splice(0)) settle(job, error);
    for (const job of queue.splice(0)) settle(job, error);
  };
  const fail = (error) => {
    if (!stopped) {
      close(error);
      onError(error);
    }
  };
  const drain = () => {
    while (!stopped && queue.length && active.length < 3) {
      const next = queue[0];
      // CDP's wheel acknowledgement can span several compositor frames. Allow
      // a bounded pipeline of compatible motion, but fence every discrete event.
      if (active.some((job) => !merge({ ...job.event }, next.event))) return;
      queue.shift();
      active.push(next);
      let result;
      try {
        result = dispatch(next.event);
      } catch (error) {
        fail(error);
        return;
      }
      Promise.resolve(result).then(() => {
        next.done = true;
        // Resolve callers in input order even if CDP replies out of order.
        while (active[0]?.done) settle(active.shift());
        drain();
      }, fail);
    }
  };
  return {
    send(events) {
      if (stopped) return Promise.reject(stopped);
      if (!events.length) return Promise.resolve();
      for (const event of events) {
        if (merge(queue.at(-1)?.event, event)) continue;
        if (queue.length >= 128) {
          const error = failure('overloaded', 'Browser input queue is full');
          fail(error);
          return Promise.reject(error);
        }
        const copy = { ...event };
        if (copy.value && typeof copy.value === 'object')
          copy.value = { ...copy.value };
        queue.push({ event: copy, waiters: [] });
      }
      const pending = new Promise((resolve, reject) =>
        queue.at(-1).waiters.push({ resolve, reject }),
      );
      drain();
      return pending;
    },
    close,
  };
}
