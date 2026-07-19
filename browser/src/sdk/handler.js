/**
 * handler.js — Tool request signature verification and dispatch
 */

import { verifyToolRequestSig } from "./auth.js";

/**
 * Create a request handler that verifies signatures and dispatches
 * to registered tools.
 */
export function createHandler(envID, kTool, tools) {
  return async function handleToolRequest(req, respond) {
    // Verify signature
    if (!req.sig) {
      respond({ status: "rejected", error: "missing request signature" });
      return;
    }

    const valid = await verifyToolRequestSig(req, envID, kTool);
    if (!valid) {
      respond({ status: "rejected", error: "invalid request signature" });
      return;
    }

    // Check deadline
    if (req.deadline) {
      if (Date.now() > new Date(req.deadline).getTime()) {
        respond({ status: "rejected", error: "request deadline exceeded" });
        return;
      }
    }

    // Find tool
    const tool = tools.get(req.tool_name);
    if (!tool) {
      respond({ status: "error", error: `unknown tool: ${req.tool_name}` });
      return;
    }

    // Check permission
    if (req.granted_level < tool.def.requiredLevel && !req.approval) {
      respond({
        status: "rejected",
        error: `insufficient permission: ${req.tool_name} requires level ${tool.def.requiredLevel}`,
      });
      return;
    }

    // Build context
    const ctx = {
      grantedLevel: req.granted_level,
      approved: !!req.approval,
      resolvedBy: req.approval?.resolved_by || "",
      sessionID: req.session_id,
      msgID: req.msg_id,
    };

    // Execute
    try {
      const result = await tool.handler(ctx, req.tool_data);
      const resp = {
        msg_id: req.msg_id,
        content: result.content || "",
        error: result.error || "",
        attrs: result.attrs || {},
      };

      switch (result.status) {
        case "rejected": resp.status = "rejected"; break;
        case "waiting": resp.status = "waiting"; break;
        default:
          resp.status = result.error ? "error" : "completed";
      }
      respond(resp);
    } catch (err) {
      respond({ msg_id: req.msg_id, status: "error", error: err.message });
    }
  };
}
