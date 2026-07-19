/**
 * storage.js — chrome.storage.local read/write wrapper
 *
 * Default settings with sensible values for a single-user browser.
 */

const DEFAULTS = {
  key: "",
  url: "wss://ivec.ai/aic/api/nc",
  deviceName: "",
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
 * Get the NATS server URL.
 */
export async function getNatsUrl() {
  const s = await loadSettings();
  return s.url;
}
