package report_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
	"github.com/eumarumar/concurtest/internal/reduction"
	"github.com/eumarumar/concurtest/internal/report"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const reportSchemaID = "https://concurtest.dev/schemas/report-v1.schema.json"

func TestWriteJSONProducesSchemaValidCompleteSafeEvidence(t *testing.T) {
	t.Parallel()

	input := completedTextInput("passed", 0)
	input.ScenarioName = "inventory check"
	input.Target = "http://example.test"
	input.RequestTimeout = 2_000_000_000
	input.ConfiguredTrials = 1
	input.Scenario.Operation.Request.Body = []byte("request-body-secret")
	input.Result.Trials[0].Run.History.Attempts[0].Execution.Request.Body = []byte("request-body-secret")
	path := []string{"data", "Products", "0", "BasketItem", "quantity"}
	input.Scenario.Invariant.JSONIntegerMinimum.Path = path
	input.Result.Trials[0].Run.Evaluation.JSONIntegerMinimum.Invariant.Path = append([]string(nil), path...)

	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	if !bytes.HasSuffix(output.Bytes(), []byte("\n")) {
		t.Fatal("JSON report does not end with a newline")
	}
	validateReportJSON(t, output.Bytes())

	text := output.String()
	for _, secret := range []string{"request-secret", "request-body-secret", "response-secret", `"Header"`, `"Body"`} {
		if strings.Contains(text, secret) {
			t.Fatalf("JSON report exposed unsafe request data %q:\n%s", secret, text)
		}
	}
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	if document["schema_version"] != report.JSONSchemaVersion || document["report_type"] != "run" {
		t.Fatalf("report identity = %v/%v", document["schema_version"], document["report_type"])
	}
	invariant := document["scenario"].(map[string]any)["invariant"].(map[string]any)
	if strings.Join(anyStrings(invariant["path"].([]any)), ".") != "data.Products.0.BasketItem.quantity" {
		t.Fatalf("scenario invariant path = %#v", invariant["path"])
	}
	if _, exists := invariant["field"]; exists {
		t.Fatalf("scenario invariant retained removed field: %#v", invariant)
	}
	trials := document["trials"].([]any)
	if len(trials) != 1 {
		t.Fatalf("trials = %d, want 1", len(trials))
	}
	evidence := trials[0].(map[string]any)["evidence"].(map[string]any)
	attempts := evidence["history"].(map[string]any)["attempts"].([]any)
	if len(attempts) != 2 || evidence["invariant_evaluation"] == nil {
		t.Fatalf("passing trial omitted complete evidence: %#v", evidence)
	}
	arguments := document["reproduction"].(map[string]any)["arguments"].([]any)
	if strings.Join(anyStrings(arguments), " ") != "concurtest run --format json --attempts 2 --concurrency 2 --no-reduce scenarios/inventory.yaml" {
		t.Fatalf("reproduction arguments = %#v", arguments)
	}
}

func TestWriteJSONEncodesBinaryAndBoundsResponseExcerpt(t *testing.T) {
	t.Parallel()

	input := completedTextInput("violated", -1)
	body := append([]byte{0xff, 0xfe}, bytes.Repeat([]byte("x"), 600)...)
	input.Result.Trials[0].Run.Observation.Response.Body = body

	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	trial := document["trials"].([]any)[0].(map[string]any)
	observation := trial["evidence"].(map[string]any)["observation"].(map[string]any)
	excerpt := observation["response"].(map[string]any)["body_excerpt"].(map[string]any)
	if excerpt["encoding"] != "base64" || excerpt["retained_bytes"] != float64(512) || excerpt["truncated"] != true {
		t.Fatalf("excerpt metadata = %#v", excerpt)
	}
	decoded, err := base64.StdEncoding.DecodeString(excerpt["data"].(string))
	if err != nil || len(decoded) != 512 {
		t.Fatalf("base64 excerpt decoded length = %d, error = %v", len(decoded), err)
	}
}

func TestWriteJSONReportsHistoryInvariantEvidence(t *testing.T) {
	t.Parallel()

	input := completedTextInput("violated", -1)
	definition := engine.MaximumSuccessfulAttemptsInvariant{
		Name: "accepted purchases must not exceed stock", Maximum: 1,
		SuccessfulStatusCodes: []int{http.StatusCreated},
	}
	input.Scenario.Invariant = engine.Invariant{MaximumSuccessfulAttempts: &definition}
	input.Scenario.Observation = nil
	trial := &input.Result.Trials[0]
	trial.Run.Observation = nil
	trial.Run.History.Attempts[1].Execution.Response.StatusCode = http.StatusCreated
	trial.Run.Evaluation = &engine.InvariantEvaluation{
		MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsEvaluation{
			Invariant: definition, SuccessfulAttemptIDs: []int{1, 2},
			OverLimitAttemptIDs: []int{2}, Violated: true,
		},
		Violated: true,
	}

	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	scenario := document["scenario"].(map[string]any)
	if scenario["observation"] != nil || scenario["invariant"].(map[string]any)["type"] != "maximum_successful_attempts" {
		t.Fatalf("scenario invariant = %#v", scenario)
	}
	trialDocument := document["trials"].([]any)[0].(map[string]any)
	evaluation := trialDocument["evidence"].(map[string]any)["invariant_evaluation"].(map[string]any)
	if evaluation["type"] != "maximum_successful_attempts" || len(evaluation["over_limit_attempt_ids"].([]any)) != 1 {
		t.Fatalf("history invariant evaluation = %#v", evaluation)
	}
}

func TestWriteJSONSerializesStructuredErrorCauses(t *testing.T) {
	t.Parallel()

	input := completedTextInput("passed", 0)
	input.RunError = failure.Wrap(
		failure.CodeTrialSequenceInterrupted,
		"run trial sequence",
		context.DeadlineExceeded,
	)
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	if document["status"] != "interrupted" {
		t.Fatalf("status = %v", document["status"])
	}
	problem := document["error"].(map[string]any)
	if problem["code"] != "trial_sequence_interrupted" {
		t.Fatalf("error = %#v", problem)
	}
	causes := problem["causes"].([]any)
	if len(causes) != 1 || causes[0].(map[string]any)["code"] != "deadline_exceeded" {
		t.Fatalf("causes = %#v", causes)
	}
}

func TestWriteJSONReportsReductionWithoutDuplicatingBaselineEvidence(t *testing.T) {
	t.Parallel()

	input := completedTextInput("violated", -1)
	selected := input.Result
	input.Scenario.Attempts = 4
	input.Scenario.Concurrency = 4
	input.ReductionEnabled = true
	input.Reduction = &reduction.Result{
		StartedAt: input.Result.StartedAt, CompletedAt: input.Result.CompletedAt,
		Baseline: input.Result,
		Candidates: []reduction.CandidateResult{{
			Candidate: reduction.Candidate{Attempts: 2, Concurrency: 2},
			Summary:   reduction.TrialSummary{Requested: 1, Completed: 1, Violated: 1},
			Accepted:  true, Trials: &selected,
		}},
		Selected:       reduction.Candidate{Attempts: 2, Concurrency: 2},
		SelectedTrials: &selected,
		Status:         reduction.StatusReduced,
	}

	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	reduced := document["reduction"].(map[string]any)
	baseline := reduced["baseline"].(map[string]any)
	if _, duplicates := baseline["trials"]; duplicates {
		t.Fatalf("reduction baseline duplicated full trial evidence: %#v", baseline)
	}
	candidates := reduced["candidates"].([]any)
	retained := candidates[0].(map[string]any)["trials"].([]any)
	if len(retained) != 1 {
		t.Fatalf("selected candidate retained trials = %d, want 1", len(retained))
	}
	selection := reduced["selection"].(map[string]any)
	if selection["source"] != "candidate" || selection["candidate_number"] != float64(1) {
		t.Fatalf("selection = %#v", selection)
	}
	arguments := document["reproduction"].(map[string]any)["arguments"].([]any)
	if strings.Join(anyStrings(arguments), " ") != "concurtest run --format json --attempts 2 --concurrency 2 --no-reduce scenarios/inventory.yaml" {
		t.Fatalf("reproduction arguments = %#v", arguments)
	}
}

func TestWriteJSONErrorProducesSchemaValidEarlyFailure(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	err := failure.Wrap(failure.CodeScenarioFileFailed, "open scenario file", errors.New("permission denied"))
	if writeErr := report.WriteJSONError(&output, report.ErrorInput{
		ScenarioPath: "scenario.yaml",
		Err:          err,
	}); writeErr != nil {
		t.Fatalf("WriteJSONError() error = %v", writeErr)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	if document["report_type"] != "error" || document["status"] != "errored" {
		t.Fatalf("early report = %#v", document)
	}
	contextValue := document["context"].(map[string]any)
	if contextValue["scenario_path"] != "scenario.yaml" || contextValue["scenario_name"] != nil {
		t.Fatalf("context = %#v", contextValue)
	}
}

func TestWriteJSONBoundsErrorTrees(t *testing.T) {
	t.Parallel()

	var problem error = errors.New("leaf")
	for index := 0; index < 40; index++ {
		problem = failure.Wrap(failure.CodeExternal, "layer", problem)
	}
	var output bytes.Buffer
	if err := report.WriteJSONError(&output, report.ErrorInput{Err: problem}); err != nil {
		t.Fatalf("WriteJSONError() error = %v", err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	nodes, truncated := countErrorNodes(document["error"].(map[string]any))
	if nodes > 32 || !truncated {
		t.Fatalf("serialized error nodes = %d, truncated = %t", nodes, truncated)
	}
}

func TestWriteJSONReturnsWriterErrors(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("output unavailable")
	if err := report.WriteJSON(errorWriter{err: wantErr}, completedTextInput("passed", 0)); !errors.Is(err, wantErr) {
		t.Fatalf("WriteJSON() error = %v, want wrapped writer error", err)
	}
}

func TestReportSchemaRejectsUnknownProperties(t *testing.T) {
	t.Parallel()

	input := completedTextInput("passed", 0)
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	document["future_field"] = true
	if err := compiledReportSchema(t).Validate(document); err == nil {
		t.Fatal("closed report schema accepted an unknown property")
	}
}

func validateReportJSON(t *testing.T, data []byte) {
	t.Helper()
	var document any
	decodeJSON(t, data, &document)
	if err := compiledReportSchema(t).Validate(document); err != nil {
		t.Fatalf("JSON report does not match schema: %v\n%s", err, data)
	}
}

func compiledReportSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	data, err := os.ReadFile("../../schemas/report-v1.schema.json")
	if err != nil {
		t.Fatalf("read report schema: %v", err)
	}
	var document any
	decodeJSON(t, data, &document)
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	compiler.AssertContent()
	if err := compiler.AddResource(reportSchemaID, document); err != nil {
		t.Fatalf("add report schema: %v", err)
	}
	schema, err := compiler.Compile(reportSchemaID)
	if err != nil {
		t.Fatalf("compile report schema: %v", err)
	}
	return schema
}

func decodeJSON(t *testing.T, data []byte, destination any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, data)
	}
}

func anyStrings(values []any) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = value.(string)
	}
	return result
}

func countErrorNodes(node map[string]any) (int, bool) {
	count := 1
	truncated := node["code"] == "error_tree_truncated"
	for _, child := range node["causes"].([]any) {
		childCount, childTruncated := countErrorNodes(child.(map[string]any))
		count += childCount
		truncated = truncated || childTruncated
	}
	return count, truncated
}
