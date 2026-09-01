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

const pb = {};
window.pb = pb;

let canvas, ctx, cols, rows;
let olCanvas, olCtx; // lock overlay: yellow border boxes drawn above the bitmap
let liveBmp = null;      // Uint8Array W*H (bitmap values 0..16)
let lockSet = new Set(); // "x,y" strings currently locked
let currentClaimId = null;
let pendingCell = null; // {x, y} of the cell we hold a claim on, if any

// clearHud drops the confirm/cancel panel — but only if `claimId` is still the
// claim that owns the HUD (a stale expiry timer or unlock for a superseded
// claim must not clobber a newer one).
function clearHud(claimId) {
  if (currentClaimId !== claimId) return;
  currentClaimId = null;
  pendingCell = null;
  document.getElementById('hud').innerHTML = '';
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
};

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
        renderLocks();
      }
    } catch (e) {
      console.warn('initial state parse failed', e);
    }
  }

  canvas.addEventListener('click', onCanvasClick);

  // Live coordinate readout: hover shows the pixel under the cursor.
  canvas.addEventListener('mousemove', onCanvasHover);
  canvas.addEventListener('mouseleave', () => updateCoords(-1, -1));
  buildPalette();
}

function onCanvasHover(evt) {
  const rect = canvas.getBoundingClientRect();
  const x = Math.floor((evt.clientX - rect.left) / rect.width * cols);
  const y = Math.floor((evt.clientY - rect.top) / rect.height * rows);
  updateCoords(x, y);
}

function updateCoords(x, y) {
  const el = document.getElementById('coords');
  if (!el) return;
  el.textContent = (x >= 0 && y >= 0 && x < cols && y < rows) ? `(${x}, ${y})` : '';
}

// ---- input: click -> claim (two-phase: claim, then confirm/cancel in HUD) ----

function onCanvasClick(evt) {
  const rect = canvas.getBoundingClientRect();
  const x = Math.floor((evt.clientX - rect.left) / rect.width * cols);
  const y = Math.floor((evt.clientY - rect.top) / rect.height * rows);
  if (x < 0 || y < 0 || x >= cols || y >= rows) return;
  claim(x, y);
}

async function claim(x, y) {
  const color = selectedColor;
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
  pendingCell = { x, y };
  const hud = document.getElementById('hud');
  hud.innerHTML = `
    <span class="countdown">confirm within 3s…</span>
    <button onclick="pb.confirm()">paint it</button>
    <button onclick="pb.cancel()">cancel</button>`;
  // Server-truth cleanup comes via lockRemoves (below); this timer is the
  // fallback if the SSE unlock is missed (drop, reconnect, backgrounded tab).
  // 3s claim window + small grace for the flush interval.
  setTimeout(() => clearHud(claimId), 3200);
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

// No per-cell clear: cellW is fractional, so an anti-aliased stroke leaves an
// alpha fringe that spills past the cell rect and clearRect can't remove it
// (residue accumulates into a yellow silhouette). renderLocks() clears the
// whole overlay and re-strokes every lock — the only mutation path.
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
    // reaped — retire the confirm/cancel HUD immediately.
    if (pendingCell && (lockRemoves || []).some(([x, y]) => x === pendingCell.x && y === pendingCell.y)) {
      clearHud(currentClaimId);
    }
  }
};

init();
