package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
)

// The tests need a few tiny programs to run as real child processes: one that
// writes to both streams, one that prints a line, one that echoes its
// arguments. `sh -c` would be the obvious way, but Windows has no POSIX shell,
// so the test binary re-runs itself instead. TestMain acts as the helper
// program when helperEnv names one, and helper builds the call.
const helperEnv = "ANYSHIP_TEST_HELPER"

func TestMain(m *testing.M) {
	if program := os.Getenv(helperEnv); program != "" {
		os.Exit(helperMain(program, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func helperMain(program string, args []string) int {
	switch program {
	case "streams":
		fmt.Println("from-stdout")
		fmt.Fprintln(os.Stderr, "from-stderr")
		// Reading stdin proves the child gets the caller's stdin, not the
		// terminal's. Callers pass an empty reader, so this returns at once.
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "print":
		fmt.Println(strings.Join(args, " "))
	case "echo-args":
		// One argument per line, so the test sees exactly what arrived.
		for _, a := range args {
			fmt.Println(a)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown test helper %q\n", program)
		return 2
	}
	return 0
}

// helper returns the exec options, command name and arguments that run the
// named helper program.
func helper(program string, args ...string) (adapter.ExecOptions, string, []string) {
	return adapter.ExecOptions{Env: []string{helperEnv + "=" + program}}, os.Args[0], args
}
