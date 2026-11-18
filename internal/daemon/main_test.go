package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/worksonmyai/kontora/internal/tmux"
)

func TestMain(m *testing.M) {
	if (len(os.Args) == 3 || len(os.Args) == 4) && os.Args[1] == "pty-log-sink" {
		if err := tmux.DrainPaneLog(os.Stdin, os.Args[2], os.Args[3:]...); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
