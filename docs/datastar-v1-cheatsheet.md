# DataStar v1 cheat sheet (verified against data-star.dev + v1.0.2 bundle)

Fetched 2026-08-26 from https://data-star.dev/ (guide + references + how-tos).
Bundle in use: `starfederation/datastar@v1.0.2` `bundles/datastar.js` (34KB,
plugins: on, init, signals, computed, show, text, class, attr, style, bind,
ref, effect, peek, indicator, json-signals, on-interval, on-intersect,
on-signal-patch). Load as `<script type="module">`.

## Corrections vs what we assumed earlier

- **v0 API is gone**: no `data-sse-connect`. Streams = long-lived GET whose
  response is `text/event-stream`: `data-init="@get('/sse')"`.
- **Attribute syntax is colon-based**: `data-on:click`, not `data-on-click`.
- **Globals ARE callable** in expressions (`claimClick(evt)`); event var is
  **`evt`** (not `$event`). Our original click failure was just the 404 bundle.
- **Auto-reconnect exists**: action defaults `retry:'auto'`,
  retryInterval 1s → ×2 backoff → maxWait 30s, maxCount 10.
- **Hidden tabs close streams by default**, reopen when visible again
  (initial sync covers the gap). `{openWhenHidden: true}` keeps them open.

## Attributes we'll actually use

| Attribute | Notes |
|---|---|
| `data-signals="{color: 0}"` | declare; dot-paths nest; `_`-prefix = local only |
| `data-bind:color` | two-way input binding (palette picker!) |
| `data-on:click="expr"` | modifiers: `__once __prevent __stop __window __document __outside __debounce.250ms __throttle.100ms ...` |
| `data-init="@get('/sse')"` | runs at load AND when element is patched in |
| `data-text="$expr"` / `data-show="$expr"` / `data-class:selected="$color == 3"` / `data-attr:disabled="$expr"` | reactive DOM |
| `data-indicator:fetching` | true while fetch in flight — must appear BEFORE data-init |
| `data-computed:x="expr"` | derived signal, no side effects (use data-effect for those) |
| `data-on-interval__duration.5s="@get('/metrics')"` | polling (dashboard) |
| `data-on-signal-patch-filter="{include:/^counter$/}"` | react to specific backend signal patches |

## Actions & implicit signals

- `@get/@post/@put/@patch/@delete(url, opts)`. Every request carries
  `Datastar-Request: true` + ALL non-underscore signals (GET → `datastar`
  query param as JSON; others → JSON body). Server reads via
  `datastar.ReadSignals(r, &out)`. So a palette picker needs zero JS glue:
  swatches set `$color`, claim posts carry it automatically.
- Options that matter: `contentType:'json'|'form'`, `headers:{}`,
  `filterSignals:{include:/…/}`, `openWhenHidden:true`,
  `requestCancellation:'auto'|'cleanup'|'disabled'` (**auto cancels any
  in-flight request to same URL+method globally**), `payload`.
- Response handling by content-type: `text/event-stream` → stream,
  `text/html` → morph patch, `application/json` → signal patch,
  `text/javascript` → execute.

## SSE wire format (v1 has exactly TWO event types)

```
event: datastar-patch-elements
data: selector #c-5-7        # optional; outer/morph default targets by id
data: mode outer|inner|replace|prepend|append|before|after|remove
data: elements <div ...>
data: elements ...
<blank line>
```

```
event: datastar-patch-signals
data: signals {"countdown": 3}     # JSON Merge Patch RFC 7396; null removes
<blank line>
```

Two newlines after every event. Morph guidance: put IDs on top-level patched
elements AND inner elements whose state should survive.

## Patterns worth stealing

- **Backend redirect**: patch `<script>window.location.href=...` appended to
  body (no dedicated redirect event).
- **Fat-morph/CQRS resilience**: send complete state per update so reopened
  streams self-heal. Our initial-sync batch is exactly this; per-cell patches
  are incremental — acceptable since reconnect re-runs initial sync.
- **FOUC warning**: `data-show` needs inline `style="display:none"` initially.
- Structured console errors on bad usage: `Uncaught datastar runtime error:
  <code>` with a docs URL — check console first when attributes don't fire.

## TODOs unlocked for PixelBeetle

1. Add `{openWhenHidden:true}` to our `/sse` @get (demo presenter switches tabs).
2. Palette picker via `data-signals` + swatch `data-on:click="$color = n"`;
   migrate app.js fetch to read `$color`, or move claim POST into DataStar.
3. HUD countdown via server `PatchSignals({"countdownLeft": ms})` or local
   `data-effect` timer; buttons via `data-show="$claimId !== null"`.
