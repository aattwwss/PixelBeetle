// Canvas Clash client glue.
//
// Input path (click → claim) uses one delegated listener + fetch because a
// 16k-cell grid can't carry per-cell handlers. Output path (server → client
// paint/lock/unlock) is pure DataStar SSE patches — see internal/hub.

let activeClaim = null;
let claimTimer = null;

async function claimClick(evt) {
  const el = evt.target.closest('.cell');
  if (!el || el.classList.contains('locked') || el.classList.contains('painted')) return;

  const x = Number(el.dataset.x), y = Number(el.dataset.y);
  const color = Math.floor(Math.random() * 16); // TODO: real palette picker

  const res = await fetch('/claim', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ x, y, color }),
  });
  if (!res.ok) {
    console.warn('claim rejected:', res.status, await res.text());
    return;
  }
  const { claimId } = await res.json();
  showHud(claimId, el);
}

function showHud(claimId, cellEl) {
  cancelClaim(); // one pending claim at a time (per player)
  activeClaim = { id: claimId, cellEl };

  // The lock window matches the transfer's `timeout` (3s). When it lapses,
  // TigerBeetle auto-expires the pending transfer and CDC emits
  // two_phase_expired — the server-side reaper unlocks the UI.
  claimTimer = setTimeout(() => cancelClaim(), 2800);

  const hud = document.getElementById('hud');
  hud.innerHTML = `
    <span class="countdown">confirm within 3s…</span>
    <button onclick="resolve(true)">paint it</button>
    <button onclick="resolve(false)">cancel</button>`;
}

async function resolve(confirm) {
  if (!activeClaim) return;
  const { id } = activeClaim;
  clearTimeout(claimTimer);
  activeClaim = null;

  await fetch(confirm ? '/confirm' : '/cancel', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ claimId: id }),
  });
  document.getElementById('hud').innerHTML = '';
}

function cancelClaim() {
  if (claimTimer) { clearTimeout(claimTimer); claimTimer = null; }
}
