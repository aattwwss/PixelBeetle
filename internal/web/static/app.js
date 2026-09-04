// PixelBeetle client glue — the paint view.
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
// changed. History/time-travel lives on its own page (/history) so this view
// never pays for it — see history.js.
//
// Painting interaction — everything happens at the pixel, nothing to hunt in
// the header:
//   hover            → ghost preview of the selected color on that cell
//   click            → arm the cell (claim it; 3s window; outlined in the
//                      selected color, distinct from others' yellow locks)
//   click it again   → paint it (confirm)
//   click elsewhere  → re-arm that cell instead (server supersedes)
//   Esc              → cancel the armed claim
//   right-click      → eyedropper: pick that pixel's color
//   1-9,0,a-f keys   → palette shortcuts; picking a color while armed
//                      re-claims the same cell so it paints the new color

const pb = {};
window.pb = pb;

let canvas, ctx, cols, rows;
let olCanvas, olCtx; // lock overlay: border boxes drawn above the bitmap
let ghostCanvas, ghostCtx; // hover preview: translucent ghost of the chosen color
let liveBmp = null;      // Uint8Array W*H (bitmap values 0..16)
let lockSet = new Set(); // "x,y" strings currently locked
let currentClaimId = null;
let pendingCell = null; // {x, y} of the cell we hold a claim on, if any
let ghostCell = null;   // {x, y} under the cursor, if any
let flash = null;       // {x, y, until} rejected-claim red flash
let toastTimer = null;

// clearHud drops the armed state + toast — but only if `claimId` is still the
// claim that owns the HUD (a stale expiry timer or unlock for a superseded
// claim must not clobber a newer one).
function clearHud(claimId) {
  if (currentClaimId !== claimId) return;
  currentClaimId = null;
  pendingCell = null;
  renderLocks();
  document.getElementById('hud').innerHTML = '';
}

// One-shot toast; replaces any previous one. `ms` auto-clears it.
function toast(msg, ms = 2400) {
  const hud = document.getElementById('hud');
  if (!hud) return;
  hud.innerHTML = `<span class="countdown">${msg}</span>`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { hud.innerHTML = ''; }, ms);
}

// Reference tracking so each signal value is applied exactly once (a flush
// that changes lockAdds but not deltas must not re-apply the stale deltas).
let lastBmp = null, lastDeltas = null, lastLockAdds = null, lastLockRemoves = null, lastLocks = null;

// ---- palette color picker ----

let selectedColor = 1; // palette index 0..15 used for the next claim

// Build the 16 swatch buttons from PALETTE_RGB (same source of truth as the
// canvas colors, so the picker can never drift from what gets painted).
function buildPalette() {
  const bar = document.getElementById('palette');
  if (!bar) return;
  bar.innerHTML = '';
  PALETTE.forEach((hex, i) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'swatch' + (i === selectedColor ? ' selected' : '');
    b.style.background = hex;
    b.title = hex;
    b.addEventListener('click', () => selectColor(i));
    bar.appendChild(b);
  });
}

window.selectColor = function (i) {
  selectedColor = i;
  document.querySelectorAll('#palette .swatch').forEach((el, idx) => {
    el.classList.toggle('selected', idx === i);
  });
  drawGhost();
  // Keep an armed claim in sync with the picker: re-arming the same cell with
  // the new color is safe — the server supersedes the previous pending claim.
  if (pendingCell) claim(pendingCell.x, pendingCell.y, { quiet: true });
};

function init() {
  canvas = document.getElementById('grid');
  ctx = canvas.getContext('2d');
  cols = canvas.width;
  rows = canvas.height;
  liveBmp = new Uint8Array(cols * rows);

  // Overlays: same box as #grid but at device-pixel resolution, so a cell is
  // cellW×cellW overlay px and stroke/fill rects can be placed precisely.
  olCanvas = document.getElementById('lock-overlay');
  ghostCanvas = document.getElementById('ghost-overlay');
  if (olCanvas) {
    olCtx = olCanvas.getContext('2d');
    if (ghostCanvas) ghostCtx = ghostCanvas.getContext('2d');
    sizeOverlays();
    window.addEventListener('resize', () => { sizeOverlays(); renderLocks(); drawGhost(); });
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
        renderLocks();
      }
    } catch (e) {
      console.warn('initial state parse failed', e);
    }
  }

  canvas.addEventListener('click', onCanvasClick);

  // Live coordinate readout + ghost preview under the cursor.
  canvas.addEventListener('mousemove', onCanvasHover);
  canvas.addEventListener('mouseleave', () => { ghostCell = null; drawGhost(); updateCoords(-1, -1); });
  canvas.addEventListener('contextmenu', onCanvasRightClick);

  document.addEventListener('keydown', onKeyDown);
  buildPalette();
}

function cellFromEvent(evt) {
  const rect = canvas.getBoundingClientRect();
  return {
    x: Math.floor((evt.clientX - rect.left) / rect.width * cols),
    y: Math.floor((evt.clientY - rect.top) / rect.height * rows),
  };
}

function onCanvasHover(evt) {
  const { x, y } = cellFromEvent(evt);
  ghostCell = (x >= 0 && y >= 0 && x < cols && y < rows) ? { x, y } : null;
  drawGhost();
  updateCoords(x, y);
}

function updateCoords(x, y) {
  const el = document.getElementById('coords');
  if (!el) return;
  el.textContent = (x >= 0 && y >= 0 && x < cols && y < rows) ? `(${x}, ${y})` : '';
}

// Right-click = eyedropper: adopt a painted cell's color (0 = empty, 1..16).
function onCanvasRightClick(evt) {
  evt.preventDefault();
  const { x, y } = cellFromEvent(evt);
  if (x < 0 || y < 0 || x >= cols || y >= rows) return;
  const v = liveBmp[y * cols + x];
  if (v > 0) {
    selectColor(v - 1);
    toast(`picked ${PALETTE[v - 1]} from (${x}, ${y})`, 1600);
  } else {
    toast('empty pixel — nothing to pick', 1400);
  }
}

// 1-9,0,a-f pick palette entries; Esc cancels an armed claim.
function onKeyDown(evt) {
  if (evt.ctrlKey || evt.metaKey || evt.altKey) return;
  if (evt.key === 'Escape') {
    if (currentClaimId) resolveClaim('/cancel');
    return;
  }
  const k = evt.key.toLowerCase();
  if (k >= '1' && k <= '9') selectColor(parseInt(k, 10) - 1);
  else if (k === '0') selectColor(9);
  else if (k >= 'a' && k <= 'f') selectColor(10 + k.charCodeAt(0) - 97);
}

// ---- input: click to arm, click the same cell again to paint ----

function onCanvasClick(evt) {
  const { x, y } = cellFromEvent(evt);
  if (x < 0 || y < 0 || x >= cols || y >= rows) return;
  // Same cell as the armed claim → paint it. Everything at the pixel.
  if (pendingCell && pendingCell.x === x && pendingCell.y === y) {
    resolveClaim('/confirm');
    return;
  }
  claim(x, y);
}

async function claim(x, y, { quiet } = {}) {
  const color = selectedColor;
  const res = await fetch('/claim', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ x, y, color }),
  });
  if (!res.ok) {
    if (res.status === 409) {
      flashCell(x, y);
      toast('⛔ that pixel just got claimed by someone else', 2000);
    } else {
      toast('claim failed — try again', 2000);
    }
    return;
  }
  const { claimId } = await res.json();
  currentClaimId = claimId;
  pendingCell = { x, y };
  // Show our claim immediately; the SSE lockAdds flush confirms/replaces it.
  lockSet.add(`${x},${y}`);
  renderLocks();
  if (!quiet) toast('armed — click the same pixel again to paint · Esc to cancel', 3000);
  // Server-truth cleanup comes via lockRemoves (below); this timer is the
  // fallback if the SSE unlock is missed (drop, reconnect, backgrounded tab).
  // 3s claim window + small grace for the flush interval.
  setTimeout(() => {
    if (currentClaimId !== claimId) return; // superseded or resolved meanwhile
    toast('⏳ claim expired — click the pixel again to re-arm', 2400);
    clearHud(claimId);
  }, 3200);
}

async function resolveClaim(path) {
  if (!currentClaimId) return;
  const id = currentClaimId;
  currentClaimId = null;
  pendingCell = null;
  renderLocks();
  document.getElementById('hud').innerHTML = '';
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ claimId: id }),
  });
  if (!res.ok) {
    toast(res.status === 404 || res.status === 410
      ? '⏳ claim expired — click the pixel again'
      : 'something went wrong — try again', 2400);
  } else if (path.endsWith('confirm')) {
    toast('✓ painted', 1100);
  }
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

// ---- overlays (locks + hover ghost) ----
// Both overlay canvases are sized to #grid's ON-SCREEN box in device pixels
// (not the grid's cell count): sizing them cols*S × rows*S would be a ~1GB
// canvas for a 1000×1000 grid. Border width adapts to the displayed cell size.

function sizeOverlays() {
  const dpr = window.devicePixelRatio || 1;
  for (const oc of [olCanvas, ghostCanvas]) {
    if (!oc) continue;
    const r = oc.getBoundingClientRect();
    oc.width = Math.max(1, Math.round(r.width * dpr));
    oc.height = Math.max(1, Math.round(r.height * dpr));
  }
}

function cellW() {
  return olCanvas ? olCanvas.width / cols : 0;
}

function drawLock(x, y, mine) {
  if (!olCtx) return;
  const cw = cellW();
  const lw = Math.max(1, Math.min(cw * 0.3, cw / 2));
  olCtx.strokeStyle = mine ? PALETTE[selectedColor] : '#ffd600';
  olCtx.lineWidth = mine ? lw * 1.6 : lw;
  olCtx.strokeRect(x * cw + lw / 2, y * cw + lw / 2, cw - lw, cw - lw);
}

// No per-cell clear: cellW is fractional, so an anti-aliased stroke leaves an
// alpha fringe that spills past the cell rect and clearRect can't remove it
// (residue accumulates into a yellow silhouette). renderLocks() clears the
// whole overlay and re-strokes every lock — the only mutation path.
function renderLocks() {
  if (!olCtx) return;
  olCtx.clearRect(0, 0, olCanvas.width, olCanvas.height);
  for (const key of lockSet) {
    const [x, y] = key.split(',').map(Number);
    const mine = pendingCell && pendingCell.x === x && pendingCell.y === y;
    drawLock(x, y, mine);
  }
}

// Hover ghost: translucent fill of the selected color under the cursor, so
// the user sees exactly what a click will arm/paint. Hidden on cells we hold
// (the armed outline already shows) and on cells others hold.
function drawGhost() {
  if (!ghostCtx) return;
  ghostCtx.clearRect(0, 0, ghostCanvas.width, ghostCanvas.height);
  const cw = cellW();
  if (!cw) return;

  if (flash && Date.now() < flash.until) {
    const { x, y } = flash;
    ghostCtx.fillStyle = 'rgba(255,60,60,.4)';
    ghostCtx.fillRect(x * cw, y * cw, cw, cw);
    return;
  }
  if (!ghostCell) return;
  const { x, y } = ghostCell;
  if (pendingCell && pendingCell.x === x && pendingCell.y === y) return;
  if (lockSet.has(`${x},${y}`)) return; // held by someone else — no ghost

  const rgb = PALETTE_RGB[selectedColor];
  ghostCtx.fillStyle = `rgba(${rgb[0]},${rgb[1]},${rgb[2]},.5)`;
  ghostCtx.fillRect(x * cw, y * cw, cw, cw);
  ghostCtx.strokeStyle = PALETTE[selectedColor];
  ghostCtx.lineWidth = Math.max(0.5, cw * 0.1);
  ghostCtx.strokeRect(x * cw, y * cw, cw, cw);
}

// Brief red flash on a cell whose claim was rejected (locked by another).
function flashCell(x, y) {
  flash = { x, y, until: Date.now() + 450 };
  drawGhost();
  setTimeout(() => { if (flash && Date.now() >= flash.until) { flash = null; drawGhost(); } }, 500);
}

// ---- DataStar effect dispatcher (called when bmp/deltas/lock signals patch) ----

pb.render = function (bmp, deltas, lockAdds, lockRemoves, locks) {
  if (bmp && bmp !== lastBmp) {
    lastBmp = bmp;
    const bytes = atob(bmp);
    if (bytes.length === liveBmp.length) {
      for (let i = 0; i < bytes.length; i++) liveBmp[i] = bytes.charCodeAt(i);
    }
    renderFull();
  }

  if (deltas && deltas !== lastDeltas) {
    lastDeltas = deltas;
    for (const [x, y, v] of deltas) liveBmp[y * cols + x] = v;
    for (const [x, y] of deltas) fillCell(x, y);
  }

  if (locks !== lastLocks) {
    lastLocks = locks;
    lockSet = new Set((locks || []).map(([x, y]) => `${x},${y}`));
    renderLocks();
  }
  if (lockAdds && lockAdds !== lastLockAdds) {
    lastLockAdds = lockAdds;
    for (const [x, y] of (lockAdds || [])) lockSet.add(`${x},${y}`);
    renderLocks();
  }
  if (lockRemoves && lockRemoves !== lastLockRemoves) {
    lastLockRemoves = lockRemoves;
    for (const [x, y] of (lockRemoves || [])) lockSet.delete(`${x},${y}`);
    renderLocks();
    // If one of the unlocks is OUR pending cell, the claim expired or was
    // reaped — retire the armed state immediately.
    if (pendingCell && (lockRemoves || []).some(([x, y]) => x === pendingCell.x && y === pendingCell.y)) {
      clearHud(currentClaimId);
    }
  }
};

init();