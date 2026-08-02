/**
 * about.js — 关于页：展示扩展版本（manifest 单一来源）。
 */

import "./common.js";

document.getElementById("about-version").textContent =
  "v" + chrome.runtime.getManifest().version;
