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
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
	"github.com/eumarumar/concurtest/internal/reduction"
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
	wantsJSON := jsonFormatRequested(args)
	if stdout == nil {
		writeDiagnostic(stderr, "ConcurTest could not start: no standard output writer was provided.")
		return exitError
	}
	if stderr == nil {
		return exitError
	}
	if ctx == nil {
		err := failure.New(failure.CodeInvalidExecution, "ConcurTest could not start: no execution context was provided.")
		if wantsJSON {
			writeEarlyJSONError(stdout, stderr, report.ErrorInput{Err: err})
		} else {
			writeDiagnostic(stderr, "%v", err)
		}
		return exitError
	}

	if helpRequested(args) {
		if err := writeUsage(stdout); err != nil {
			writeDiagnostic(stderr, "Could not write help: %v", err)
			return exitError
		}
		return exitSuccess
	}

	options, err := runOptionsFromArgs(args)
	if err != nil {
		if wantsJSON {
			err = failure.Wrap(failure.CodeInvalidCommand, "command is invalid", err)
			writeEarlyJSONError(stdout, stderr, report.ErrorInput{Err: err})
			return exitError
		}
		writeDiagnostic(stderr, "Command error: %v", err)
		if usageErr := writeUsage(stderr); usageErr != nil {
			return exitError
		}
		return exitError
	}

	definition, err := loadScenario(options.scenarioPath)
	if err != nil {
		if options.format == outputJSON {
			writeEarlyJSONError(stdout, stderr, report.ErrorInput{ScenarioPath: options.scenarioPath, Err: err})
			return exitError
		}
		writeDiagnostic(stderr, "Could not load scenario %q: %v", options.scenarioPath, err)
		return exitError
	}
	if err := applyRunOptions(&definition, options); err != nil {
		if options.format == outputJSON {
			err = failure.Wrap(failure.CodeInvalidCommand, "command is invalid", err)
			writeEarlyJSONError(stdout, stderr, report.ErrorInput{
				ScenarioPath: options.scenarioPath, ScenarioName: definition.Name,
				Target: definition.Target, Err: err,
			})
			return exitError
		}
		writeDiagnostic(stderr, "Command error: %v", err)
		return exitError
	}

	if options.format == outputText {
		textOptions := report.TextOptions{
			Verbose: options.verbose,
			Color:   shouldUseColor(options.color, stdout),
		}
		if err := report.WriteTextStart(stdout, report.TextStartInput{
			ScenarioName:     definition.Name,
			Target:           definition.Target,
			Concurrency:      definition.Scenario.Concurrency,
			ReductionEnabled: definition.Reduce,
		}, textOptions); err != nil {
			writeDiagnostic(stderr, "Could not write the run details: %v", err)
			return exitError
		}
	}

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		err := failure.New(failure.CodeInternal, "ConcurTest could not create an HTTP client.")
		if options.format == outputJSON {
			writeEarlyJSONError(stdout, stderr, report.ErrorInput{
				ScenarioPath: options.scenarioPath, ScenarioName: definition.Name,
				Target: definition.Target, Err: err,
			})
			return exitError
		}
		writeDiagnostic(stderr, "ConcurTest could not create an HTTP client.")
		return exitError
	}
	transport := defaultTransport.Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   definition.RequestTimeout,
	}
	var result engine.TrialsResult
	var reductionResult *reduction.Result
	var runErr error
	if definition.Reduce {
		reduced, err := reduction.Reduce(ctx, client, definition.Scenario, definition.Trials)
		result = reduced.Baseline
		reductionResult = &reduced
		runErr = err
	} else {
		result, runErr = engine.RunTrials(ctx, client, definition.Scenario, definition.Trials)
	}
	reportInput := report.Input{
		ScenarioPath:     options.scenarioPath,
		ScenarioName:     definition.Name,
		Target:           definition.Target,
		RequestTimeout:   definition.RequestTimeout,
		ConfiguredTrials: definition.Trials,
		ReductionEnabled: definition.Reduce,
		Scenario:         definition.Scenario,
		Result:           result,
		RunError:         runErr,
		Reduction:        reductionResult,
	}
	var reportErr error
	if options.format == outputJSON {
		reportErr = report.WriteJSON(stdout, reportInput)
	} else {
		reportErr = report.WriteTextWithOptions(stdout, reportInput, report.TextOptions{
			Verbose: options.verbose,
			Color:   shouldUseColor(options.color, stdout),
		})
	}
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

type runOptions struct {
	scenarioPath   string
	attempts       int
	attemptsSet    bool
	concurrency    int
	concurrencySet bool
	noReduce       bool
	format         outputFormat
	formatSet      bool
	verbose        bool
	color          colorMode
	colorSet       bool
}

type outputFormat string

const (
	outputText outputFormat = "text"
	outputJSON outputFormat = "json"
)

type colorMode string

const (
	colorAuto   colorMode = "auto"
	colorAlways colorMode = "always"
	colorNever  colorMode = "never"
)

func runOptionsFromArgs(args []string) (runOptions, error) {
	if len(args) == 0 {
		return runOptions{}, errors.New("expected the run command and a scenario file")
	}
	if args[0] != "run" {
		return runOptions{}, fmt.Errorf("unknown command %q", args[0])
	}

	options := runOptions{format: outputText, color: colorAuto}
	for index := 1; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--no-reduce":
			if options.noReduce {
				return runOptions{}, errors.New("run accepts --no-reduce only once")
			}
			options.noReduce = true
		case argument == "--verbose":
			if options.verbose {
				return runOptions{}, errors.New("run accepts --verbose only once")
			}
			options.verbose = true
		case argument == "--color" || strings.HasPrefix(argument, "--color="):
			value, next, err := namedOptionValue(args, index, "--color", "auto, always, or never")
			if err != nil {
				return runOptions{}, err
			}
			if options.colorSet {
				return runOptions{}, errors.New("run accepts --color only once")
			}
			switch colorMode(value) {
			case colorAuto, colorAlways, colorNever:
				options.color = colorMode(value)
			default:
				return runOptions{}, fmt.Errorf("--color must be auto, always, or never, got %q", value)
			}
			options.colorSet = true
			index = next
		case argument == "--format" || strings.HasPrefix(argument, "--format="):
			value, next, err := formatOptionValue(args, index)
			if err != nil {
				return runOptions{}, err
			}
			if options.formatSet {
				return runOptions{}, errors.New("run accepts --format only once")
			}
			switch outputFormat(value) {
			case outputText, outputJSON:
				options.format = outputFormat(value)
			default:
				return runOptions{}, fmt.Errorf("--format must be text or json, got %q", value)
			}
			options.formatSet = true
			index = next
		case argument == "--attempts" || strings.HasPrefix(argument, "--attempts="):
			value, next, err := optionValue(args, index, "--attempts")
			if err != nil {
				return runOptions{}, err
			}
			if options.attemptsSet {
				return runOptions{}, errors.New("run accepts --attempts only once")
			}
			options.attempts, err = positiveOption("--attempts", value)
			if err != nil {
				return runOptions{}, err
			}
			options.attemptsSet = true
			index = next
		case argument == "--concurrency" || strings.HasPrefix(argument, "--concurrency="):
			value, next, err := optionValue(args, index, "--concurrency")
			if err != nil {
				return runOptions{}, err
			}
			if options.concurrencySet {
				return runOptions{}, errors.New("run accepts --concurrency only once")
			}
			options.concurrency, err = positiveOption("--concurrency", value)
			if err != nil {
				return runOptions{}, err
			}
			options.concurrencySet = true
			index = next
		case strings.HasPrefix(argument, "-"):
			return runOptions{}, fmt.Errorf("unknown run option %q", argument)
		case options.scenarioPath == "":
			options.scenarioPath = argument
		default:
			return runOptions{}, errors.New("run accepts exactly one scenario file")
		}
	}
	if options.scenarioPath == "" {
		return runOptions{}, errors.New("run needs a scenario file")
	}
	if options.format == outputJSON {
		switch {
		case options.verbose && options.colorSet:
			return runOptions{}, errors.New("--verbose and --color only apply to text reports")
		case options.verbose:
			return runOptions{}, errors.New("--verbose only applies to text reports")
		case options.colorSet:
			return runOptions{}, errors.New("--color only applies to text reports")
		}
	}
	return options, nil
}

func namedOptionValue(args []string, index int, name, choices string) (string, int, error) {
	argument := args[index]
	if argument == name {
		if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
			return "", index, fmt.Errorf("%s needs %s", name, choices)
		}
		return args[index+1], index + 1, nil
	}
	value := strings.TrimPrefix(argument, name+"=")
	if value == "" {
		return "", index, fmt.Errorf("%s needs %s", name, choices)
	}
	return value, index, nil
}

func formatOptionValue(args []string, index int) (string, int, error) {
	argument := args[index]
	if argument == "--format" {
		if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
			return "", index, errors.New("--format needs text or json")
		}
		return args[index+1], index + 1, nil
	}
	value := strings.TrimPrefix(argument, "--format=")
	if value == "" {
		return "", index, errors.New("--format needs text or json")
	}
	return value, index, nil
}

func jsonFormatRequested(args []string) bool {
	for index, argument := range args {
		if argument == "--format=json" {
			return true
		}
		if argument == "--format" && index+1 < len(args) && args[index+1] == "json" {
			return true
		}
	}
	return false
}

func optionValue(args []string, index int, name string) (string, int, error) {
	argument := args[index]
	if argument == name {
		if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
			return "", index, fmt.Errorf("%s needs a positive integer", name)
		}
		return args[index+1], index + 1, nil
	}
	value := strings.TrimPrefix(argument, name+"=")
	if value == "" {
		return "", index, fmt.Errorf("%s needs a positive integer", name)
	}
	return value, index, nil
}

func positiveOption(name, value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func applyRunOptions(definition *scenario.Definition, options runOptions) error {
	if options.attemptsSet {
		definition.Scenario.Attempts = options.attempts
	}
	if options.concurrencySet {
		definition.Scenario.Concurrency = options.concurrency
	}
	if options.noReduce {
		definition.Reduce = false
	}
	if definition.Scenario.Concurrency > definition.Scenario.Attempts {
		return fmt.Errorf(
			"--concurrency result %d must not exceed attempts %d",
			definition.Scenario.Concurrency,
			definition.Scenario.Attempts,
		)
	}
	if definition.Reduce && definition.Scenario.Attempts < 2 {
		return errors.New("reduction needs at least 2 attempts; use --no-reduce to run this override")
	}
	if definition.Reduce && definition.Scenario.Concurrency < 2 {
		return errors.New("reduction needs concurrency of at least 2; use --no-reduce to run this override")
	}
	return nil
}

func loadScenario(path string) (scenario.Definition, error) {
	file, err := os.Open(path)
	if err != nil {
		return scenario.Definition{}, failure.Wrap(failure.CodeScenarioFileFailed, "open scenario file", err)
	}

	definition, decodeErr := scenario.Decode(file)
	closeErr := file.Close()
	if decodeErr != nil {
		return scenario.Definition{}, failure.Join(
			failure.CodeScenarioInvalid,
			"load scenario",
			failure.Wrap(failure.CodeScenarioInvalid, "decode scenario", decodeErr),
			failure.Wrap(failure.CodeScenarioFileFailed, "close scenario file", closeErr),
		)
	}
	if closeErr != nil {
		return scenario.Definition{}, failure.Wrap(failure.CodeScenarioFileFailed, "close scenario file", closeErr)
	}
	return definition, nil
}

func writeUsage(writer io.Writer) error {
	_, err := io.WriteString(
		writer,
		"Usage:\n  concurtest run [--attempts N] [--concurrency N] [--no-reduce] [--format text|json] [--verbose] [--color auto|always|never] <scenario.yaml>\n\nRuns one adversarial scenario against its configured target. Text output is the default. Run with --verbose to display all retained trial evidence, or --format json for structured automation output.\n",
	)
	return err
}

func shouldUseColor(mode colorMode, writer io.Writer) bool {
	switch mode {
	case colorAlways:
		return true
	case colorNever:
		return false
	}
	if value, exists := os.LookupEnv("NO_COLOR"); exists && value != "" {
		return false
	}
	file, ok := writer.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

func writeDiagnostic(writer io.Writer, format string, arguments ...any) {
	if writer == nil {
		return
	}
	_, _ = fmt.Fprintf(writer, format+"\n", arguments...)
}

func writeEarlyJSONError(writer io.Writer, diagnostics io.Writer, input report.ErrorInput) {
	if err := report.WriteJSONError(writer, input); err != nil {
		writeDiagnostic(diagnostics, "Could not write the JSON error report: %v", err)
	}
}
