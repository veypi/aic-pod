import fs from 'node:fs';

// The packaged copy is selected by electron-builder; development reads the same source as Go's embed.
const candidates = [new URL('./schema.json', import.meta.url), new URL('../../protocol/ui/schema.json', import.meta.url)];
export const schema = JSON.parse(fs.readFileSync(candidates.find(p => fs.existsSync(p)), 'utf8'));
export class UIError extends Error {
  constructor(code, message, recovery) { super(message); this.code = code; this.recovery = recovery; }
}
export const fail = (code, message, recovery) => { throw new UIError(code, message, recovery); };
const invalid = message => fail('invalid_argument', message);
export function specFor(domain, op) {
  const spec = schema.commands[op];
  if (!spec?.domains.includes(domain)) return null;
  return { ...spec, ...spec.domain_overrides?.[domain] };
}
export const commands = domain => Object.keys(schema.commands).filter(k => specFor(domain, k)).sort();
const duration = s => {
  const m = /^(\d+(?:\.\d+)?)(ms|s|m)$/.exec(s);
  if (!m) invalid('duration must include ms, s or m');
  const n = Number(m[1]) * { ms: 1, s: 1000, m: 60000 }[m[2]];
  if (!Number.isInteger(n) || n < 1 || n > 300000) invalid('duration must be 1ms..5m in whole milliseconds');
  return n;
};
const scalar = (kind, s) => {
  if (kind === 'string') return s;
  if (kind === 'duration') return duration(s);
  if (kind === 'json') { try { return JSON.parse(s, (_,v) => { if (typeof v === 'number' && !Number.isFinite(v)) throw Error('nonfinite JSON number'); return v; }); } catch { invalid('invalid JSON value'); } }
  if (kind.startsWith('enum:')) { if (!kind.slice(5).split(',').includes(s)) invalid(`expected one of ${kind.slice(5)}`); return s; }
  if (!/^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$/.test(s)) invalid('expected a finite number');
  const n = Number(s);
  if (!Number.isFinite(n)) invalid('expected a finite number');
  if (kind === 'integer' && (!Number.isInteger(n) || n < 1 || n > 2147483647)) invalid('expected a positive integer');
  if (kind === 'positive' && n <= 0) invalid('expected a positive number');
  return n;
};
export function parse(domain, argv) {
  if (!['browser', 'cua'].includes(domain)) invalid('unknown UI domain');
  if (!Array.isArray(argv) || argv.some(a => typeof a !== 'string')) invalid('argv must be a string array');
  if (!argv.length) invalid('command required; use help');
  let op = argv[0], start = 1;
  if (specFor(domain, `${op}.${argv[1]}`)) { op += `.${argv[1]}`; start = 2; }
  const spec = specFor(domain, op);
  if (!spec) fail('unsupported', `unknown ${domain} command ${op}; use help`);
  const allowed = [...schema.common_flags, ...spec.flags];
  if (spec.locator !== 'none') allowed.push(...schema.locator_flags);
  if (spec.observe_after) allowed.push('after');
  if (spec.mutates) allowed.push('delivery');
  const flags = {}, pos = [];
  let literal = false;
  for (let i = start; i < argv.length; i++) {
    const a = argv[i];
    if (!literal && a === '--') { literal = true; continue; }
    if (!literal && a.startsWith('--')) {
      const k = a.slice(2), kind = schema.flags[k];
      if (!allowed.includes(k)) invalid(`unsupported flag --${k} for ${op}`);
      if (Object.hasOwn(flags, k)) invalid(`duplicate flag --${k}`);
      if (kind === 'bool') { flags[k] = true; continue; }
      const n = kind === 'point' ? 2 : 1;
      if (i + n >= argv.length) invalid(`missing value for --${k}`);
      if (kind === 'point') flags[k] = [scalar('number', argv[++i]), scalar('number', argv[++i])];
      else flags[k] = scalar(kind, argv[++i]);
    } else pos.push(a);
  }
  if (pos.length > spec.positions.length) invalid(`too many positional arguments for ${op}`);
  spec.positions.forEach((p, i) => {
    const k = p.replace(/\?$/, '');
    if (i >= pos.length) { if (k === p) invalid(`missing ${k}`); return; }
    if (Object.hasOwn(flags, k)) invalid(`duplicate ${k}`);
    flags[k] = pos[i];
  });
  for (const k of spec.required) if (!Object.hasOwn(flags, k)) invalid(`missing --${k}`);
  const result = { domain, op, args: {}, locator: {}, options: { timeout_ms: ['open', 'navigate'].includes(op) ? 30000 : 10000, after: spec.observe_after ? 'snapshot' : 'none', format: 'text', delivery: 'background' } };
  Object.defineProperty(result, 'spec', { value: spec });
  if (op === 'run') result.options.timeout_ms = 60000;
  for (const [k, v] of Object.entries(flags)) {
    if (k === 'target') result.target = v;
    else if (['timeout', 'after', 'format', 'delivery'].includes(k)) result.options[k === 'timeout' ? 'timeout_ms' : k] = v;
    else (spec.locator !== 'none' && schema.locator_flags.includes(k) ? result.locator : result.args)[k.replaceAll('-', '_')] = v;
  }
  validate(result);
  if (!Object.keys(result.locator).length) delete result.locator;
  return result;
}
function validate(o) {
  const l = o.locator, a = o.args;
  if (o.op === 'run') {
    if (Object.hasOwn(a, 'code') === Object.hasOwn(a, 'file') || !(a.code ?? a.file).trim()) invalid('run requires exactly one nonempty --code or --file');
    if (a.code && Buffer.byteLength(a.code) > 512 * 1024) invalid('script exceeds 512 KiB');
  }
  const n = ['ref', 'role', 'label', 'css', 'at'].filter(k => Object.hasOwn(l, k)).length;
  if (n > 1) invalid('choose one locator: ref, role/name, label, css or at');
  if (l.name !== undefined && l.role === undefined) invalid('--name requires --role');
  if (l.contains && l.role === undefined && l.label === undefined) invalid('--contains requires a semantic locator');
  if (o.spec.locator === 'required' && !n) invalid('locator required');
  if (l.ref !== undefined && !/^@s[a-zA-Z0-9_-]+:e[1-9][0-9]*$/.test(l.ref)) invalid('ref must be a snapshot-bound @s…:eN');
  if (l.at && !l.snapshot) invalid('coordinates require --snapshot');
  if (l.snapshot && !l.at) invalid('--snapshot requires coordinates');
  if (o.domain === 'cua' && l.css !== undefined) fail('unsupported', 'CSS locators are browser-only');
  if (o.op === 'scroll' && a.dx === undefined && a.dy === undefined) invalid('scroll requires --dx or --dy');
  if (o.op === 'drag') {
    const refs = a.from !== undefined && a.to !== undefined;
    const points = a.from_at !== undefined && a.to_at !== undefined && a.snapshot !== undefined;
    if (refs === points) invalid('drag requires --from/--to refs OR --from-at/--to-at/--snapshot');
    if (refs && (a.from_at !== undefined || a.to_at !== undefined || a.snapshot !== undefined) || points && (a.from !== undefined || a.to !== undefined)) invalid('mixed drag locators');
  }
  if (o.op === 'wait') {
    if (n && a.state === undefined && a.value === undefined) invalid('only state/value wait conditions accept a locator');
    if (o.domain === 'cua' && a.state === 'hidden') fail('unsupported', 'native accessibility cannot prove absence');
    if (a.ms > 300000) invalid('--ms must be at most 300000');
    if (['text', 'value', 'state', 'url', 'load', 'ms'].filter(k => Object.hasOwn(a, k)).length !== 1) invalid('wait requires exactly one condition');
    if ((a.state !== undefined || a.value !== undefined) && !n) invalid('--state/--value requires a locator');
    if (o.domain === 'cua' && (a.url !== undefined || a.load !== undefined)) fail('unsupported', 'URL/load conditions are browser-only');
  }
  if (o.op === 'get' && !['title', 'url', 'text', 'value', 'checked', 'bounds', 'enabled'].includes(a.field)) invalid('unknown get field');
  if (o.op === 'menu' && (!Array.isArray(a.path) || !a.path.length || a.path.some(x => typeof x !== 'string' || !x))) invalid('menu path must be a nonempty JSON string array');
  if (o.op === 'set' && (a.value === null || typeof a.value === 'object' && (!Array.isArray(a.value) || a.value.some(x => typeof x !== 'string')))) invalid('set value must be a scalar or string array');
}
export const required = o => o.options.delivery === 'foreground' ? 3 : o.spec.level;
export function help(domain, command) {
  if (command) return specFor(domain, command.replaceAll(' ', '.')) ?? fail('unsupported', 'unknown command');
  return { protocol: schema.protocol, commands: commands(domain).map(op => ({ op, description: specFor(domain, op).description })), locator: 'snapshot ref @s…:eN, role/name, label, CSS (browser), or --at X Y --snapshot S', common: '--target ID --timeout 10s --format text|json', after: 'Actions return a fresh snapshot (--after none|snapshot|screenshot).' };
}
export function result(o) { return { protocol: schema.protocol, domain: o.domain, op: o.op, state: 'completed' }; }
export function errorResult(o, e, performed) {
  const r = result(o); r.state = 'error';
  r.error = { code: e.code || 'execution_failed', message: e.message || String(e), retryable: false, ...(e.recovery ? { recovery: e.recovery } : {}) };
  if (performed !== undefined) r.action = { performed };
  return r;
}
export function render(r, format = 'text') {
  if (format === 'json') return JSON.stringify(r);
  let out = `[ui/1] ${r.domain}.${r.op} state=${r.state}\n`;
  for (const k of ['target', 'action', 'observation', 'data', 'artifacts', 'warnings', 'error']) {
    if (r[k] === undefined) continue;
    if (k === 'observation') {
      const { text, elements, ...meta } = r[k];
      out += `\n[observation]\n${JSON.stringify(meta)}\n${text || ''}\n`;
    } else out += `\n[${k}]\n${JSON.stringify(r[k])}\n`;
  }
  return out;
}
export function envelope(r, format, images = {}) { return { state: r.state, content: render(r, format), error: r.error?.message || '', attrs: images }; }
