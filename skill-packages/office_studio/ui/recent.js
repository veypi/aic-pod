/* recent.js — Office Studio 共享「最近打开」历史（首页 / Excel / Word 三页共用，localStorage 单源）
 * 条目：{ path, name, kind, time }
 *   path — 树路径（统一存 /cloud/... 形态，去 /fs 前缀；打开时各页面自行兼容）
 *   kind — 'excel' | 'word'
 * 任何页面打开/新建保存成功都写入同一列表；首页读取并展示。
 */
export const RECENT_KEY = 'office_studio.recent'
export const RECENT_MAX = 10

const norm = (p) => String(p || '').replace(/^\/fs(?=\/)/, '')

/** 扩展名 → 类型（'excel' | 'word' | ''） */
export function kindOf(path) {
  const name = String(path || '').split('/').filter(Boolean).pop() || ''
  if (/\.xlsx?$/i.test(name)) return 'excel'
  if (/\.docx?$/i.test(name)) return 'word'
  return ''
}

/** 读列表（最近在前；过滤非法项；补全缺失的 kind / name） */
export function loadRecent() {
  try {
    const raw = localStorage.getItem(RECENT_KEY)
    const arr = raw ? JSON.parse(raw) : []
    if (!Array.isArray(arr)) return []
    return arr
      .filter((x) => x && x.path)
      .map((x) => {
        const path = norm(x.path)
        return {
          path,
          name: String(x.name || '') || path.split('/').filter(Boolean).pop() || '',
          kind: x.kind === 'excel' || x.kind === 'word' ? x.kind : kindOf(path),
          time: Number(x.time) || 0,
        }
      })
  } catch (e) { return [] }
}

const save = (list) => {
  try { localStorage.setItem(RECENT_KEY, JSON.stringify(list)) } catch (e) { /* 忽略 */ }
}

/** 记一条（去重置顶，最多 RECENT_MAX 条） */
export function pushRecent(path, name) {
  const p = norm(path)
  if (!p) return
  const rest = loadRecent().filter((x) => x.path !== p)
  rest.unshift({ path: p, name: name || '', kind: kindOf(p), time: Date.now() })
  save(rest.slice(0, RECENT_MAX))
}

/** 删除一条 */
export function removeRecent(path) {
  save(loadRecent().filter((x) => x.path !== norm(path)))
}

/** 清空 */
export function clearRecent() {
  save([])
}
