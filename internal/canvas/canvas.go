// Package canvas holds rendering-neutral canvas facts shared by the SSR
// layer and the SSE hub, so server-rendered cells and streamed patches are
// byte-identical.
package canvas

import (
	"fmt"
	"strings"
)

// Palette has 16 entries; the color byte maps onto it modulo length.
// The raw byte is what gets persisted in the transfer's `code` field.
var Palette = [16]string{
	"#ffffff", "#e4e4e4", "#888888", "#222222",
	"#ffb470", "#9a6324", "#800000", "#ba2d2d",
	"#ffd600", "#808000", "#469990", "#42d4f4",
	"#4363d8", "#000075", "#f032e6", "#fabed4",
}

func CellID(x, y uint32) string { return fmt.Sprintf("c-%d-%d", x, y) }

func ColorCSS(color uint8) string { return Palette[int(color)%len(Palette)] }

// CellHTML renders one grid cell. Used both by SSR templates and by SSE
// patches so DataStar's outer merge produces identical markup.
//
// state: "" empty | "painted" | "locked"
func CellHTML(x, y uint32, state string, color uint8) string {
	var sb strings.Builder
	sb.WriteString(`<div id="`)
	sb.WriteString(CellID(x, y))
	sb.WriteString(`" class="cell`)
	if state != "" {
		sb.WriteByte(' ')
		sb.WriteString(state)
	}
	sb.WriteString(`" data-x="`)
	fmt.Fprintf(&sb, "%d", x)
	sb.WriteString(`" data-y="`)
	fmt.Fprintf(&sb, "%d", y)
	sb.WriteString(`"`)
	if state == "painted" {
		sb.WriteString(` style="background:`)
		sb.WriteString(ColorCSS(color))
		sb.WriteString(`"`)
	}
	sb.WriteString(`></div>`)
	return sb.String()
}
