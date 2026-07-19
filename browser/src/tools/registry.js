/**
 * registry.js — Tool registry (name → handler mapping)
 */

export function createRegistry() {
  const tools = new Map();

  return {
    register(name, def, handler) {
      tools.set(name, { def, handler });
    },

    get(name) {
      return tools.get(name);
    },

    getDefs() {
      const defs = [];
      for (const [_, t] of tools) {
        defs.push({
          name: t.def.name,
          description: t.def.description,
          parameters: t.def.parameters,
          required_level: t.def.requiredLevel,
          policy_version: t.def.policyVersion || "1",
        });
      }
      return defs;
    },

    all() {
      return tools;
    },
  };
}
