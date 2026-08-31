// PixelBeetle timelapse view.
//
// There is NO client-side history buffer. Every slider stop fires one live
// server request; the server folds its nearest one-minute anchor bitmap with
// the TigerBeetle transfers in (anchor, requested-ts] and returns the frame.
// A frame is a base64 packed bitmap (~64KB at 256×256, ~1.3MB at 1M²). The
// latest request wins: slower responses to earlier stops are discarded.

const hCanvas = document.getElementById('hgrid');
const hCtx = hCanvas.getContext('2d');
const slider = document.getElementById('tslider');
const tlabel = document.getElementById('tlabel');
const hstatus = document.getElementById('hstatus');
const hCols = hCanvas.width, hRows = hCanvas.height;

let seekSeq = 0; // monotonically increasing; only the newest seek may draw

function drawFrame(b64) {
  const bytes = atob(b64);
  if (bytes.length !== hCols * hRows) {
    hstatus.textContent = 'frame size mismatch';
    return;
  }
  const img = hCtx.createImageData(hCols, hRows);
  const d = img.data;
  for (let i = 0; i < bytes.length; i++) {
    const v = bytes.charCodeAt(i);
    const rgb = v ? PALETTE_RGB[(v - 1) % 16] : EMPTY_RGB;
    const o = i * 4;
    d[o] = rgb[0]; d[o + 1] = rgb[1]; d[o + 2] = rgb[2]; d[o + 3] = 255;
  }
  hCtx.putImageData(img, 0, 0);
}

async function seek(tsMs) {
  const seq = ++seekSeq;
  hstatus.textContent = 'querying…';
  try {
    const r = await fetch(`/api/history/frame?ts_ms=${tsMs}`);
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const f = await r.json();
    if (seq !== seekSeq) return; // a newer slider stop superseded this one
    drawFrame(f.bmp);
    tlabel.textContent = new Date(f.tsMs).toLocaleString();
    hstatus.textContent = '';
  } catch (e) {
    if (seq === seekSeq) hstatus.textContent = `query failed: ${e.message}`;
  }
}

// Trailing-edge throttle: while dragging, at most one live query per 120ms,
// always for the newest position (intermediate stops are simply skipped).
function throttle(fn, ms) {
  let last = 0, timer = null, pending = null;
  return (...args) => {
    pending = args;
    const now = Date.now();
    if (now - last >= ms) {
      last = now;
      fn(...pending);
    } else if (!timer) {
      timer = setTimeout(() => {
        timer = null;
        last = Date.now();
        fn(...pending);
      }, ms - (now - last));
    }
  };
}
const throttledSeek = throttle(seek, 120);

fetch('/api/history/meta')
  .then(r => r.json())
  .then(m => {
    if (!m.maxMs) {
      tlabel.textContent = '';
      hstatus.textContent = 'nothing painted yet — go paint something first';
      return;
    }
    slider.min = m.minMs;
    slider.max = m.maxMs;
    slider.value = m.maxMs;
    slider.disabled = false;
    throttledSeek(Number(slider.value));
  })
  .catch(e => { hstatus.textContent = `meta load failed: ${e.message}`; });

slider.addEventListener('input', () => throttledSeek(Number(slider.value)));
