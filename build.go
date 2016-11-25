package buildworker

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const ldFlagVarPkg = "github.com/mholt/caddy/caddy/caddymain"

func makeLdFlags(repoPath string) (string, error) {
	var ldflags []string

	for _, ldvar := range []struct {
		name  string
		value func() (string, error)
	}{
		// Timestamp of build
		{
			name: "buildDate",
			value: func() (string, error) {
				return time.Now().UTC().Format("Mon Jan 02 15:04:05 MST 2006"), nil
			},
		},

		// Current tag, if HEAD is on a tag
		{
			name: "gitTag",
			value: func() (string, error) {
				// OK to ignore error since HEAD may not be at a tag
				return run(exec.Command("git", "-C", repoPath, "describe", "--exact-match", "HEAD"), true)
			},
		},

		// Nearest tag on branch
		{
			name: "gitNearestTag",
			value: func() (string, error) {
				return run(exec.Command("git", "-C", repoPath, "describe", "--abbrev=0", "--tags", "HEAD"), false)
			},
		},

		// Commit SHA
		{
			name: "gitCommit",
			value: func() (string, error) {
				return run(exec.Command("git", "-C", repoPath, "rev-parse", "--short", "HEAD"), false)
			},
		},

		// Summary of uncommitted changes
		{
			name: "gitShortStat",
			value: func() (string, error) {
				return run(exec.Command("git", "-C", repoPath, "diff-index", "--shortstat", "HEAD"), false)
			},
		},

		// List of modified files
		{
			name: "gitFilesModified",
			value: func() (string, error) {
				return run(exec.Command("git", "-C", repoPath, "diff-index", "--name-only", "HEAD"), false)
			},
		},
	} {
		value, err := ldvar.value()
		if err != nil {
			return "", err
		}
		ldflags = append(ldflags, fmt.Sprintf(`-X "%s.%s=%s"`, ldFlagVarPkg, ldvar.name, value))
	}

	return strings.Join(ldflags, " "), nil
}

func run(cmd *exec.Cmd, ignoreError bool) (string, error) {
	out, err := cmd.Output()
	if err != nil && !ignoreError {
		return string(out), err
	}
	return strings.TrimSpace(string(out)), nil
}
