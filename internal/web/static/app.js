// Canvas Clash client glue.
//
// The grid is a <canvas> drawn from a packed byte array (one byte per cell;
// 0 = empty, 1..16 = palette color + 1). The server ships the canvas as
// DataStar signals:
//
//   on connect:  bmp (base64), locks ([[x,y],...])
//   live flush:  deltas ([[x,y,color],...]), lockAdds, lockRemoves
//
// A single DataStar effect calls pb.render() whenever any of those signals
// change; pb.render diffs against the previous values and draws only what
// changed. Time travel is client-side: the slider bisects a manifest fetched
// once from GET /history and redraws the canvas — no per-tick server round trip.

const PALETTE = ["#ffffff", "#e4e4e4", "#888888", "#222222", "#ffb470", "#9a6324", "#800000", "#ba2d2d",
                 "#ffd600", "#808000", "#469990", "#42d4f4", "#4363d8", "#000075", "#f032e6", "#fabed4"];
const PALETTE_RGB = PALETTE.map(h => [parseInt(h.slice(1, 3), 16), parseInt(h.slice(3, 5), 16), parseInt(h.slice(5, 7), 16)]);
const EMPTY_RGB = [0x1c, 0x1c, 0x1c];

const pb = {};
window.pb = pb;

let canvas, ctx, cols, rows;
let olCanvas, olCtx; // lock overlay: yellow border boxes drawn above the bitmap
let liveBmp = null;      // Uint8Array W*H (bitmap values 0..16)
let lockSet = new Set(); // "x,y" strings currently locked
let scrubEvents = null;  // [{ts,x,y,c}] sorted ascending, from GET /history
let scrubbing = false;
let currentClaimId = null;

// Reference tracking so each signal value is applied exactly once (a flush
// that changes lockAdds but not deltas must not re-apply the stale deltas).
let lastBmp = null, lastDeltas = null, lastLockAdds = null, lastLockRemoves = null, lastLocks = null;

function init() {
  canvas = document.getElementById('grid');
  ctx = canvas.getContext('2d');
  cols = canvas.width;
  rows = canvas.height;
  liveBmp = new Uint8Array(cols * rows);

  // Lock overlay: same box as #grid but at device-pixel resolution, so a
  // cell is cellW×cellW overlay px and a strokeRect can render a border box.
  olCanvas = document.getElementById('lock-overlay');
  if (olCanvas) {
    olCtx = olCanvas.getContext('2d');
    sizeOverlay();
    window.addEventListener('resize', () => { sizeOverlay(); renderLocks(); });
  }

  // Initial state from SSR (instant first paint, no flash).
  const initEl = document.getElementById('initial-state');
  if (initEl) {
    try {
      const s = JSON.parse(initEl.textContent);
      if (s && typeof s.bmp === 'string') {
        const bytes = atob(s.bmp);
        for (let i = 0; i < bytes.length; i++) liveBmp[i] = bytes.charCodeAt(i);
        lastBmp = s.bmp;
        renderFull();
      }
      if (Array.isArray(s.locks)) {
        lockSet = new Set(s.locks.map(([x, y]) => `${x},${y}`));
        renderFull();
      }
    } catch (e) {
      console.warn('initial state parse failed', e);
    }
  }

  canvas.addEventListener('click', onCanvasClick);

  fetch('/history')
    .then(r => r.json())
    .then(h => { scrubEvents = h.events || []; })
    .catch(e => console.warn('history fetch failed', e));
}

// ---- input: click -> claim (two-phase: claim, then confirm/cancel in HUD) ----

function onCanvasClick(evt) {
  if (scrubbing) return; // no claims while time-traveling
  const rect = canvas.getBoundingClientRect();
  const x = Math.floor((evt.clientX - rect.left) / rect.width * cols);
  const y = Math.floor((evt.clientY - rect.top) / rect.height * rows);
  if (x < 0 || y < 0 || x >= cols || y >= rows) return;
  claim(x, y);
}

async function claim(x, y) {
  const color = Math.floor(Math.random() * 16); // TODO: palette picker
  const res = await fetch('/claim', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ x, y, color }),
  });
  if (!res.ok) {
    console.warn('claim rejected', res.status, await res.text());
    return;
  }
  const { claimId } = await res.json();
  currentClaimId = claimId;
  const hud = document.getElementById('hud');
  hud.innerHTML = `
    <span class="countdown">confirm within 3s…</span>
    <button onclick="pb.confirm()">paint it</button>
    <button onclick="pb.cancel()">cancel</button>`;
}

pb.confirm = async function () { await resolveClaim('/confirm'); };
pb.cancel = async function () { await resolveClaim('/cancel'); };

async function resolveClaim(path) {
  if (!currentClaimId) return;
  const id = currentClaimId;
  currentClaimId = null;
  document.getElementById('hud').innerHTML = '';
  await fetch(path, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ claimId: id }),
  });
}

// ---- canvas drawing (1px per cell; CSS scales up with pixelated rendering) ----

function fillCell(x, y) {
  const v = liveBmp[y * cols + x];
  const rgb = v ? PALETTE_RGB[(v - 1) % 16] : EMPTY_RGB;
  ctx.fillStyle = `rgb(${rgb[0]},${rgb[1]},${rgb[2]})`;
  ctx.fillRect(x, y, 1, 1);
}

function renderFull() {
  const img = ctx.createImageData(cols, rows);
  const d = img.data;
  for (let i = 0; i < liveBmp.length; i++) {
    const v = liveBmp[i];
    const rgb = v ? PALETTE_RGB[(v - 1) % 16] : EMPTY_RGB;
    const o = i * 4;
    d[o] = rgb[0]; d[o + 1] = rgb[1]; d[o + 2] = rgb[2]; d[o + 3] = 255;
  }
  ctx.putImageData(img, 0, 0);
  renderLocks();
}

// ---- lock overlay (yellow border box, transparent center) ----
// The overlay is sized to #grid's ON-SCREEN box in device pixels (not the
// grid's cell count): sizing it cols*S × rows*S would be a ~1GB canvas for a
// 1000×1000 grid. Border width adapts to the displayed cell size.

function sizeOverlay() {
  if (!olCanvas) return;
  const dpr = window.devicePixelRatio || 1;
  const r = olCanvas.getBoundingClientRect();
  olCanvas.width = Math.max(1, Math.round(r.width * dpr));
  olCanvas.height = Math.max(1, Math.round(r.height * dpr));
}

function cellW() {
  return olCanvas ? olCanvas.width / cols : 0;
}

function drawLock(x, y) {
  if (!olCtx) return;
  const cw = cellW();
  const lw = Math.max(1, Math.min(cw * 0.3, cw / 2));
  olCtx.strokeStyle = '#ffd600';
  olCtx.lineWidth = lw;
  olCtx.strokeRect(x * cw + lw / 2, y * cw + lw / 2, cw - lw, cw - lw);
}

function clearLock(x, y) {
  if (!olCtx) return;
  const cw = cellW();
  olCtx.clearRect(x * cw, y * cw, cw, cw);
}

function renderLocks() {
  if (!olCtx) return;
  olCtx.clearRect(0, 0, olCanvas.width, olCanvas.height);
  for (const key of lockSet) {
    const [x, y] = key.split(',').map(Number);
    drawLock(x, y);
  }
}

// ---- DataStar effect dispatcher (called when bmp/deltas/lock signals patch) ----

pb.render = function (bmp, deltas, lockAdds, lockRemoves, locks) {
  if (bmp && bmp !== lastBmp) {
    lastBmp = bmp;
    const bytes = atob(bmp);
    if (bytes.length === liveBmp.length) {
      for (let i = 0; i < bytes.length; i++) liveBmp[i] = bytes.charCodeAt(i);
    }
    if (!scrubbing) renderFull();
  }

  if (deltas && deltas !== lastDeltas) {
    lastDeltas = deltas;
    for (const [x, y, v] of deltas) liveBmp[y * cols + x] = v;
    if (!scrubbing) for (const [x, y] of deltas) fillCell(x, y);
  }

  if (locks !== lastLocks) {
    lastLocks = locks;
    lockSet = new Set((locks || []).map(([x, y]) => `${x},${y}`));
    if (!scrubbing) renderFull();
  }
  if (lockAdds && lockAdds !== lastLockAdds) {
    lastLockAdds = lockAdds;
    for (const [x, y] of (lockAdds || [])) lockSet.add(`${x},${y}`);
    if (!scrubbing) for (const [x, y] of (lockAdds || [])) drawLock(x, y);
  }
  if (lockRemoves && lockRemoves !== lastLockRemoves) {
    lastLockRemoves = lockRemoves;
    for (const [x, y] of (lockRemoves || [])) {
      if (!scrubbing) clearLock(x, y);
      lockSet.delete(`${x},${y}`);
    }
  }
};

// ---- time travel (client-side) ----

pb.scrub = function (tsMs) {
  if (!scrubEvents) return;
  scrubbing = true;
  if (olCtx) olCtx.clearRect(0, 0, olCanvas.width, olCanvas.height); // history view: no lock overlays
  const idx = bisect(scrubEvents, Number(tsMs));
  const tmp = new Uint8Array(cols * rows);
  for (let i = 0; i < idx; i++) {
    const e = scrubEvents[i];
    tmp[e.y * cols + e.x] = (e.c % 16) + 1;
  }
  const img = ctx.createImageData(cols, rows);
  const d = img.data;
  for (let i = 0; i < tmp.length; i++) {
    const v = tmp[i];
    const rgb = v ? PALETTE_RGB[(v - 1) % 16] : EMPTY_RGB;
    const o = i * 4;
    d[o] = rgb[0]; d[o + 1] = rgb[1]; d[o + 2] = rgb[2]; d[o + 3] = 255;
  }
  ctx.putImageData(img, 0, 0);
};

pb.live = function () {
  window.location.reload(); // simplest correct reset back to live + SSE
};

// number of events with ts <= target (first index past the cutoff)
function bisect(events, ts) {
  let lo = 0, hi = events.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (events[mid].ts <= ts) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

init();
