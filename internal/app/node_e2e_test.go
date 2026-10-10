package app_test

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/app"
)

func TestNodeInventoryEndToEnd(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		scenario string
		evidence string
	}{
		{"observation-scenario.yaml", "Observed        $[\"stock\"] = -1"},
		{"history-scenario.yaml", "Observed        2 successful attempts"},
	} {
		t.Run(test.scenario, func(t *testing.T) {
			t.Parallel()
			target := startNodeInventory(t)
			scenarioPath := scenarioForTarget(t, test.scenario, target)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var stdout, stderr bytes.Buffer
			code := app.Run(ctx, []string{"run", scenarioPath}, &stdout, &stderr)
			if code != 1 || stderr.Len() != 0 {
				t.Fatalf("Run() exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
			}
			assertOutputContains(t, stdout.String(),
				"Target · "+target,
				"10/10 trials demonstrated the violation",
				"Status          REDUCED",
				"Attempts        2",
				"Concurrency     2",
				"Violations      10/10 trials",
				test.evidence,
				"concurtest run --attempts 2 --concurrency 2 --no-reduce "+scenarioPath,
			)
		})
	}
}

func startNodeInventory(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is not installed; skipping the Node inventory integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	// Port zero isolates parallel tests. Closing stdin shuts down the child;
	// the context deadline also bounds startup and cleanup if it stops responding.
	const script = `
const { createServer } = require('./server');
const server = createServer();
server.listen(0, '127.0.0.1', () => console.log('http://127.0.0.1:' + server.address().port));
process.stdin.resume();
process.stdin.on('end', () => {
  server.close();
  server.closeAllConnections();
});
`
	command := exec.CommandContext(ctx, node, "-e", script)
	command.Dir = filepath.Join("..", "..", "examples", "vulnerable-inventory", "node")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("open Node stdout: %v", err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("open Node stdin: %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start Node inventory: %v", err)
	}
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			t.Errorf("close Node stdin: %v", err)
		}
		if err := command.Wait(); err != nil {
			t.Errorf("Node inventory stopped: %v\nstderr:\n%s", err, &stderr)
		}
	})
	address, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read Node inventory address: %v", err)
	}
	return strings.TrimSpace(address)
}
