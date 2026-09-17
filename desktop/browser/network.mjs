// Passive CDP history. No fetch/XHR patching in the inspected page.
export function createEventLog(limit = 500) {
  const network = new Map(), console = [];
  function event(method, p) {
    if (method === 'Network.requestWillBeSent') {
      network.set(p.requestId, { id: p.requestId, url: p.request.url, method: p.request.method, type: p.type, started_at: p.wallTime });
      if (network.size > limit) network.delete(network.keys().next().value);
    } else if (method === 'Network.responseReceived') {
      Object.assign(network.get(p.requestId) || {}, { status: p.response.status, mime: p.response.mimeType });
    } else if (method === 'Network.loadingFailed') {
      Object.assign(network.get(p.requestId) || {}, { error: p.errorText });
    } else if (method === 'Runtime.consoleAPICalled') {
      console.push({ type: p.type, text: p.args.map(a => String(a.value ?? a.description ?? a.type)).join(' '), timestamp: p.timestamp });
      if (console.length > limit) console.shift();
    } else if (method === 'Runtime.exceptionThrown') {
      console.push({ type: 'exception', text: p.exceptionDetails.exception?.description || p.exceptionDetails.text, timestamp: p.timestamp });
      if (console.length > limit) console.shift();
    }
  }
  return { event, read(family, id) { return family === 'console' ? [...console] : id ? network.get(id) : [...network.values()]; }, clear(family) { if (family === 'console') console.length = 0; else network.clear(); } };
}
