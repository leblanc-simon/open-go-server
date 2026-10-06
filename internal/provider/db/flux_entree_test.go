package db

import (
	"bytes"
	"io"

	"leblanc.io/open-go-server/internal/exec"
)

// entréeDe rend ce qu'une commande reçoit sur son entrée standard.
func entréeDe(cmd exec.Command) io.Reader {
	if cmd.StdinStream != nil {
		return cmd.StdinStream
	}
	return bytes.NewReader(cmd.Stdin)
}
