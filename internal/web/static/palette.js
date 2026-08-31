// Shared 16-color palette — the single source of truth for the live canvas
// (app.js) and the timelapse view (history.js).
//
// Server-side encoding contract: a bitmap byte value v means palette index
// (v-1); 0 = empty canvas background. Keep in sync with the palette array the
// swatch picker builds from.
const PALETTE = ["#ffffff", "#e4e4e4", "#888888", "#222222", "#ffb470", "#9a6324", "#800000", "#ba2d2d",
                 "#ffd600", "#808000", "#469990", "#42d4f4", "#4363d8", "#000075", "#f032e6", "#fabed4"];
const PALETTE_RGB = PALETTE.map(h => [parseInt(h.slice(1, 3), 16), parseInt(h.slice(3, 5), 16), parseInt(h.slice(5, 7), 16)]);
const EMPTY_RGB = [0x1c, 0x1c, 0x1c];
