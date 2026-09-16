// Local execution rules. Approval state never enters this object.
export function parseFsAllow(value) {
  if (typeof value !== 'string') throw new Error('fs_allow entries must be strings');
  const readOnly = value.startsWith('ro:');
  const path = readOnly ? value.slice(3) : value;
  if (!path.trim() || path.includes('\0')) throw new Error('fs_allow path is empty or invalid');
  return { path, readOnly };
}
function clean(path) {
  const parts = String(path).replaceAll('\\', '/').split('/');
  const out = [];
  for (const part of parts) {
    if (!part || part === '.') continue;
    if (part === '..') out.pop(); else out.push(part);
  }
  return '/' + out.join('/');
}
function pathMatch(pattern, path) {
  pattern = clean(pattern); path = clean(path);
  if (!/[?*]/.test(pattern)) return pattern === '/' || path === pattern || path.startsWith(pattern + '/');
  const p = pattern.split('/'), s = path.split('/');
  const memo = new Map();
  const visit = (i,j) => {
    const key = `${i},${j}`;
    if (memo.has(key)) return memo.get(key);
    let hit;
    if (i === p.length) hit = j === s.length;
    else if (p[i] === '**') hit = visit(i+1,j) || (j<s.length && visit(i,j+1));
    else {
      const re = p[i].replace(/[.+^${}()|[\]\\]/g, '\\$&').replaceAll('*','[^/]*').replaceAll('?','[^/]');
      hit = j<s.length && new RegExp('^'+re+'$').test(s[j]) && visit(i+1,j+1);
    }
    memo.set(key,hit); return hit;
  };
  return visit(0,0);
}
export class ExecutionPolicy {
  constructor(config) {
    this.config = structuredClone(config);
    for (const domain of ['fs','exec']) {
      if (!['open','deny'].includes(this.config[domain+'_policy'])) throw new Error('invalid '+domain+'_policy');
      for (const key of [domain+'_deny',domain+'_allow']) {
        const list = this.config[key] ?? [];
        if (!Array.isArray(list) || list.some(x=>typeof x!=='string' || !x.trim() || x.includes('\0'))) throw new Error('invalid '+key);
        this.config[key] = list;
      }
    }
    for (const key of ['exec_allow','exec_deny']) {
      if (this.config[key].some(x => x.trim() !== x || /[/\\\s]/.test(x) || (x !== '*' && /[*?]/.test(x)))) throw new Error('invalid '+key);
    }
    this.fsAllow = this.config.fs_allow.map(parseFsAllow);
    if (this.config.fs_deny.some(x=>x.startsWith('ro:'))) throw new Error('ro: is only valid in fs_allow');
  }
  checkFs(path, write = false) {
    const c = this.config;
    if (c.fs_deny.some(p=>pathMatch(p,path)) || !(this.fsAllow.some(p=>(!write || !p.readOnly) && pathMatch(p.path,path)) || c.fs_policy==='open')) {
      throw new Error(`permission denied: fs ${write?'write':'read'} ${path}`);
    }
  }
  checkExec(name) {
    const c = this.config, hit = list=>list.some(p=>p==='*' || p===name);
    if (hit(c.exec_deny) || !(hit(c.exec_allow) || c.exec_policy==='open')) throw new Error('permission denied: exec '+name);
  }
}
export function pageExecutionPolicy() {
  return new ExecutionPolicy({fs_policy:'deny',fs_deny:[],fs_allow:['/'],exec_policy:'deny',exec_deny:[],exec_allow:['*']});
}
