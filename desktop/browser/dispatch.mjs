import { parse, required, errorResult, envelope, UIError } from '../ui/protocol.mjs';

// Ordinary commands stay serialized until the underlying operation actually ends.
// A modal yields its response, not its execution lock. Dialog controls can then
// run out of band; unrelated input is refused instead of waiting behind the modal.
export function createBrowserDispatcher(adapter, handle, bounded) {
  let active = null;
  const queue = [], controls = new Set();
  const metadata = new Set(['help', 'capabilities', 'target.list', 'target.current']);
  const control = new Set(['dialog.accept', 'dialog.dismiss', 'close']);
  async function failure(job, code, message, detail, performed = false) {
    const r = errorResult(job.op, new UIError(code, message), performed);
    if (job.ctx.uiTarget) r.target = job.ctx.uiTarget;
    if (detail) r.data = { dialog: detail };
    if (code === 'dialog_open') r.error.recovery = 'Use dialog accept or dialog dismiss on this target; do not replay the interrupted operation.';
    await bounded(r, job.ctx.sessionDir);
    return envelope(r, job.op.options.format);
  }
  function reply(job, response) {
    if (job.responded) return;
    job.responded = true;
    clearTimeout(job.timer);
    job.resolve(response);
  }
  function blockQueued() {
    for (const job of queue.splice(0)) reply(job, failure(job, 'browser_busy', 'A browser operation is suspended; resolve its dialog or close its target before issuing more input.'));
  }
  function suspend(job, dialog) {
    if (job.responded) return;
    job.suspended = true;
    job.ctx.yielded = true;
    reply(job, failure(job, dialog ? 'dialog_open' : 'timeout', dialog ? 'Page execution is paused by a JavaScript dialog.' : 'The operation exceeded its deadline; its outcome may still be pending.', dialog, job.ctx.actionStarted ? 'unknown' : false));
    if (job === active || controls.has(job)) blockQueued();
  }
  const unsubscribe = adapter.onDialog?.(({ tab, dialog }) => {
    if (!dialog) return;
    for (const job of [active, ...controls]) if (job?.ctx.nativeTarget === tab) suspend(job, dialog);
  });
  function start(job, lane) {
    const predecessors = lane === 'control' ? [active, ...controls].filter(Boolean) : [];
    if (lane === 'normal') active = job;
    if (lane === 'control') controls.add(job);
    job.ctx.waitForResume = async tab => {
      const waiting = predecessors.filter(previous => previous.ctx.nativeTarget === tab);
      if (!waiting.length) return;
      // Original completion includes any input continuation and postconditions.
      // Wait for earlier controls too: a new dialog can suspend their observation
      // after the initial input has already completed.
      await Promise.all(waiting.map(previous => previous.done));
      const original = waiting[0], r = original.ctx.uiResult;
      if (r) return { request_id: original.ctx.msgID, op: r.op, state: r.state, action: r.action, error: r.error, warnings: r.warnings };
    };
    const end = Math.min(job.ctx.deadline ? Date.parse(job.ctx.deadline) : Infinity, (job.ctx.startedAt || Date.now()) + job.op.options.timeout_ms);
    job.timer = setTimeout(() => suspend(job), Math.max(0, end - Date.now()));
    job.done = Promise.resolve().then(() => handle(job.ctx, job.request)).catch(e => failure(job, e.code || 'execution_failed', e.message, undefined, job.ctx.actionStarted ? 'unknown' : false)).then(response => {
      reply(job, response);
      controls.delete(job);
      if (active === job) active = null;
      drain();
      return response;
    });
  }
  function drain() { if (!active && !controls.size && queue.length) start(queue.shift(), 'normal'); }
  function dispatch(ctx, request) {
    let op;
    try { op = parse('browser', request.argv); } catch { return handle(ctx, request); }
    // Validation/permission failures do not need the execution lane.
    if ((ctx.grantedLevel || 0) < required(op)) return handle(ctx, request);
    return new Promise(resolve => {
      const job = { ctx: { ...ctx, startedAt: ctx.startedAt || Date.now() }, request, op, resolve, responded: false, suspended: false };
      const suspended = active?.suspended || [...controls].some(c => c.suspended);
      if (control.has(op.op) && suspended) {
        if ([...controls].some(c => !c.responded)) { reply(job, failure(job, 'browser_busy', 'A dialog control is already in progress.')); return; }
        start(job, 'control');
      } else if (suspended) {
        if (metadata.has(op.op)) start(job, 'metadata');
        else reply(job, failure(job, 'browser_busy', 'A browser operation is suspended; resolve its dialog or close its target before issuing more input.'));
      } else { queue.push(job); drain(); }
    });
  }
  dispatch.endSession = sid => handle.endSession(sid);
  dispatch.dispose = () => unsubscribe?.();
  return dispatch;
}
