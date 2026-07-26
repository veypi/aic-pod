/**
 * crypto.js — HKDF-SHA256 + HMAC-SHA256 via Web Crypto API
 *
 * Ported from Go SDK: sdk/crypto.go
 * Uses SubtleCrypto instead of Go's crypto/hmac + hkdf package.
 */

/**
 * HKDF-SHA256 key derivation with per-purpose info strings.
 * Returns base64url-encoded 32-byte keys.
 */
async function hkdfDerive(secret, salt, info) {
  const enc = new TextEncoder();
  const keyMaterial = await crypto.subtle.importKey(
    "raw", enc.encode(secret),
    { name: "HKDF" }, false, ["deriveBits"]
  );
  const derived = await crypto.subtle.deriveBits(
    { name: "HKDF", hash: "SHA-256", salt: enc.encode(salt), info: enc.encode(info) },
    keyMaterial, 256
  );
  return btoaUrl(new Uint8Array(derived));
}

/**
 * Derive three purpose-isolated keys from secret and envID.
 * - kConnect: for NATS connect token signing
 * - kServer:  for server proof verification (reserved)
 * - kTool:    for tool request signature verification
 */
export async function deriveKeys(secret, envID) {
  const kConnect = await hkdfDerive(secret, envID, "aic/host/connect/v1");
  const kServer  = await hkdfDerive(secret, envID, "aic/host/server-proof/v1");
  const kTool    = await hkdfDerive(secret, envID, "aic/host/tool-request/v1");
  return { kConnect, kServer, kTool };
}

/**
 * HMAC-SHA256, returns raw bytes (Uint8Array).
 */
export async function hmacSHA256(key, message) {
  const enc = new TextEncoder();
  // key is a base64url string from HKDF; Go uses []byte(key) which gives
  // the ASCII bytes of the string, NOT the decoded base64 bytes.
  const keyBytes = typeof key === "string" ? enc.encode(key) : key;
  const cryptoKey = await crypto.subtle.importKey(
    "raw", keyBytes,
    { name: "HMAC", hash: "SHA-256" }, false, ["sign"]
  );
  const sig = await crypto.subtle.sign("HMAC", cryptoKey, enc.encode(message));
  return new Uint8Array(sig);
}

/**
 * HMAC-SHA256, returns base64url-encoded string.
 */
export async function hmacSHA256B64(key, message) {
  const raw = await hmacSHA256(key, message);
  return btoaUrl(raw);
}

/**
 * SHA-256 hash of raw bytes, returns hex string.
 */
export async function sha256Hex(data) {
  const hash = await crypto.subtle.digest("SHA-256", typeof data === "string"
    ? new TextEncoder().encode(data) : data);
  return Array.from(new Uint8Array(hash))
    .map(b => b.toString(16).padStart(2, "0")).join("");
}

// ---- helpers ----

function btoaUrl(bytes) {
  return btoa(String.fromCharCode(...bytes))
    .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function b64urlToBytes(s) {
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  while (s.length % 4) s += "=";
  return Uint8Array.from(atob(s), c => c.charCodeAt(0));
}
