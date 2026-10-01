// Private browser/viewer framing. The hosts transport only moves binary packets.
export function decodeBrowserFrame(packet) {
  const bytes = packet instanceof Uint8Array ? packet : new Uint8Array(packet);
  if (bytes.byteLength < 4) throw new Error('画面数据包不完整');
  const length = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint32(0);
  if (length > 4096 || length > bytes.byteLength - 4) throw new Error('画面元数据无效');
  const header = JSON.parse(new TextDecoder().decode(bytes.subarray(4, 4 + length)));
  return {...header, bytes:bytes.subarray(4 + length)};
}
