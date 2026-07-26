// auth.js 审批指纹测试（§10.1 第 4 条）
import { test } from "node:test";
import assert from "node:assert/strict";
import { approvalInputHash, verifyApprovalFingerprint } from "./auth.js";

// 固定向量：与 Go sdk/fingerprint.go 及 aic types.ApprovalInputHash 必须一致
test("approvalInputHash 固定向量（跨实现一致）", async () => {
  assert.equal(await approvalInputHash("cloud", "read", ["/a", "--limit", "20"]), "5ff51decf470e46a");
  assert.equal(await approvalInputHash("host_abc", "write", ["/a.txt", "--content", "hi"]), "aec0feb022efb7b1");
  assert.equal(await approvalInputHash("host_abc", "rm", ["/tmp/x"]), "7798623f326a4ad3");
});

test("verifyApprovalFingerprint", async () => {
  const hash = await approvalInputHash("host_abc", "write", ["/a.txt", "--content", "hi"]);
  const fp = `fp:sess1:fs:1:${hash}`;
  const toolData = { action: "write", argv: ["/a.txt", "--content", "hi"] };
  assert.equal(await verifyApprovalFingerprint(fp, "sess1", "fs", "host_abc", toolData), true);
  // 改参数 → 不符
  assert.equal(await verifyApprovalFingerprint(fp, "sess1", "fs", "host_abc", { action: "write", argv: ["/b.txt"] }), false);
  // 不同 target → 不符
  assert.equal(await verifyApprovalFingerprint(fp, "sess1", "fs", "host_xyz", toolData), false);
  // 格式错误
  assert.equal(await verifyApprovalFingerprint("bad", "sess1", "fs", "host_abc", toolData), false);
});
