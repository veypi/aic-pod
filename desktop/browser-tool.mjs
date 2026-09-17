/** Authenticated local provider for the ui/1 browser runtime. */
import net from 'node:net';
import crypto from 'node:crypto';
import path from 'node:path';
import os from 'node:os';
import { createBrowserHandler } from './browser/core.mjs';
import { createElectronAdapter } from './electron-adapter.mjs';
import { errorResult, envelope } from './ui/protocol.mjs';

export function safeSessionDir(dir, sid) {
  if (!sid || /[\\/\0]/.test(sid) || sid === '.' || sid === '..') return null;
  const expected = path.join(os.homedir(), '.aic', 'sessions', sid);
  return path.resolve(String(dir || '')) === expected ? expected : null;
}
export async function startBrowserServer({ host, log = () => {} } = {}) {
  const adapter = createElectronAdapter(host), handler = createBrowserHandler(adapter);
  const token = crypto.randomBytes(32).toString('hex');
  const server = net.createServer(conn => {
    let buffer = '', dispatched = false;
    conn.setTimeout(310000, () => conn.destroy());
    conn.on('error', () => {});
    const reply = response => { if (!conn.destroyed) conn.end(JSON.stringify(response) + '\n'); };
    conn.on('data', chunk => {
      if (dispatched) return;
      buffer += chunk;
      if (Buffer.byteLength(buffer) > 4 * 1024 * 1024) { conn.destroy(); return; }
      const lineEnd = buffer.indexOf('\n');
      if (lineEnd < 0) return;
      dispatched = true;
      let req;
      try { req = JSON.parse(buffer.slice(0, lineEnd)); } catch { reply({ state: 'error', error: 'invalid json' }); return; }
      if (req.token !== token) { reply({ state: 'rejected', error: 'invalid token' }); return; }
      const sessionDir = safeSessionDir(req.session_dir, req.session_id);
      if (!sessionDir) { reply({ state: 'rejected', error: 'invalid session_dir' }); return; }
      const startedAt = Date.now();
      Promise.resolve().then(async () => {
        if (req.control === 'session_end') { handler.endSession(req.session_id); reply({ state: 'completed', content: 'session released' }); return; }
        const res = await handler({ grantedLevel: req.granted_level, sessionID: req.session_id, msgID: req.msg_id, sessionDir, deadline: req.deadline, startedAt, authorizedFile: req.authorized_file, downloadDir: req.download_dir }, { argv: req.argv });
        reply(res);
      }).catch(e => {
        log('[browser] request failed: %s', e.message);
        reply(envelope(errorResult({ domain: 'browser', op: req.argv?.[0] || 'unknown' }, e, 'unknown')));
      });
    });
  });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  server.once('close', () => handler.dispose());
  const port = server.address().port;
  log('[browser] ui/1 channel listening on 127.0.0.1:%d', port);
  return { port, token, server, adapter };
}
