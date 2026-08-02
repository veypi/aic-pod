// history.js — 执行历史 IndexedDB 持久化（扩展级，独立于 PageFS 存储）。
// 背景：service worker 重启即丢内存历史——落库后跨重启保留。
// 数据模型：db aic-ext-history / store entries（keyPath id 自增），
// 索引 by_time（时间序回放）、by_msg（msg_id 定位终态更新）。
// 容量：保留最近 MAX_KEEP 条，超出在 add 时惰性裁剪。

const DB_NAME = "aic-ext-history";
const STORE = "entries";
const MAX_KEEP = 500; // 保留条数上限（列表展示最近 200）
const PRUNE_TO = 400; // 触发裁剪时删到该条数（避免每次 add 都裁）

function openDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, 1);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE)) {
        const st = db.createObjectStore(STORE, { keyPath: "id", autoIncrement: true });
        st.createIndex("by_time", "time", { unique: false });
        st.createIndex("by_msg", "msgId", { unique: false });
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

export class HistoryStore {
  constructor() {
    this._db = null;
    this._seq = 0; // 同一毫秒内的次序补充（time 碰撞时保序）
  }

  async _ensure() {
    if (!this._db) this._db = await openDB();
    return this._db;
  }

  _tx(db, mode) {
    return db.transaction(STORE, mode).objectStore(STORE);
  }

  // add 追加一条记录（pending 态），返回 Promise（调用方可 fire-and-forget）。
  async add(entry) {
    const db = await this._ensure();
    const rec = { ...entry, seq: ++this._seq };
    await new Promise((resolve, reject) => {
      const req = this._tx(db, "readwrite").add(rec);
      req.onsuccess = () => resolve();
      req.onerror = () => reject(req.error);
    });
    // 惰性裁剪：超 MAX_KEEP 删到 PRUNE_TO
    const count = await this._count(db);
    if (count > MAX_KEEP) await this._prune(db, count - PRUNE_TO);
  }

  // updateState 按 msgId 回填终态（pending → completed/error/...）。
  async updateState(msgId, state, error) {
    if (!msgId) return;
    const db = await this._ensure();
    const idx = this._tx(db, "readwrite").index("by_msg");
    await new Promise((resolve, reject) => {
      const req = idx.get(msgId);
      req.onsuccess = () => {
        const rec = req.result;
        if (!rec) return resolve();
        rec.state = state || "completed";
        rec.error = error || "";
        const put = this._tx(db, "readwrite").put(rec);
        put.onsuccess = () => resolve();
        put.onerror = () => reject(put.error);
      };
      req.onerror = () => reject(req.error);
    });
  }

  // list 取最近 limit 条（时间升序返回，展示层自行倒序）。
  async list(limit = 200) {
    const db = await this._ensure();
    return new Promise((resolve, reject) => {
      const out = [];
      // by_time 索引倒序游标取最新 N 条
      const req = this._tx(db, "readonly").index("by_time").openCursor(null, "prev");
      req.onsuccess = () => {
        const cur = req.result;
        if (!cur || out.length >= limit) {
          // 同 time 按自增 id 次序修正（毫秒碰撞）
          out.sort((a, b) => a.id - b.id);
          resolve(out);
          return;
        }
        out.push(cur.value);
        cur.continue();
      };
      req.onerror = () => reject(req.error);
    });
  }

  // clear 清空（options 手动清除）。
  async clear() {
    const db = await this._ensure();
    return new Promise((resolve, reject) => {
      const req = this._tx(db, "readwrite").clear();
      req.onsuccess = () => resolve();
      req.onerror = () => reject(req.error);
    });
  }

  async _count(db) {
    return new Promise((resolve, reject) => {
      const req = this._tx(db, "readonly").count();
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error);
    });
  }

  // _prune 删除最老的 n 条（by_time 正序游标）。
  async _prune(db, n) {
    return new Promise((resolve, reject) => {
      let deleted = 0;
      const req = this._tx(db, "readwrite").index("by_time").openCursor();
      req.onsuccess = () => {
        const cur = req.result;
        if (!cur || deleted >= n) return resolve();
        cur.delete();
        deleted++;
        cur.continue();
      };
      req.onerror = () => reject(req.error);
    });
  }
}
