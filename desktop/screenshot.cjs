'use strict'

// 平台页真实位图。只负责 Electron 原语，调用方负责平台 frame 身份校验。
async function capturePlatform(platformView) {
  if (!platformView || platformView.webContents.isDestroyed()) {
    throw new Error('platform view unavailable')
  }
  const image = await platformView.webContents.capturePage()
  if (image.isEmpty()) throw new Error('empty platform capture')
  const size = image.getSize()
  if (!size.width || !size.height) throw new Error('empty platform capture')
  return { data_url: image.toDataURL(), width: size.width, height: size.height }
}

module.exports = { capturePlatform }
