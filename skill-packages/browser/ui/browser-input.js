const encoder = new TextEncoder();
const MAX_EVENTS = 32, MAX_BYTES = 24 * 1024;

// Continuous input represents the newest pending intent, not work to replay.
// Keep one move and one wheel per segment; discrete input, documents and
// modifiers fence segments so button/key transitions retain their order.
function enqueueLatest(queue, item) {
  const motion = e => e.type === 'pointer.move' || e.type === 'wheel';
  if (motion(item.event)) {
    for (let n=queue.length-1;n>=0;n--) {
      const old=queue[n];
      if (!motion(old.event) || old.document!==item.document || old.event.modifiers!==item.event.modifiers) break;
      if (old.event.type===item.event.type) { queue.splice(n,1); break; }
    }
  }
  queue.push(item);
}

// Batches wait only for the local channel buffer. There is no per-batch RPC or
// execution acknowledgement; stream failures arrive asynchronously.
export class BrowserInput {
  constructor(stream, document, onError) {
    Object.assign(this, {document, onError, queue:[], seq:0});
    const bind = value => {
      if (this.closed) { value.close().catch(() => {}); return value; }
      this.stream = value;
      this.unlisten = value.onClose?.(error => { if (!this.closed) this.fail(error); });
      return value;
    };
    this.ready = stream?.then ? stream.then(bind) : Promise.resolve(bind(stream));
    this.ready.catch(error => { if (!this.closed) this.fail(error); });
  }
  reset() {
    this.queue = [];
    this.enqueue({type:'reset'});
  }
  enqueue(event) {
    if (this.closed) return;
    const item = {event:{...event}, document:this.document()};
    enqueueLatest(this.queue, item);
    if (this.queue.length > 128) {
      this.fail(new Error('设备输入积压，输入通道已关闭'));
      return;
    }
    // Bound dispatch to a display cadence, including when the connection is fast.
    if (!this.flushing && !this.timer) this.timer = setTimeout(() => this.flush(), 16);
  }
  async flush() {
    clearTimeout(this.timer); this.timer = null;
    if (this.flushing || this.closed) return;
    this.flushing = true;
    try {
      const stream = this.stream || await this.ready;
      while (this.queue.length && !this.closed) {
        const document_id = this.queue[0].document;
        const batch = {seq:++this.seq, document_id, events:[]};
        let bytes = encoder.encode(JSON.stringify(batch)).length;
        while (this.queue.length && batch.events.length < MAX_EVENTS) {
          const next = this.queue[0];
          if (next.document !== document_id) break;
          const size = encoder.encode(JSON.stringify(next.event)).length + 1;
          if (bytes + size > MAX_BYTES) {
            if (!batch.events.length) throw new Error('单次输入内容过大');
            break;
          }
          batch.events.push(this.queue.shift().event); bytes += size;
        }
        await stream.send(encoder.encode(JSON.stringify(batch)));
      }
    } catch (e) { if (!this.closed) this.fail(e); }
    finally { this.flushing = false; }
  }
  fail(error) {
    this.close().catch(() => {});
    this.onError(error);
  }
  async close() {
    if (this.closing) return this.closing;
    this.closed = true;
    this.unlisten?.();
    clearTimeout(this.timer);
    this.queue = [];
    this.closing = this.stream?.close() || Promise.resolve();
    await this.closing;
  }
}
