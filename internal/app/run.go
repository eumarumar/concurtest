// Package app connects the ConcurTest command-line interface to scenario
// loading, engine execution, and reporting.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/report"
	"github.com/eumarumar/concurtest/internal/scenario"
)

const (
	exitSuccess   = 0
	exitViolation = 1
	exitError     = 2
)

// Run executes the command described by args and returns its process exit code.
// It does not call os.Exit, allowing callers to release process resources and
// tests to inspect all output.
func Run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if stdout == nil {
		writeDiagnostic(stderr, "ConcurTest could not start: no standard output writer was provided.")
		return exitError
	}
	if stderr == nil {
		return exitError
	}
	if ctx == nil {
		writeDiagnostic(stderr, "ConcurTest could not start: no execution context was provided.")
		return exitError
	}

	if helpRequested(args) {
		if err := writeUsage(stdout); err != nil {
			writeDiagnostic(stderr, "Could not write help: %v", err)
			return exitError
		}
		return exitSuccess
	}

	scenarioPath, err := scenarioPathFromArgs(args)
	if err != nil {
		writeDiagnostic(stderr, "Command error: %v", err)
		if usageErr := writeUsage(stderr); usageErr != nil {
			return exitError
		}
		return exitError
	}

	definition, err := loadScenario(scenarioPath)
	if err != nil {
		writeDiagnostic(stderr, "Could not load scenario %q: %v", scenarioPath, err)
		return exitError
	}

	if err := writeStart(stdout, definition); err != nil {
		writeDiagnostic(stderr, "Could not write the run details: %v", err)
		return exitError
	}

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		writeDiagnostic(stderr, "ConcurTest could not create an HTTP client.")
		return exitError
	}
	transport := defaultTransport.Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   definition.RequestTimeout,
	}
	result, runErr := engine.RunTrials(ctx, client, definition.Scenario, definition.Trials)
	reportErr := report.WriteText(stdout, report.TextInput{
		ScenarioPath: scenarioPath,
		Scenario:     definition.Scenario,
		Result:       result,
		RunError:     runErr,
	})
	if reportErr != nil {
		writeDiagnostic(stderr, "Could not write the run report: %v", reportErr)
		return exitError
	}

	if runErr != nil {
		return exitError
	}
	switch result.Status {
	case engine.TrialStatusPassed:
		return exitSuccess
	case engine.TrialStatusViolated:
		return exitViolation
	case engine.TrialStatusInconclusive, engine.TrialStatusErrored:
		return exitError
	default:
		writeDiagnostic(stderr, "ConcurTest returned an unknown trials result %q.", result.Status)
		return exitError
	}
}

func helpRequested(args []string) bool {
	if len(args) == 1 {
		return args[0] == "help" || args[0] == "-h" || args[0] == "--help"
	}
	return len(args) == 2 && args[0] == "run" && (args[1] == "-h" || args[1] == "--help")
}

func scenarioPathFromArgs(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("expected the run command and a scenario file")
	}
	if args[0] != "run" {
		return "", fmt.Errorf("unknown command %q", args[0])
	}
	if len(args) < 2 {
		return "", errors.New("run needs a scenario file")
	}
	if len(args) > 2 {
		return "", errors.New("run accepts exactly one scenario file")
	}
	return args[1], nil
}

func loadScenario(path string) (scenario.Definition, error) {
	file, err := os.Open(path)
	if err != nil {
		return scenario.Definition{}, err
	}

	definition, decodeErr := scenario.Decode(file)
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil {
		return scenario.Definition{}, errors.Join(
			decodeErr,
			wrapError("close scenario file", closeErr),
		)
	}
	return definition, nil
}

func writeStart(writer io.Writer, definition scenario.Definition) error {
	_, err := fmt.Fprintf(
		writer,
		"Scenario: %q\nTarget: %s\nWarning: this command sends concurrent requests and may modify target state.\n\n",
		definition.Name,
		definition.Target,
	)
	return err
}

func writeUsage(writer io.Writer) error {
	_, err := io.WriteString(writer, "Usage:\n  concurtest run <scenario.yaml>\n\nRuns one adversarial scenario against its configured target.\n")
	return err
}

func writeDiagnostic(writer io.Writer, format string, arguments ...any) {
	if writer == nil {
		return
	}
	_, _ = fmt.Fprintf(writer, format+"\n", arguments...)
}

func wrapError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}
