import { BrowserInput } from './browser-input.js';
import { decodeBrowserFrame } from './browser-wire.js';
import { shellQuote, shellFlags } from './shell_quote.js';

// browserCall 经 exec 动作执行 browser vsh 指令并消费 --json 输出
//（hosts-vsh-redesign §5：viewer 改 exec(script)；脚本参数一律经
// shell_quote 单点转义）。返回解析后的 JSON；非 0 退出抛带 stderr 的错误。
// RTC 直连不截断、不转后台（那些都是 NATS/AI 语义）——响应即全量，
// 没有任何兑底分支。
export async function browserCall(connection, script, waitMS = 30000) {
  const r = await connection.execCall(script, { waitMS });
  if (r?.attrs?.exit_code !== '0')
    throw new Error(r?.attrs?.stderr || r?.content || 'browser command failed');
  return JSON.parse(r?.content || 'null');
}

// Browser state belongs to the device tool. Connections own only forwarding.
export class BrowserDirectory {
  constructor(hosts, changed = () => {}) {
    this.hosts = hosts;
    this.changed = changed;
    this.connections = new Map();
    this.closed = false;
  }
  async refresh() {
    if (this.pending) return this.pending;
    this.pending = this.load().finally(() => {
      this.pending = null;
    });
    return this.pending;
  }
  async load() {
    const hosts = await this.hosts.directory.list();
    if (this.closed) return;
    const wanted = new Set(
      hosts.filter((h) => h.status === 'enabled').map((h) => h.id),
    );
    for (const [id, connection] of this.connections)
      if (!wanted.has(id) || connection.closed) {
        this.connections.delete(id);
        await connection.close().catch(() => {});
      }
    const rows = hosts.map(
      (host) =>
        this.rows?.find((r) => r.id === host.id) || {
          id: host.id,
          name: host.name || host.id,
          status: 'connecting',
          windows: [],
        },
    );
    this.rows = rows;
    let next = 0;
    // A slow/unreachable device must not hold up discovery of the other devices.
    const worker = async () => {
      while (!this.closed && next < hosts.length) {
        const index = next++,
          host = hosts[index];
        const row = {
          id: host.id,
          name: host.name || host.hostname || host.id,
          status: 'connecting',
          windows: [],
        };

        if (host.status !== 'enabled') row.status = 'disabled';
        else if (
          host.last_seen &&
          Date.now() - Date.parse(host.last_seen) > 40000
        )
          row.status = 'offline';
        else if (
          !host.caps?.transports?.rtc?.enabled ||
          !host.caps?.tool_protocols?.includes('hosts_rtc/2')
        )
          row.status = 'unavailable';
        else {
          try {
            let connection = this.connections.get(host.id);
            if (!connection) {
              connection = await this.hosts.openTools(host.id);
              if (this.closed) {
                await connection.close();
                return;
              }
              this.connections.set(host.id, connection);
            }
            const result = await browserCall(connection, 'browser page.list --json');
            row.status = 'ready';
            row.windows = (result || []).map((w) => ({
              id: w.page_id, viewport: {width:w.width, height:w.height},
              ...w,
              hostId: host.id,
              hostName: row.name,
              key: `${host.id}/${w.page_id}`,
            }));
          } catch (error) {
            row.status = 'error';
            row.error = error.message;
            const connection = this.connections.get(host.id);
            this.connections.delete(host.id);
            await connection?.close().catch(() => {});
          }
        }
        rows[index] = row;
        if (!this.closed) this.changed(rows.filter(Boolean));
      }
    };
    await Promise.all(
      Array.from({ length: Math.min(4, hosts.length) }, worker),
    );
    if (!this.closed) this.changed(rows.filter(Boolean));
    return rows;
  }
  connection(hostId) {
    return this.connections.get(hostId);
  }
  async close() {
    this.closed = true;
    const connections = [...this.connections.values()];
    this.connections.clear();
    await Promise.all(connections.map((s) => s.close().catch(() => {})));
  }
}

export class BrowserView {
  constructor(connection, target, {frame = () => {}, changed = () => {}} = {}) {
    Object.assign(this, {connection, target, onFrame:frame, changed, visible:true});
  }
  get interactive() { return !!this.live && this.visible && !this.closed && !this.mutating; }
  state(error = '') { this.changed({ready:!!this.live, error}); }
  start() {
    if (this.closed || this.opening || this.live || !this.visible) return this.opening;
    this.opening = this.connect().finally(() => { this.opening = null; });
    return this.opening;
  }
  async connect() {
    try {
      const stream = await this.connection.openStream('page.frames', {page_id:this.target.page_id});
      if (this.closed || !this.visible) { await stream.close(); return; }
      this.live = stream; this.state();
      this.pump(stream);
    } catch(e) { if (!this.closed) this.state(e.message); }
  }
  async pump(stream) {
    let chunks = [], size = 0, meta;
    const display = {stream, pending:null, painting:false, controller:new AbortController(), seq:this.frameSeq || 0};
    try {
      while (!this.closed && this.live === stream) {
        let item;
        item = decodeBrowserFrame(await stream.recv());
        if (this.closed || this.live !== stream) break;
        if (item.metadata) { meta = item.metadata; chunks = []; size = 0; }
        if (!meta) throw new Error('画面缺少元数据');
        const bytes = item.bytes;
        size += bytes.length;
        if (size > 4 * 1024 * 1024) throw new Error('画面过大');
        chunks.push(bytes);
        if (!item.final) continue;
        if (size !== meta.size || meta.page_id !== this.target.page_id) throw new Error('画面数据不完整');
        if (!Number.isSafeInteger(meta.frame_seq) || meta.frame_seq <= 0) throw new Error('画面序号无效');
        if (meta.frame_seq <= display.seq) { chunks = []; size = 0; meta = null; continue; }
        const data = new Uint8Array(size); let offset = 0;
        for (const chunk of chunks) { data.set(chunk, offset); offset += chunk.length; }
        display.seq = meta.frame_seq;
        display.pending = {...meta, viewport:{width:meta.width, height:meta.height}, data, signal:display.controller.signal};
        // Drain RTC immediately. A slow decoder gets one current frame and one
        // replaceable latest frame, never a FIFO of old pictures to replay.
        this.paintPending(display).catch(error => {
          display.error = error;
          stream.close().catch(() => {});
        });
        chunks = []; size = 0; meta = null;
      }
    } catch(e) {
      if (!this.closed && this.live === stream) this.state((display.error || e).message);
    } finally {
      display.controller.abort(); display.pending = null;
      await stream.close();
      if (this.live === stream) {
        this.live = null;
        await this.closeInput();
        if (!this.closed && this.visible && !this.connection.closed)
          this.retryTimer = setTimeout(() => this.start(), 1000);
      }
    }
  }
  async paintPending(display) {
    if (display.painting) return;
    display.painting = true;
    try {
      while (display.pending && this.live === display.stream && !this.closed) {
        const frame = display.pending; display.pending = null;
        if (await this.onFrame(frame) === false) throw new Error('设备画面解码失败');
        if (this.live !== display.stream || this.closed) return;
        if (this.document && this.document !== frame.document_id) await this.closeInput();
        this.document = this.target.document_id = frame.document_id;
        this.target.viewport = frame.viewport;
        this.frameSeq = frame.frame_seq;
      }
    } finally { display.painting = false; }
  }
  ensureInput() {
    if (this.controls && !this.controls.closed) return this.controls;
    // Opening a channel does not acquire control. Keep the first input queued
    // while the channel connects; the backend reacts to actual input only.
    const controls = new BrowserInput(
      this.connection.openStream('page.input',{page_id:this.target.page_id}),
      () => this.document || this.target.document_id,
      error => {
        if (this.controls !== controls) return;
        this.closeInput().catch(() => {});
        if (!this.closed) this.state(error.message);
      },
    );
    this.controls = controls;
    return controls;
  }
  async closeInput() {
    const controls = this.controls;
    this.controls = null;
    await controls?.close();
  }
  enqueue(event) {
    if (!this.interactive) return;
    this.ensureInput().enqueue(event);
  }
  send(kind, value = {}) {
    if (kind === 'reset') { this.controls?.reset(); return; }
    const modifiers = (value.alt?1:0) | (value.control?2:0) | (value.meta?4:0) | (value.shift?8:0);
    if (kind === 'mouse') this.enqueue({type:{mousemove:'pointer.move',mousedown:'pointer.down',mouseup:'pointer.up'}[value.type],x:value.x,y:value.y,button:['left','middle','right'][value.button] || 'none',modifiers});
    else if (kind === 'wheel') this.enqueue({type:'wheel',x:value.x,y:value.y,delta_x:value.dx,delta_y:value.dy,modifiers});
    else if (kind === 'key') this.enqueue({type:value.type === 'keyDown'?'key.down':'key.up',key:value.key,code:value.code,modifiers});
    else if (kind === 'text') this.enqueue({type:'text',text:value});
  }
  async mutate(method, args = {}) {
    this.mutating = true;
    try {
      await this.closeInput();
      const page = shellQuote(this.target.page_id);
      let script;
      if (method === 'dialog') {
        const rest = {...args, dialog_id: undefined};
        delete rest.dialog_id;
        // 位置参数（page_id/dialog_id 用户可控）置于 `--` 边界后；旗标值用
        // = 形式——`-` 开头的值不被 shellQuote 防护（见 shell_quote.js 契约）。
        script = 'browser page.dialog.resolve' +
          // accept 恒显式传——后端 bool 零值 false，省略 = 取消（确认变取消）。
          (args.accept === false ? ' --accept=false' : ' --accept') +
          (rest.text ? ` --text=${shellQuote(rest.text)}` : '') +
          ` --json -- ${page} ${shellQuote(this.target.dialog?.id || '')}`;
      } else if (method === 'navigate') {
        // 位置参数契约：page.navigate <page_id> <url>（后端无 --url 旗标）。
        script = `browser page.navigate --json -- ${page} ${shellQuote(args.url || 'about:blank')}`;
      } else {
        script = `browser page.${method}` +
          (Object.keys(args).length ? ' ' + shellFlags(args) : '') + ` --json -- ${page}`;
      }
      return await browserCall(this.connection, script);
    } finally { this.mutating = false; }
  }
  setVisible(visible) {
    this.visible = visible;
    if (!visible) {
      this.closeInput().catch(() => {});
      const live = this.live; this.live = null; live?.close().catch(() => {});
      clearTimeout(this.retryTimer);
    } else this.start();
  }
  async close() {
    if (this.closed) return;
    this.closed = true; clearTimeout(this.retryTimer);
    try { await this.closeInput(); }
    finally { const live = this.live; this.live = null; await live?.close(); }
  }
}
