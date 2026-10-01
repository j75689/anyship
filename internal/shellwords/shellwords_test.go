package shellwords

import (
	"slices"
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	for command, want := range map[string][]string{
		"reth node --chain mainnet":                   {"reth", "node", "--chain", "mainnet"},
		"  node   server.js  ":                        {"node", "server.js"},
		`echo 'hello world' "a b" c\ d`:               {"echo", "hello world", "a b", "c d"},
		`printf "say \"hi\" \n"`:                      {"printf", `say "hi" \n`},
		`sh -c 'echo $HOME | tr a-z A-Z'`:             {"sh", "-c", "echo $HOME | tr a-z A-Z"},
		`--flag=''`:                                   {"--flag="},
		`--http.api eth,net,web3 --http.addr 0.0.0.0`: {"--http.api", "eth,net,web3", "--http.addr", "0.0.0.0"},
	} {
		got, err := Split(command)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Split(%q) = %q, %v; want %q", command, got, err, want)
		}
	}
}

func TestSplitRejectsShellSyntax(t *testing.T) {
	for command, wantErr := range map[string]string{
		"node a.js | tee log":  "shell syntax '|'",
		"node a.js > out":      "shell syntax '>'",
		"echo $HOME":           "shell syntax '$'",
		`echo "$HOME"`:         "shell syntax '$'",
		"node a.js && echo ok": "shell syntax '&'",
		"rm *.log":             "shell syntax '*'",
		"echo 'unterminated":   "unterminated ' quote",
		"echo trailing\\":      "trailing backslash",
		"   ":                  "command is empty",
	} {
		_, err := Split(command)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Split(%q) error = %v, want it to mention %q", command, err, wantErr)
		}
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"anyship/eth-mainnet": "anyship/eth-mainnet",
		"my dir":              "'my dir'",
		"it's":                `'it'\''s'`,
		"":                    "''",
		"$(rm -rf /)":         "'$(rm -rf /)'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}
