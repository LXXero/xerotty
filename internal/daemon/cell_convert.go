package daemon

import (
	"github.com/LXXero/xerotty/internal/cellconv"
	"github.com/LXXero/xerotty/internal/protocol"
	uv "github.com/charmbracelet/ultraviolet"
)

// cellFromUV / cellToUV delegate to internal/cellconv — the single
// home for both conversion directions (the decode side used to live
// only in daemonsource; the hot-upgrade resume path needed it here
// and copying it would invite attr-bit drift).
func cellFromUV(c *uv.Cell) protocol.Cell { return cellconv.FromUV(c) }
func cellToUV(p protocol.Cell) uv.Cell    { return cellconv.ToUV(p) }
