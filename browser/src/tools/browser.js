/**
 * browser.js — browser 指令集（Chrome 插件端装配入口）
 *
 * 实现 = 平台无关核心（browser/core.js）+ Chrome 适配器（browser/chrome-adapter.js）。
 * 平台自有指令集：open/click/close/download/eval/get/network/read/screenshot/
 * snapshot/tab/wait/sleep；未列能力用 eval <js> 替代。
 */

import { createBrowserHandler } from "./browser/core.js";
import { createChromeAdapter } from "./browser/chrome-adapter.js";

export const browserHandler = createBrowserHandler(createChromeAdapter());
