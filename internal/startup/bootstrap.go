package startup

import (
	"fmt"
	"io"
)

func printBootstrapInstructions(w io.Writer, code string) {
	if code == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "\nOswald first-administrator bootstrap\n\nBootstrap code: %s\n\nRun /bootstrap %s from an authenticated Discord, iMessage, or Home Assistant account. The code is valid once for this process. Restart Oswald to replace a lost code while no administrator exists.\n\n", code, code)
}
