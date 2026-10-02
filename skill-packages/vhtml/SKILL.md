---
name: vhtml
version: 0.1.0
description: vhtml browser-only HTML component framework user manual — components, script setup, bindings, props, URL prefix rules, ESM import, slots, refs, env.js, routes.js, vrouter, $data/$sys/$mod/$router, $t/$i18n, $bus, lifecycle scripts. Read this guide whenever a task involves vhtml pages, components, routing, i18n, or module-scoped concepts.
---

# vhtml Frontend Guide

`vhtml` is a browser-only HTML component runtime: no SSR, no hydration, no virtual DOM — the real DOM is the source of truth. One `.html` file is a full page or a reusable component. `vrouter` is optional. Use vhtml patterns, not Vue/React patterns.

## File Layout

```txt
ui/
  root.html   # app entry
  env.js      # module-scoped setup (optional)
  routes.js   # router-view config (optional)
  langs.json  # i18n translations
  layout/     # router layouts
  page/       # route pages
  local/      # reusable local components
```

## Components

Custom tags map to HTML files by kebab path:

```html
<user-card></user-card>       → /user/card.html
<agent-list></agent-list>     → /agent/list.html
```

Component file shape:

```html
<!DOCTYPE html>
<html>
  <head><title>Counter</title></head>
  <style>
    body { display: flex; gap: 12px; align-items: center; }
  </style>
  <body>
    <button @click="count--">-</button>
    <span>{{ count }}</span>
    <button @click="count++">+</button>
  </body>
  <script setup>
    count = 0
  </script>
</html>
```

### Styles

Component styles are automatically scoped to the component's DOM subtree; `@keyframes` names are isolated per component.

```css
.title { ... }          /* matches only elements compiled by this component */
body { ... }            /* the component host node itself (body / :root → host) */
body .title { ... }     /* ALL descendant elements, scope attribute not required — style piercing */
```

With a `body` / `:root` prefix, descendant selectors are no longer scope-restricted — they also reach runtime-created elements (`document.createElement`, third-party library DOM), which plain selectors don't match.

Nested rule blocks are scoped recursively: `@media`, `@supports`, and `@container` (size and style queries) — selectors and `@keyframes` references inside them get the same scope treatment. Other at-rules (`@font-face`, `@import`, …) pass through untouched.

### Props

Attributes on a component tag map to the child's `$data` keys (auto camelCase ↔ kebab-case):

```html
<user-card name="Tom" :age="userAge" v:score="score" disabled></user-card>
```

| form | behavior |
| ---- | -------- |
| `name="Tom"` | static value, assigned once |
| `:age="userAge"` | one-way binding from parent |
| `v:score="score"` | two-way binding |
| `disabled` (bare) | boolean `true` when the key exists in child `$data` |

### `$data` Declaration Rules (`<script setup>`)

- Bare assignment (`count = 0`, `save = () => {}`) and ESM imports register on `$data` — public, accessible from template bindings, refs, and parent imperative calls.
- `const` / `let` / `var` / `function` stay private to setup.
- Do not use bare `=` for short-lived locals; use `const` or `let`.

## Runtime Model

Bare identifier resolution order:

```text
$data → own data keys → $mod keys → $sys keys → builtins (framework globals, v-for iteration vars) → window
```

Use explicit prefixes (`$data.xxx`, `$mod.xxx`, `$sys.xxx`) when the source matters or to avoid shadowing.

### `$mod`

Module-scoped context shared by all components under the same `scoped` prefix. Not inherited through parent components.

| key | description |
| ----- | ------------- |
| `scoped` | module path prefix, e.g. `/page` (root module is `""`) |
| `$bus` | module-level EventBus |
| `$i18n` | I18n instance |
| `$t(key, params)` | translation shorthand |
| `fetch(url, options)` | scoped fetch — relative and `/`-prefixed URLs auto-prepend `scoped` |
| `restrictedFetch` | replaces `fetch` in `unsafe` mode — throws on http(s) URLs and cross-scoped paths |
| `define(key, value, opts?)` | define a module entry — see below |

Backend response headers prefixed `vhtml-` (e.g. `vhtml-debug`) are injected as custom keys on `$mod`.

**Local entries and shared (global) entries:** each module has its own `$mod` entries; on top of that there is a per-page shared layer that every module reads and writes:

- Read: a local entry wins; if the key exists only in the shared layer, `$mod.key` returns the shared value (reactive — shared updates propagate to readers in any module).
- Write by assignment `$mod.x = y`: local entry present → writes the local entry; only a shared entry exists → writes through to the shared entry (visible to every module); neither exists → creates a local private entry. Assignment is always legal and equals `define` without opts.
- `$mod.define(key, value, opts?)` = explicit **local** write: never touches the shared layer, so it can shadow a shared same-name entry (that is its purpose vs assignment). `manager.define(key, value, opts?)` (the second `env.js` argument) = explicit **shared** write. Both accept `{ get, set, writable, configurable, enumerable }`; defaults are all `true` (repeated define = overwrite, last wins). Readonly = `{ writable: false, configurable: false }` — later assignment or re-define throws a TypeError. Accessor options are supported: getters re-evaluate reactively, setters notify watchers.
- Built-in keys (`scoped`, `$bus`, `$i18n`, `$t`, `fetch`, `restrictedFetch`, `define`) are readonly — assigning or redefining them throws.
- Define shared entries inside an `env.js`: components start compiling only after env.js finishes, so templates never read a shared entry before it exists. Defining a shared entry later still works, but already-rendered templates that read-missed the key will not re-evaluate — a dev warning is logged.
- `__vhtml_dev.defines` lists every define: `{ name, target: '<scope>|$globals', opts }`.

### `$sys`

System variable pool, inherited from ancestor components via prototype chain.

| key | description |
| ----- | ------------- |
| `$router` | proxy to the nearest ancestor `<vrouter>` |
| `$emit(name, ...args)` | emit custom event to the parent's `@name` handler |

`$emit` event names must not collide with native DOM event names (`change`, `input`, `click`, ...): a parent's `@name` would attach as a native DOM listener and never receive the custom payload. This **throws** (fail-fast). Always pick non-built-in names.

### `$router`

Nearest ancestor `<vrouter>` view, local to the current router subtree.

| API | description |
| --- | --- |
| `push(to, data?)` / `replace(to, data?)` | navigate; `data: { params, query, hash }` |
| `back()` / `forward()` / `go(n)` | history navigation |
| `current` | `{ path, fullPath, params, query, hash, meta, layout }` |
| `params` / `query` | shortcuts to `current.params` / `current.query` |
| `setQuery(patch, opts?)` / `setParams(patch, opts?)` | merge/replace then navigate; `opts: { mode: 'replace' \| 'push', merge }` |
| `onChange(fn)` | subscribe to route changes, returns unsubscribe function |
| `addRoute(route)` / `addRoutes(routes)` / `resetRoutes()` | runtime route management |
| `cachedPages()` | cached-page list for tab/page-management UIs: `{ key, title, path, fullPath, isActive, active(), del() }` |
| `dropPage(key)` | destroy a cached page by cacheKey (dropping the active page remounts it ≈ refresh); returns `false` while the page is mid-mount |

Navigation is transactional: the target page is prepared first and switched in atomically; a new navigation supersedes one still in flight. Two idempotence rules: navigating to the **already-current destination**, or repeating the **target of an in-flight navigation**, is a no-op — so URL-sync patterns (`setParams`/`setQuery` inside a setup watcher) are safe even when they fire during the page's own first build. `current.params`/`current.query` already hold the target values before the page starts building, so setup can read its own route params on first run. Anchors without `href` (pure `@click` buttons) are not registered with the router and receive no `href`/`active`; an empty-string target is not a valid navigation (never resolves to the current path).

## URL Prefix Rules

Relative URLs inside a component (template and scripts) are auto-prefixed with `$mod.scoped`:

| scenario | behavior |
| -------- | -------- |
| Script `fetch('data/x.json')` | auto-prefixed (bare `fetch` handled same as `$mod.fetch`) |
| Script `fetch('/abs/x.json')` | **also prefixed** — never prepend `$mod.scoped` yourself (double prefix → 404) |
| Template `<img src="x.png">`, `:src` binding | auto-prefixed |
| **Runtime-created elements** (`document.createElement('img')`, third-party library / engine DOM) | **NOT prefixed** — use full path: `$mod.scoped + '/x.png'` or full URL |
| `http://`, `https://`, `//`, `@/` | passthrough, no prefix |

Rules:

1. Inside component code, always write **relative paths** for fetch; the runtime handles prefixing.
2. For images / media / iframe src passed to engines or third-party libraries, build absolute paths with `$mod.scoped`.
3. Bare `fetch()` in the browser console has no prefix — check the network panel for actual URLs; don't infer component behavior from console fetches.

`@/path` strips `@` and bypasses **all** prefixes (scoped, router, component). `javascript:` / `vbscript:` / `data:text/html` URLs are replaced with `about:blank`.

## ESM Import in `<script setup>`

Static imports are supported; relative paths resolve against the component's own URL:

```html
<script setup>
  import PPT from './ppt.js'   // resolves to <component-dir>/ppt.js
  ready = false
  init = async (opts) => { const p = new PPT($refs.host, opts) }
</script>
```

- Imported bindings register on `$data` (public, template-accessible).
- `/xxx` paths get `scoped` prepended; `@/xxx` strips `@`; `.js` is auto-appended.
- `.min.js` and `http://` imports are rejected with a warning — load external libraries via `<script>` tags instead.
- `await import('path')` dynamic imports are supported.
- In `unsafe` mode, all import statements are stripped.
- `unsafe` mode is a **fat-finger guard, not a security boundary**: it blocks common escape tricks, but a determined script can still break out. Never rely on it for real isolation — treat it as protection against accidental mistakes only.

## Bindings

```html
<div>{{ title }}</div>                               <!-- text interpolation -->
<img :src="avatarUrl" :title="name" />               <!-- dynamic attribute (auto URL prefix) -->
<div :class="{ active: isActive }"></div>            <!-- string / object / array all supported -->
<div :style="{ color: 'red' }"></div>
<button @click="save()">Save</button>
<button @click.stop.prevent="remove(id)">Delete</button>
<input v:value="value" />                            <!-- two-way binding -->
<div v-show="loading">Loading</div>
<div v-if="a">A</div>
<div v-else-if="b">B</div>
<div v-else>C</div>
<div v-for="item in items">{{ item.name }}</div>
<div v-for="(item, idx) in items">...</div>
<template v-for="item in items">                     <!-- multi-root list -->
  <div>{{ item.a }}</div>
  <div>{{ item.b }}</div>
</template>
<div vsrc="/local/card.html"></div>                  <!-- static component -->
<div :vsrc="currentComponent"></div>                 <!-- dynamic component -->
<div v-html="htmlContent"></div>                     <!-- raw HTML, <div> only -->
<div no-vhtml>{{ raw }}</div>                        <!-- skip compilation -->
```

- Text interpolation: function values are auto-invoked, object values are auto-`JSON.stringify`-ed.
- Event modifiers: `.stop`, `.prevent`, `.self`, `.delay[500ms|1s]`; key aliases: `space`, `esc`, `up`, `down`, `left`, `right`, `del`, `ins` (e.g. `@keyup.esc="close()"`).
- Special events: `@mounted` (node inserted into DOM), `@outerclick` (click outside the element).
- `v-for` and `v-if` can coexist on the same node: `v-for` clones first, then `v-if` filters each clone.
- `v-for` has **no `:key` attribute** — item identity is tracked automatically (objects by reference, primitives by position). A `:key` on a v-for node compiles as a plain inert attribute; delete it.
- Always initialize list variables in `<script setup>`: `items = []`.

## Script Types

| type | when it runs |
| ------ | ------------- |
| `<script setup>` | once at instance creation, before DOM compilation |
| `<script>` | once after DOM compilation, before first activation |
| `<script active>` | on entering live-in-page state: mount, cached route re-entry, browser tab visible again. Handler receives `$reason`: `'mount' \| 'route' \| 'visibility'` |
| `<script deactive>` | on leaving live-in-page state but staying alive: route cached, tab hidden; also fired before `dispose` when disposed while active (`$reason: 'dispose'`) |
| `<script dispose>` | when the instance is destroyed (`v-if` removal, page unload) |

Helpers available in all script types:

- `$node` — the current host DOM element.
- `$watch(() => expr, (val) => { ... })` — reactive watcher, auto-cleaned on dispose. In `<script setup>` the first evaluation runs after props are bound, so it already sees the incoming prop values; in other script types it starts immediately.

### Disposal Contract

- **Who removes DOM owns the dispose**: before externally removing an element that hosts vhtml content (instances / bindings), call `disposeNode(el)` (exported from `vhtml/component-instance.js`). It cleans the whole subtree and is idempotent (returns `false` when there was nothing to clean).
- The framework handles disposal at all its own removal points (`v-for` / `v-if` / `:vsrc` / `v-html` / page destroy). An external `el.remove()` without `disposeNode` is still cleaned up automatically, but logs a dev warning (`disposed via observer fallback`) — treat that warning as a bug in the removing code and switch to `disposeNode`. The warning only fires for DOM created from vhtml templates; foreign DOM (e.g. elements owned by third-party libraries) is cleaned up silently.
- Route-cached pages survive DOM removal via the instance field `keepOnDetach` (managed by the router).

## Refs and Parent-to-Child Calls

`ref="xxx"` is collected into `$data.$refs.xxx`:

```html
<script setup>
  reloadChild = () => $refs.panel.$data.reload()
</script>
<child-panel ref="panel"></child-panel>
```

Host nodes expose `$data`, `$sys`, `$mod`. Prefer `props + $emit` for normal communication; use `$refs.xxx.$data` only for imperative parent-to-child calls.

## Slots

```html
<!-- caller -->
<card-shell>
  <div vslot="header">Header</div>   <!-- projected: caller runtime -->
  <div>Body</div>                    <!-- default slot -->
</card-shell>

<!-- card-shell.html -->
<body>
  <header><vslot name="header"><span>Default</span></vslot></header>
  <main><vslot></vslot></main>       <!-- fallback: child runtime -->
</body>
```

Projected content runs in the caller's runtime (`$data`/`$sys`/`$mod`); fallback content runs in the child's own runtime. `<vslot>` supports `:name` for dynamic slot names. `<vslot vbind="a, b">` exposes the outlet component's `$data` keys `a`/`b` to the projected content (kept in sync on change).

## `env.js`

Loaded once per `scoped` prefix. Use for module-wide services, i18n, config:

```js
export default async ($mod, manager) => {
  $mod.define('config', await $mod.fetch('/config.json').then(r => r.json()))
  $mod.$i18n.load(await $mod.fetch('/langs.json').then(r => r.json()))

  await manager.loadModule('/shared')          // preload a sub-module and wait for its env.js
  manager.addAlias('uikit', '/lib/ui-kit')     // <uikit-button> → /lib/ui-kit/button.html
  manager.define('$auth', authService)         // global entry: every module reads it as $mod.$auth
}
```

- `manager.loadModule(subPath)` — preload a sub-module's `env.js`; `/`-prefixed = absolute, otherwise relative to the current scoped.
- `manager.addAlias(prefix, baseUrl, isGlobal)` — register a component path alias. `prefix` must be letters only (it matches the tag's first `-`-segment); `baseUrl` must start with `/` or `https://`. Non-global aliases only register while an `env.js` is loading; aliases resolve only in non-root modules (`scoped` ≠ `''`).
- `$mod.define(key, value, opts?)` / `manager.define(key, value, opts?)` — module-local vs shared entries (semantics see the `$mod` section).

Do NOT use `env.js` for route guards, per-page state, or component-local data.

## `routes.js`

Belongs to a `<vrouter>` view. Loaded from the current scoped module unless `vrouter` provides an explicit `routes` path.

```js
// 1. array
export default [
  { path: '/', component: '/page/index.html' },
  { path: '*', component: '/page/404.html' },
]

// 2. factory (recommended when $mod capabilities are needed)
export default ({ $mod, router }) => ({
  path_prefix: '/panel',        // default: $mod.scoped
  component_prefix: '',         // default: ''
  routes: [
    { path: '/', component: '/page/index.html', layout: 'default' },
    { path: '/user/:id', component: '/page/user.html', meta: { auth: true } },
    {
      path: '/admin',
      component: '/page/admin.html',
      layout: 'admin',
      children: [{ path: 'settings', component: '/page/admin_settings.html' }],
    },
    {
      path: '/edit/:id',
      component: (path, params) => `/page/edit/${params.id}.html`,
      redirect: '/login',                          // string | { path, params, query, hash } | (matchedRoute) => target
      error_redirect: '/404',                      // string | (matchedRoute, error) => target
    },
    { path: '*', component: '/page/404.html' },
  ],
  beforeEnter: async (to, from, next) => {
    if (!$mod.auth?.isLogin() && to.path !== '/login') {
      next('/login')
      return false
    }
  },
  afterEnter: (to, from) => { ... },
})
```

Route record fields:

| field | description |
| ------- | ------------- |
| `path` | required. `:param`, `:param?`, `*rest`, `*` (catch-all, keep last for 404) |
| `component` | required. HTML path or `(path, params) => url`; `params` includes fixed `:params` values plus matched route params |
| `layout` | layout name → `/layout/{name}.html`; layouts should expose a default `<vslot>` for the page outlet |
| `redirect` | string, `{ path, params, query, hash }`, or `(matchedRoute) => target` |
| `error_redirect` | fallback when the component fails to load |
| `meta` | arbitrary metadata, exposed on `$router.current.meta` |
| `children` | nested routes; child paths relative to parent; children inherit parent layout/meta |
| `cacheKey` | `false` (no cache) · string (shared instance) · `(matchedRoute) => key` · default: path-based, query/hash excluded (query changes update router state, page DOM kept) |

`beforeEnter` / `afterEnter` belong in `routes.js`, not `env.js`.

## `vrouter`

```html
<vrouter></vrouter>
<vrouter history="memory" initial="/list"></vrouter>
<vrouter history="memory" prefix="/panel" initial="/list"></vrouter>
<vrouter history="panelA"></vrouter>
<vrouter routes="/admin_routes.js"></vrouter>
<vrouter :routes="routes"></vrouter>
<vrouter :routes="{ routes, path_prefix: '/panel', component_prefix: '/panel-ui', beforeEnter }"></vrouter>
<vrouter :routes="routes" :params="{ app_id: appId }"></vrouter>
```

- Without `routes`, loads the current scoped `routes.js`. `routes` may be a module URL, or `:routes` may bind an array / route-module object directly.
- `:params` injects fixed values into `$router.params`, guard `to.params`, and `component(path, params)` functions; matched path params override same-key fixed params.
- `history`: default = browser routing (`window.location` + `window.history`); `"memory"` = isolated virtual history starting at `initial`; any other value resolves a named history registered via `registerRouterHistory(name, history)`.
- Multiple `<vrouter>` instances per page are allowed.
- Navigation prefix priority: `$router.prefix` > initiating component `$mod.router_prefix` > initiating component `$mod.scoped`.
- Route registration prefixes come from route-module `path_prefix` / `component_prefix`, not from `prefix`.
- `@/path` bypasses router normalization and resolves to `/path`; `http(s)://` links are not intercepted.
- `<a>` is intercepted only when compiled under a RouterView runtime, with automatic `active` attribute on path match.
- Virtual routers inject bare `location` / `history` into `$sys`; outside a virtual router those names fall through to `window`. Virtual histories do not update `document.title`.
- Debug logging: `localStorage.debug`.

## `$bus`

Module-level EventBus with wildcards:

```js
const off = $mod.$bus.on('user.updated', fn)   // returns unsubscribe function
$mod.$bus.on('user.*', fn)                     // * = exactly one token
$mod.$bus.on('order.>', fn)                    // > = zero or more trailing tokens (must be last)
$mod.$bus.emit('user.updated', payload)
$mod.$bus.emit('@.global.event', data)         // @. prefix: broadcast to other modules only, not local
$mod.$bus.once('ready', fn)
$mod.$bus.emitLocal('evt', data)               // local only, never broadcasts
$mod.$bus.off('evt', fn)
```

## i18n

`$i18n` and `$t` live on `$mod` — translations are module-scoped by default.

```html
<title>{{ $t('page.title') }}</title>
<button>{{ $t('common.save') }}</button>
```

`langs.json`:

```json
{ "zh-CN": { "common.save": "保存" }, "en-US": { "common.save": "Save" } }
```

| API | description |
| --- | --- |
| `$t(key, { count, ...vars })` | translate; `{{var}}` interpolation, `.zero` / `.one` / `.other` plural forms |
| `$i18n.setLocale(lang)` / `getLocale()` | current locale, shared across modules in the page |
| `$i18n.load(messages, merge = true)` | load translations; `merge: false` replaces |
| `$i18n.d(date, opts?)` | `Intl.DateTimeFormat` |
| `$i18n.n(num, opts?)` | `Intl.NumberFormat` |
| `$i18n.c(num, currency, opts?)` | currency formatting |
| `$i18n.rtf(value, unit, opts?)` | relative time ("3 days ago") |
| `$i18n.has(key, locale?)` / `getLocales()` | key existence / loaded locale list |

Key scanning via CLI:

```bash
vhtml i18n scan                    # scan, clean up, report missing keys
vhtml i18n add -json '{"zh-CN":{"k":"v"},"en-US":{"k":"v"}}'
```

### Reserved keys (`_` prefix)

Keys starting with `_` (`_err.40100`, `_theme.dark`) are maintained manually in langs.json: scan skips them for missing/unused checks, `--autoremove` never deletes them. Use for dynamic keys referenced via concatenation, variables, or function args (not exact string literals).

## Web Components: No Interop

vhtml does **not** special-case native Web Components. A tag containing `-` is always an internal vhtml component, loaded and compiled by vhtml.

To embed a third-party WC, keep it **outside vhtml's compilation scope** and handle it yourself: a `no-vhtml` region (compilation skipped — set attributes / append children from a setup script or via `v-html` content), or plain manual DOM (`document.createElement` + `customElements` registration in your own code).

## Tooling: `vhtml check`

Static template check for AI-written-UI workflows. Every candidate expression in the template is compiled for verification only — nothing is executed, zero side effects. Requires Node.js; when Node is unavailable the command fails explicitly (exit `2`), never a silent pass.

```bash
vhtml check [path...]            # default "."; skips node_modules/dist/.git, collects *.html
vhtml check --json               # findings as a JSON array (agent consumption)
```

**Input scope:** `*.html` templates only — `.js` files are not supported inputs (JS comments are not masked, so a `<tag>`-shaped literal inside a comment is reported as a structure finding). Config/env JS is covered indirectly via the `<script>` blocks of HTML files.

**Exit codes (stable contract):** `0` = no findings; `1` = findings (E or W); `2` = tool failure (e.g. Node.js missing) — explicit error, never silent pass.

**Output (text):** `<file>:<line>:<col> [E|W] <kind>: <message>`; JSON: `[{file,line,col,severity,kind,message}]`.

**Kinds:** `syntax` (interpolation `{{ }}`, `:bind`, `@handler`, `v-if/else-if/show/html` RHS, `v-for` RHS, inline `<script>` blocks), `vfor` (malformed LHS/RHS), `directive` (unknown `v-` attr — catches `v-fo` typos; bare `@evt` without handler), `vslot-pair`, `structure` (tag balance; legal tag omissions like `li`/`p` are accepted), `io`.

**Ability boundary (do not over-promise):** syntax and structure only — no semantic validation, no undefined-identifier detection (undefined names warn once at runtime). Modifier-only handlers (`@click.stop`) are legal. Files containing Go-template syntax (`{{.`) are skipped wholesale (server-templated sources; only the rendered output can be checked).

## Reactivity Contract & Pitfalls

Nested objects are reactive — no opt-in needed. Updates are batched: multiple writes within the same task collapse into one refresh. Writes made **during a watcher's own evaluation are ignored** (feedback-loop guard).

**Change gate**: a watcher callback fires only when the value actually changed (`Object.is` comparison). Pass `{ equality: null }` to `$watch` for an always-run subscription. Known boundary: a template expression that returns a fresh reference on every evaluation (e.g. `items.filter(...)`) always passes the gate — avoid allocating inside template expressions.

**Pure-replacement writes**: assigning to a reactive key replaces the value outright — no deep merge, and the new value gets a fresh identity (v-for position-keyed row reuse is the one exception — see below).

**Array mutators**: all standard mutators (`push / pop / splice / shift / unshift / sort / reverse / copyWithin / fill`) are safe on reactive arrays; their notifications collapse into one refresh. In-place mutation keeps v-for row identity.

### v-for item identity (no `:key`)

v-for tracks items automatically; there is no `:key` attribute (it compiles as a plain inert attribute — delete it):

| list content | identity | update behavior |
| ------------ | -------- | --------------- |
| object items with stable references | the item itself | mutate fields → in-place patch, DOM kept · `list[i] = {...}` replaces the entry → destroyed & rebuilt · wholesale `list = [new objects]` → all entries destroyed & rebuilt (transient state like focus is lost) |
| fresh objects without stable references (e.g. a function source like `v-for="tr in tracks()"`) | array position | same shape (equal top-level key set) → merged in place, DOM kept · shape change → entry destroyed & rebuilt |
| primitive items (string/number) | array position | value changes patch in place, DOM kept |
| the same object twice in one list | — | both entries bind the same record; avoid |

Structural edits (insert / remove / reorder): either in-place mutators (`splice` / `unshift` / `sort`) or copy-then-assign (`slice()` / spread + assign back) — both are safe; kept items retain identity, so their DOM is preserved and physically re-ordered.

### Other rules

1. Mutating a nested object through a held **raw reference** (`const msg = {...}` then `msg.text = x`) bypasses reactivity — no update. Always write through the reactive path: `d.list[i].text = x`.
2. For streaming/animation (typewriter, count-up): drive from top-level scalar `$data` props, not nested object fields; lists should be append-only immutable records.
3. Writes from within a reactive evaluation (watchers, binding expressions) do not notify — mutate state from event handlers, timers, or rAF callbacks instead.
4. A runaway feedback loop (a callback writing its own dependency every round) aborts after 10 rounds in one refresh, throwing an error — check `window.__vhtml_dev.cascadeErrors` for the effect chain.
5. Errors are exposed, never silent: template compilation failures throw; a component that fails to mount renders a visible red `[vhtml] ... failed` placeholder instead of blank space; every compile/expression/mount error is recorded in `window.__vhtml_dev.errors` (newest last) with code preview and component location. Undefined identifiers read inside sandboxed code warn once per name (spelling check).

#### Compile stats (`__vhtml_dev.compileStats`)

Counters for compile-vs-render profiling: `nodeCompiles` / `nodeMs` (DOM-compile calls and self time), `codeCompiles` / `codeMs` (expression compiles, including cache hits), `vforLines` (new v-for rows).

## Debug

`localStorage.debug = 1` enables verbose logs (router navigation, module loading). Warnings and errors always print regardless.

## Example

```html
<!DOCTYPE html>
<html>
  <head><title>Home</title></head>
  <style>.card { border: 1px solid #ddd; padding: 16px; margin: 8px 0; }</style>
  <body>
    <h1>{{ $t('page.title') }}</h1>
    <input v:value="keyword" placeholder="Search" />
    <div v-if="loading">Loading</div>
    <div v-else>
      <div v-for="item in list" class="card">
        <h3>{{ item.name }}</h3>
        <button @click="remove(item.id)">Delete</button>
      </div>
    </div>
  </body>
  <script setup>
    keyword = ''
    list = []
    loading = true

    const load = async () => {
      const res = await fetch('/api/list')   // auto-prefixed with scoped
      list = await res.json()
      loading = false
    }

    remove = (id) => {
      list = list.filter(item => item.id !== id)
      console.log('Deleted')
    }

    load()
  </script>
</html>
```
