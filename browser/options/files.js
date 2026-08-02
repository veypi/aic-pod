/**
 * files.js — 本地文件中心：目录树（懒加载）+ 文本查看/编辑 + 图片预览。
 * 数据源 = PageFS（扩展 IndexedDB，与 background fs 通道同一存储）。
 */

import { loadSettings } from "../src/sdk/storage.js";
import { PageFS } from "../src/sdk/fs.js";
import { showStatus, escapeHtml, fmtSize, cmpStr } from "./common.js";

let fileFs = null;
let currentPath = "/"; // 虚拟根：/home/{uid} 与 /sessions 双根
let currentFile = ""; // 当前查看/编辑的文件绝对路径
let currentObjectUrl = null; // 图片预览 objectURL（切换时回收）
let searchQuery = ""; // 当前搜索词（非空时树面板显示搜索结果）

const fileSearchEl = document.getElementById("file-search");
const fileTreeEl = document.getElementById("file-tree");
const fileContentEl = document.getElementById("file-content");
const fileImageEl = document.getElementById("file-image");
const fileMetaEl = document.getElementById("file-meta");
const fileViewTitle = document.getElementById("file-view-title");
const fileStatus = document.getElementById("file-status");
const fileSaveBtn = document.getElementById("file-save");

async function ensureFs() {
  if (fileFs) return fileFs;
  const s = await loadSettings();
  const parts = (s.key || "").split(".");
  const uid = parts.length === 4 ? parts[3] : "admin";
  fileFs = new PageFS({ uid });
  return fileFs;
}

const FILE_CTX = { sessionId: "" }; // 允许浏览任意 /sessions/* 与 /home/* 及任意绝对路径

// ---- 目录树 ----

// 渲染根目录树（懒加载：目录节点首次展开时读取子项）
async function renderTree(rootAbs) {
  fileTreeEl.innerHTML = "";
  const root = await readDir(rootAbs);
  if (!root) {
    fileTreeEl.innerHTML = `<div class="tree-empty">路径不存在: ${escapeHtml(rootAbs)}</div>`;
    return;
  }
  const ul = document.createElement("ul");
  ul.className = "tree-root";
  ul.appendChild(makeDirNode(rootAbs, rootAbs === "/" ? "本地文件" : root.name, true));
  fileTreeEl.appendChild(ul);
}

// 读取目录子项（含排序：目录优先、名称字节序）
async function readDir(abs) {
  try {
    const fs = await ensureFs();
    const st = await fs.stat(abs, FILE_CTX);
    if (st === null || !st.dir) return null;
    const res = await fs.list(abs, FILE_CTX);
    const items = (res.items || []).slice().sort((a, b) =>
      a.dir !== b.dir ? (a.dir ? -1 : 1) : cmpStr(a.name, b.name),
    );
    return { name: abs.split("/").filter(Boolean).pop() || abs, items };
  } catch (e) {
    return null;
  }
}

// 目录节点（可展开）：初始闭合，首次展开懒加载子项
function makeDirNode(abs, name, expanded) {
  const li = document.createElement("li");
  li.className = "tree-item tree-dir" + (expanded ? " expanded" : "");
  li.dataset.abs = abs; // 展开/懒加载用完整路径
  const tw = document.createElement("span");
  tw.className = "tree-tw";
  tw.textContent = expanded ? "▾" : "▸";
  const label = document.createElement("span");
  label.className = "tree-label";
  label.textContent = name || abs;
  li.appendChild(tw);
  li.appendChild(label);
  if (expanded) {
    const ul = document.createElement("ul");
    li.appendChild(ul);
    loadChildren(li, ul, abs);
  }
  return li;
}

// 懒加载目录子项并挂到 ul（目录节点可继续展开）
async function loadChildren(li, ul, abs) {
  const dir = await readDir(abs);
  if (!dir) {
    ul.innerHTML = `<li class="tree-empty">（空或不可读）</li>`;
    return;
  }
  if (dir.items.length === 0) {
    ul.innerHTML = `<li class="tree-empty">（空目录）</li>`;
    return;
  }
  for (const it of dir.items) {
    const childAbs = it.path; // PageFS list 返回的 path 已带尾斜杠（目录）
    if (it.dir) {
      ul.appendChild(makeDirNode(childAbs, it.name, false));
    } else {
      const fLi = document.createElement("li");
      fLi.className = "tree-item tree-file";
      fLi.dataset.path = childAbs;
      const label = document.createElement("span");
      label.className = "tree-label";
      label.textContent = it.name;
      label.title = `${childAbs} (${fmtSize(it.size)})`;
      fLi.appendChild(label);
      ul.appendChild(fLi);
    }
  }
}

// 目录树事件：文件点击加载内容；目录点击展开/折叠（懒加载）。
// 注意必须取最近的 li 再判类型——文件节点嵌在父目录 li 的子 ul 里，
// 直接 closest("li.tree-dir") 会把文件点击误判成折叠父目录。
fileTreeEl.addEventListener("click", async (e) => {
  const li = e.target.closest("li");
  if (!li || !fileTreeEl.contains(li)) return;

  if (li.classList.contains("tree-file")) {
    await openFile(li.dataset.path);
    return;
  }

  if (li.classList.contains("tree-dir")) {
    const tw = li.querySelector(":scope > .tree-tw");
    const abs = li.dataset.abs || "";
    const ul = li.querySelector(":scope > ul");
    if (li.classList.contains("expanded")) {
      li.classList.remove("expanded");
      if (tw) tw.textContent = "▸";
      if (ul) ul.hidden = true;
    } else {
      li.classList.add("expanded");
      if (tw) tw.textContent = "▾";
      if (!ul) {
        const newUl = document.createElement("ul");
        li.appendChild(newUl);
        await loadChildren(li, newUl, abs);
      } else {
        ul.hidden = false;
      }
    }
  }
});

// ---- 文件内容查看/编辑 ----

function clearPreview() {
  if (currentObjectUrl) {
    URL.revokeObjectURL(currentObjectUrl);
    currentObjectUrl = null;
  }
  fileImageEl.hidden = true;
  fileImageEl.removeAttribute("src");
  fileContentEl.hidden = false;
  fileContentEl.disabled = false;
  fileSaveBtn.disabled = false;
}

async function openFile(abs) {
  try {
    const fs = await ensureFs();
    const raw = await fs.readRaw(abs, FILE_CTX);
    clearPreview();
    currentFile = abs;
    fileViewTitle.textContent = abs;
    const meta = `${raw.mime || "unknown"} · ${fmtSize(raw.size)}`;
    if (raw.content instanceof Blob && raw.mime && raw.mime.startsWith("image/")) {
      // 图片预览（截图等）：objectURL 直显，不可编辑
      currentObjectUrl = URL.createObjectURL(raw.content);
      fileImageEl.src = currentObjectUrl;
      fileImageEl.hidden = false;
      fileContentEl.hidden = true;
      fileContentEl.disabled = true;
      fileSaveBtn.disabled = true;
      fileMetaEl.textContent = `${meta} · 图片预览（不可编辑）`;
    } else if (raw.mime && raw.mime !== "text/plain") {
      fileContentEl.value = `（二进制文件 ${raw.mime}，${fmtSize(raw.size)}，仅支持文本编辑）`;
      fileContentEl.disabled = true;
      fileSaveBtn.disabled = true;
      fileMetaEl.textContent = meta;
    } else {
      fileContentEl.value = String(raw.content ?? "");
      fileMetaEl.textContent = meta;
    }
  } catch (e) {
    showStatus(fileStatus, "读取失败: " + (e?.message || e), "error");
  }
}

async function saveFile() {
  if (!currentFile || fileContentEl.disabled) return;
  try {
    const fs = await ensureFs();
    await fs.writeText(currentFile, fileContentEl.value, FILE_CTX);
    showStatus(fileStatus, "已保存 ✓", "success");
  } catch (e) {
    showStatus(fileStatus, "保存失败: " + (e?.message || e), "error");
  }
}

// ---- 搜索（跨 /home 与 /sessions 全量文件名/路径子串匹配） ----

const SEARCH_ROOTS = ["/home", "/sessions"];
const SEARCH_LIMIT = 100;

let searchTimer = null;
fileSearchEl.addEventListener("input", () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    searchQuery = fileSearchEl.value.trim().toLowerCase();
    if (searchQuery) runSearch(searchQuery);
    else renderTree(currentPath);
  }, 200);
});

async function runSearch(q) {
  const fs = await ensureFs();
  const results = [];
  try {
    for (const root of SEARCH_ROOTS) {
      const w = await fs.walk(root, FILE_CTX);
      for (const it of w.items || []) {
        if (it.dir) continue;
        if (it.path.toLowerCase().includes(q)) {
          results.push(it);
          if (results.length >= SEARCH_LIMIT) break;
        }
      }
      if (results.length >= SEARCH_LIMIT) break;
    }
  } catch (e) {
    showStatus(fileStatus, "搜索失败: " + (e?.message || e), "error");
    return;
  }
  renderSearchResults(q, results);
}

function renderSearchResults(q, results) {
  fileTreeEl.innerHTML = "";
  if (results.length === 0) {
    fileTreeEl.innerHTML = `<div class="tree-empty">无匹配文件: ${escapeHtml(q)}</div>`;
    return;
  }
  const ul = document.createElement("ul");
  ul.className = "tree-root";
  for (const it of results) {
    const name = it.path.split("/").pop();
    const li = document.createElement("li");
    li.className = "tree-item tree-file";
    li.dataset.path = it.path;
    const label = document.createElement("span");
    label.className = "tree-label";
    label.textContent = name;
    const pathEl = document.createElement("span");
    pathEl.className = "result-path";
    pathEl.textContent = it.path;
    label.appendChild(pathEl);
    li.appendChild(label);
    ul.appendChild(li);
  }
  fileTreeEl.appendChild(ul);
  const meta = document.createElement("div");
  meta.className = "tree-empty";
  meta.textContent = results.length >= SEARCH_LIMIT
    ? `已显示前 ${SEARCH_LIMIT} 条（继续输入缩小范围）`
    : `${results.length} 个匹配文件`;
  fileTreeEl.appendChild(meta);
}

// ---- 工具栏 ----

fileSaveBtn.addEventListener("click", saveFile);

document.getElementById("file-refresh").addEventListener("click", () => {
  if (searchQuery) runSearch(searchQuery);
  else renderTree(currentPath);
  if (currentFile) openFile(currentFile);
});

document.getElementById("file-newfile").addEventListener("click", async () => {
  const name = prompt("新建文件（绝对路径或相对当前目录）：");
  if (!name) return;
  try {
    const fs = await ensureFs();
    const abs = name.startsWith("/") ? name : `${currentPath === "/" ? "" : currentPath}/${name}`;
    await fs.writeText(abs, "", FILE_CTX);
    showStatus(fileStatus, "已创建 ✓", "success");
    await renderTree(currentPath);
    const st = await fs.stat(abs, FILE_CTX);
    if (st) await openFile(st.path);
  } catch (e) {
    showStatus(fileStatus, "创建失败: " + (e?.message || e), "error");
  }
});

// ---- Init ----

ensureFs().then(() => renderTree(currentPath));
