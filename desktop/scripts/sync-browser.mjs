#!/usr/bin/env node
// Fixed upstream archives are verified before extraction/replacement. The
// complete payload (including resources and notices) stays outside app.asar.
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import crypto from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { pipeline } from 'node:stream/promises'
import { Readable } from 'node:stream'
import { fileURLToPath, pathToFileURL } from 'node:url'
import bundle from '../browser-bundle.cjs'

const desktopDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
export async function sha256(file) {
  const hash = crypto.createHash('sha256')
  for await (const data of fs.createReadStream(file)) hash.update(data)
  return hash.digest('hex')
}

export async function syncBrowser({ platform = process.platform, arch = process.arch,
  root = path.join(desktopDir, 'vendor', 'browser'), archive, force = false,
  config = bundle.manifest } = {}) {
  const key = `${platform}-${arch}`, asset = config.assets[key]
  if (!asset || !/^\d+\.\d+\.\d+\.\d+$/.test(config.version) || !/^[a-f0-9]{64}$/.test(asset.sha256))
    throw new Error(`Invalid or unsupported Chrome manifest target: ${key}`)
  if (!force && !archive) {
    try { return bundle.assertBrowserBundle(root, platform, arch, config) } catch { /* resync incomplete/stale cache */ }
  }
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-browser-'))
  fs.mkdirSync(root, { recursive: true })
  const prepared = fs.mkdtempSync(path.join(root, '.sync-'))
  const dest = path.join(root, key), backup = path.join(prepared, 'previous')
  try {
    if (!archive) {
      archive = path.join(temporary, 'chrome.zip')
      const url = `https://storage.googleapis.com/chrome-for-testing-public/${config.version}/${asset.platform}/chrome-${asset.platform}.zip`
      const response = await fetch(url, { signal: AbortSignal.timeout(300000) })
      if (!response.ok) throw new Error(`Chrome download failed: HTTP ${response.status}`)
      await pipeline(Readable.fromWeb(response.body), fs.createWriteStream(archive))
    }
    if (await sha256(archive) !== asset.sha256) throw new Error(`Chrome SHA-256 mismatch for ${key}`)
    const extracted = path.join(temporary, 'extracted')
    fs.mkdirSync(extracted)
    if (process.platform === 'win32') {
      // Paths travel through env, never interpolated into PowerShell code.
      execFileSync('powershell', ['-NoProfile', '-NonInteractive', '-Command',
        'Expand-Archive -LiteralPath $env:AIC_CHROME_ARCHIVE -DestinationPath $env:AIC_CHROME_EXTRACT -Force'],
        { stdio: 'inherit', env: { ...process.env, AIC_CHROME_ARCHIVE: path.resolve(archive), AIC_CHROME_EXTRACT: extracted } })
    } else {
      execFileSync('unzip', ['-q', path.resolve(archive), '-d', extracted], { stdio: 'inherit' })
    }
    const payload = path.join(extracted, `chrome-${asset.platform}`)
    fs.cpSync(payload, path.join(prepared, key), { recursive: true, verbatimSymlinks: true })
    fs.writeFileSync(path.join(prepared, key, '.aic-browser.json'), JSON.stringify({
      version: config.version, target: key, sha256: asset.sha256,
    }) + '\n')
    bundle.assertBrowserBundle(prepared, platform, arch, config)
    if (fs.existsSync(dest)) fs.renameSync(dest, backup)
    try { fs.renameSync(path.join(prepared, key), dest) } catch (error) {
      if (fs.existsSync(backup)) fs.renameSync(backup, dest)
      throw error
    }
    console.log(`[sync-browser] Chrome ${config.version} → ${dest}`)
    return bundle.assertBrowserBundle(root, platform, arch, config)
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true })
    fs.rmSync(prepared, { recursive: true, force: true })
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const args = process.argv.slice(2), options = {}
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--force') options.force = true
    else if (['--platform', '--arch', '--root', '--asset'].includes(args[i]) && args[i + 1])
      options[args[i++].slice(2).replace('asset', 'archive')] = args[i]
    else throw new Error(`Unknown/incomplete option: ${args[i]}`)
  }
  await syncBrowser(options)
}
