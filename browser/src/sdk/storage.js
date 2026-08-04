/**
 * storage.js — chrome.storage.local read/write wrapper
 *
 * Default settings with sensible values for a single-user browser.
 */

const DEFAULTS = {
  key: "",
  host: "https://ivec.ai", // 平台地址（NATS 端点由此推断，与 cli/desktop 同一语义）
  background: true,
  incognito: false,
  autoConnect: true,
  viewport: { width: 1280, height: 720 },
  timeout: 30,
};

/**
 * Load all settings, merging with defaults.
 */
export async function loadSettings() {
  const result = await chrome.storage.local.get("settings");
  if (result.settings) {
    return { ...DEFAULTS, ...result.settings, viewport: { ...DEFAULTS.viewport, ...(result.settings.viewport || {}) } };
  }
  return { ...DEFAULTS };
}

/**
 * Save settings.
 */
export async function saveSettings(settings) {
  await chrome.storage.local.set({ settings });
}

/**
 * Get a single setting value.
 */
export async function getSetting(key) {
  const s = await loadSettings();
  return s[key];
}

/**
 * Get the credential (AIC env key).
 */
export async function getCredential() {
  const s = await loadSettings();
  return s.key;
}

/**
 * Get the platform host address.
 */
export async function getHost() {
  const s = await loadSettings();
  return s.host;
}
