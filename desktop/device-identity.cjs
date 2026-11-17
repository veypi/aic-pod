'use strict'

const crypto = require('node:crypto')
const fs = require('node:fs')
const path = require('node:path')

// device_uuid 仅用于首次 ensure 的创建参考；它不是 Host 身份，也不是凭证。
function createDeviceIdentity({ file, name }) {
  let value = null
  return {
    name,
    async uuid() {
      if (value) return value
      try {
        const existing = (await fs.promises.readFile(file, 'utf8')).trim()
        if (crypto.randomUUID && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(existing)) {
          value = existing.toLowerCase()
          return value
        }
      } catch (error) {
        if (error.code !== 'ENOENT') throw error
      }
      value = crypto.randomUUID()
      await fs.promises.mkdir(path.dirname(file), { recursive: true, mode: 0o700 })
      const temp = file + '.tmp-' + process.pid
      await fs.promises.writeFile(temp, value + '\n', { mode: 0o600 })
      await fs.promises.rename(temp, file)
      return value
    },
  }
}

module.exports = { createDeviceIdentity }
