'use strict'

const test = require('node:test')
const assert = require('node:assert/strict')
const { capturePlatform } = require('./screenshot.cjs')

function fixture() {
  const image = {
    isEmpty: () => false,
    getSize: () => ({ width: 2880, height: 1800 }),
    toDataURL: () => 'data:image/png;base64,abc',
  }
  // WebContentsView 自身没有 isDestroyed()，只有 webContents 有。
  return {
    webContents: {
      isDestroyed: () => false,
      capturePage: async () => image,
    },
  }
}

test('capturePlatform returns real dimensions and a PNG data URL', async () => {
  assert.deepEqual(await capturePlatform(fixture()), {
    data_url: 'data:image/png;base64,abc', width: 2880, height: 1800,
  })
})

test('capturePlatform rejects a destroyed view', async () => {
  const view = fixture()
  view.webContents.isDestroyed = () => true
  await assert.rejects(capturePlatform(view), /unavailable/)
})

test('capturePlatform rejects an empty image', async () => {
  const view = fixture()
  view.webContents.capturePage = async () => ({
    isEmpty: () => true,
    getSize: () => ({ width: 0, height: 0 }),
    toDataURL: () => '',
  })
  await assert.rejects(capturePlatform(view), /empty platform capture/)
})
