#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import crypto from 'node:crypto'
import {pipeline} from 'node:stream/promises'
import {Readable} from 'node:stream'
import {fileURLToPath, pathToFileURL} from 'node:url'
const desktop = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
export const manifest = JSON.parse(fs.readFileSync(path.join(desktop, 'agent-browser.json')))
const binary = platform => platform === 'win32' ? 'agent-browser.exe' : 'agent-browser'
const digest = file => crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex')
export function assertAgentBrowserBundle(dir, platform, arch) {
  const asset = manifest.assets[`${platform}-${arch}`]
  const stamp = JSON.parse(fs.readFileSync(path.join(dir, '.aic-agent-browser.json')))
  if (!asset || stamp.version !== manifest.version || stamp.sha256 !== asset.sha256)
    throw new Error(`agent-browser version mismatch: ${dir}`)
  for (const file of [binary(platform), 'LICENSE'])
    if (!fs.statSync(path.join(dir, file)).isFile()) throw new Error(`agent-browser resource missing: ${file}`)
  return path.join(dir, binary(platform))
}
export async function syncAgentBrowser({platform=process.platform, arch=process.arch,
  root=path.join(desktop, 'vendor/agent-browser')}={}) {
  const key = `${platform}-${arch}`, asset = manifest.assets[key]
  if (!asset) throw new Error(`Unsupported agent-browser target: ${key}`)
  const dest = path.join(root, key)
  try { return assertAgentBrowserBundle(dest, platform, arch) } catch {}
  fs.mkdirSync(root, {recursive:true})
  const stage = fs.mkdtempSync(path.join(root, '.sync-'))
  try {
    const response = await fetch(`https://github.com/${manifest.repo}/releases/download/${manifest.tag}/${asset.file}`, {signal:AbortSignal.timeout(300000)})
    if (!response.ok) throw new Error(`agent-browser download: HTTP ${response.status}`)
    const file = path.join(stage, binary(platform))
    await pipeline(Readable.fromWeb(response.body), fs.createWriteStream(file))
    if (digest(file) !== asset.sha256) throw new Error(`agent-browser SHA-256 mismatch: ${key}`)
    fs.chmodSync(file, 0o755)
    const license = await fetch(`https://raw.githubusercontent.com/${manifest.repo}/${manifest.tag}/LICENSE`, {signal:AbortSignal.timeout(30000)})
    if (!license.ok) throw new Error(`agent-browser license: HTTP ${license.status}`)
    fs.writeFileSync(path.join(stage,'LICENSE'), await license.text())
    fs.writeFileSync(path.join(stage,'.aic-agent-browser.json'), JSON.stringify({version:manifest.version,sha256:asset.sha256})+'\n')
    assertAgentBrowserBundle(stage, platform, arch)
    fs.rmSync(dest,{recursive:true,force:true})
    fs.renameSync(stage,dest)
    console.log(`[agent-browser] ${manifest.version} → ${dest}`)
    return path.join(dest,binary(platform))
  } finally { fs.rmSync(stage,{recursive:true,force:true}) }
}
if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) await syncAgentBrowser()
