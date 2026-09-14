// File-name search contract: basename glob (* and ?), case-sensitive, no content search.
export function searchOptions(opts = {}) {
  for (const key of Object.keys(opts)) if (!['glob', 'limit', 'depth'].includes(key)) throw new Error('fs.search: unsupported option ' + key);
  const glob = opts.glob || '*';
  if (typeof glob !== 'string' || /[\/\\\[\]{}]/.test(glob)) throw new Error('fs.search: basename glob only (* and ?)');
  const limit = opts.limit ?? 100;
  if (!Number.isInteger(limit) || limit < 1 || limit > 200) throw new Error('fs.search: limit must be 1..200');
  const depth = opts.depth ?? 0;
  if (!Number.isInteger(depth) || depth < 0) throw new Error('fs.search: depth must be a nonnegative integer');
  return {glob, limit, depth};
}
export function filenameMatches(name, glob) {
  const source = [...glob].map(c => c === '*' ? '.*' : c === '?' ? '.' : c.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('');
  return new RegExp('^' + source + '$', 'u').test(name);
}
