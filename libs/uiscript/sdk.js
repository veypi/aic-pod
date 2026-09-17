// The SDK only builds ui/1 argv. Parsing, authorization and execution belong to
// the host; helpers do not introduce a second set of interaction semantics.
(function (bridge, log, domain, schema) {
  'use strict';
  const encode = JSON.stringify, decode = JSON.parse;
  let pending = false;
  function fail(code, message) { const e = new Error(message); e.code = code; throw e; }
  function object(value) {
    if (!value || typeof value !== 'object' || Array.isArray(value)) fail('invalid_argument', 'expected an options object');
    return value;
  }
  function observation(value) {
    if (!value) return value;
    Object.defineProperty(value, 'ref', { enumerable: false, value(query) {
      object(query);
      if (!Object.keys(query).length || Object.keys(query).some(k => !['role','name','value','contains'].includes(k))) fail('invalid_argument', 'ref query accepts role/name/value and contains');
      if (!Array.isArray(value.elements)) fail('stale_ref', 'observation has no inline elements; request a narrower snapshot with query');
      const matches = value.elements.filter(e => Object.keys(query).every(k => k === 'contains' || (query.contains ? String(e[k]).includes(String(query[k])) : e[k] === query[k])));
      if (matches.length !== 1) fail(matches.length ? 'ambiguous_target' : 'not_found', 'ref query must match exactly one element');
      if (!matches[0].ref) fail('not_actionable', 'element has no actionable ref');
      return matches[0].ref;
    }});
    return value;
  }
  function wrap(result) {
    observation(result.observation);
    if (result.observation) Object.defineProperty(result, 'ref', {enumerable:false, value:q => result.observation.ref(q)});
    return result;
  }
  async function call(argv) {
    if (!Array.isArray(argv) || argv.some(v => typeof v !== 'string')) fail('invalid_argument', 'ui.call expects a string argv array');
    if (pending) fail('concurrent_call', 'await each ui call before starting the next one');
    pending = true;
    let reply;
    try { reply = decode(await bridge(encode(argv))); } finally { pending = false; }
    const result = wrap(reply.result);
    if (result.state !== 'completed') {
      const e = new Error(result.error ? result.error.message : 'UI command failed');
      e.code = result.error ? result.error.code : 'execution_failed'; e.step = reply.index; e.result = result;
      throw e;
    }
    return result;
  }
  function command(op, params = {}) {
    object(params);
    const base = schema.commands[op];
    if (!base || !base.domains.includes(domain) || op === 'run') fail('unsupported', 'unsupported or nested UI command: ' + op);
    const spec = Object.assign({}, base, (base.domain_overrides || {})[domain]);
    const args = Object.assign({}, params);
    if (args.locator !== undefined) { Object.assign(args, locator(args.locator)); delete args.locator; }
    const positions = [];
    for (const position of spec.positions) {
      const key = position.replace(/\?$/, '');
      if (args[key] === undefined) { if (key === position) fail('invalid_argument', 'missing ' + key); continue; }
      if (typeof args[key] !== 'string') fail('invalid_argument', key + ' must be a string');
      positions.push(args[key]); delete args[key];
    }
    const argv = op.split('.');
    for (const key of Object.keys(args)) {
      const name = key.replace(/[A-Z]/g, c => '-' + c.toLowerCase()).replace(/_/g, '-');
      const kind = schema.flags[name], value = args[key];
      if (!kind || value === undefined) fail('invalid_argument', 'unknown or undefined option: ' + key);
      if (kind === 'bool') { if (typeof value !== 'boolean') fail('invalid_argument', key + ' must be boolean'); if (value) argv.push('--' + name); }
      else if (kind === 'point') { if (!Array.isArray(value) || value.length !== 2 || value.some(n => typeof n !== 'number' || !Number.isFinite(n))) fail('invalid_argument', key + ' must be [x,y]'); argv.push('--' + name, ...value.map(String)); }
      else if (kind === 'json') argv.push('--' + name, encode(value));
      else { if (typeof value !== 'string' && !(typeof value === 'number' && Number.isFinite(value) && kind !== 'string' && kind !== 'duration')) fail('invalid_argument', 'invalid option: ' + key); argv.push('--' + name, String(value)); }
    }
    if (positions.length) argv.push('--', ...positions);
    return call(argv);
  }
  function locator(value) { return typeof value === 'string' ? {ref:value} : object(value); }
  const ui = {call, command};
  for (const op of Object.keys(schema.commands)) {
    if (!schema.commands[op].domains.includes(domain) || op === 'run') continue;
    const parts = op.split('.');
    if (parts.length === 1) ui[op] = params => command(op, params);
    else { ui[parts[0]] ||= {}; ui[parts[0]][parts[1]] = params => command(op, params); }
  }
  const target = (id, opts = {}) => command('target.use', Object.assign({}, opts, {id}));
  Object.assign(target, ui.target); target.use = target; ui.target = target;
  ui.targets = opts => command('target.list', opts);
  ui.current = opts => command('target.current', opts);
  ui.open = (value, opts = {}) => command('open', typeof value === 'string' ? Object.assign({}, opts, domain === 'browser' ? {url:value} : {app:value}) : value);
  if (domain === 'browser') ui.navigate = (url, opts = {}) => command('navigate', Object.assign({}, opts, {url}));
  for (const op of ['click','move','download']) if (ui[op]) ui[op] = (value, opts = {}) => command(op, Object.assign({}, opts, locator(value)));
  for (const [op, key] of [['fill','text'],['set','value'],['upload','file']]) if (ui[op]) ui[op] = (value, data, opts = {}) => command(op, Object.assign({}, opts, locator(value), {[key]:data}));
  for (const [op, key] of [['type','text'],['press','key']]) ui[op] = (data, opts = {}) => command(op, Object.assign({}, opts, {[key]:data}));
  ui.get = (field, opts = {}) => command('get', Object.assign({}, typeof opts === 'string' ? {ref:opts} : opts, {field}));
  ui.read = (opts = {}) => command('read', typeof opts === 'string' ? {ref:opts} : opts);
  const console = {};
  for (const level of ['log','info','warn','error']) console[level] = (...values) => log(encode({level, text:values.map(v => typeof v === 'string' ? v : encode(v)).join(' ')}));
  ui.log = console.log;
  return {ui, console, encode, error(value) {
    return encode({code: value && typeof value.code === 'string' ? value.code : 'script_error', message: String(value && value.message || value), step: value && Number.isInteger(value.step) ? value.step : undefined});
  }};
})
