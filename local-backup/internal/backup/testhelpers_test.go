package backup

import (
	"os"
	"os/exec"
	"testing"
)

func newSelfCmd(t *testing.T, testName string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^"+testName+"$")
	cmd.Env = append(os.Environ(), "CRASH_TEST="+testName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}
