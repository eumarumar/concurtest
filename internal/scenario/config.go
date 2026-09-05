// Package scenario decodes human-readable scenario definitions into engine
// inputs. It does not execute scenarios or perform network requests.
package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
	"go.yaml.in/yaml/v3"
)

const (
	supportedVersion      = 1
	maxConfigurationBytes = 1 << 20
)

// Definition contains a validated scenario and the metadata needed to run it.
type Definition struct {
	Name           string
	Target         string
	RequestTimeout time.Duration
	Trials         int
	Reduce         bool
	Scenario       engine.Scenario
}

// Decode reads and validates one YAML scenario document.
func Decode(reader io.Reader) (definition Definition, decodeErr error) {
	defer func() {
		if decodeErr != nil {
			decodeErr = failure.Wrap(failure.CodeScenarioInvalid, "decode scenario configuration", decodeErr)
		}
	}()
	if reader == nil {
		return Definition{}, errors.New("read scenario: no input provided")
	}

	data, err := io.ReadAll(io.LimitReader(reader, maxConfigurationBytes+1))
	if err != nil {
		return Definition{}, fmt.Errorf("read scenario: %w", err)
	}
	if len(data) > maxConfigurationBytes {
		return Definition{}, errors.New("read scenario: file is larger than 1 MiB")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var document documentConfig
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return Definition{}, errors.New("read scenario: file is empty")
		}
		return Definition{}, fmt.Errorf("read scenario YAML: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Definition{}, errors.New("read scenario YAML: only one document is allowed")
	} else if !errors.Is(err, io.EOF) {
		return Definition{}, fmt.Errorf("read trailing scenario YAML: %w", err)
	}

	return document.definition()
}

type documentConfig struct {
	Version        strictInt       `yaml:"version"`
	Name           strictString    `yaml:"name"`
	Target         strictString    `yaml:"target"`
	RequestTimeout strictString    `yaml:"request_timeout"`
	Setup          *requestConfig  `yaml:"setup"`
	Operation      operationConfig `yaml:"operation"`
	Execution      executionConfig `yaml:"execution"`
	Observation    *requestConfig  `yaml:"observation"`
	Invariant      invariantConfig `yaml:"invariant"`
}

type requestConfig struct {
	Method  strictString `yaml:"method"`
	Path    strictString `yaml:"path"`
	Headers headerConfig `yaml:"headers"`
	Body    strictString `yaml:"body"`
}

type operationConfig struct {
	Name    strictString `yaml:"name"`
	Method  strictString `yaml:"method"`
	Path    strictString `yaml:"path"`
	Headers headerConfig `yaml:"headers"`
	Body    strictString `yaml:"body"`
}

func (operation operationConfig) request() requestConfig {
	return requestConfig{
		Method:  operation.Method,
		Path:    operation.Path,
		Headers: operation.Headers,
		Body:    operation.Body,
	}
}

type executionConfig struct {
	Attempts    strictInt  `yaml:"attempts"`
	Concurrency strictInt  `yaml:"concurrency"`
	Trials      strictInt  `yaml:"trials"`
	Reduce      strictBool `yaml:"reduce"`
}

type invariantConfig struct {
	Name                      strictString   `yaml:"name"`
	JSONIntegerPath           jsonPathConfig `yaml:"json_integer_path"`
	Minimum                   *strictInt64   `yaml:"minimum"`
	MaximumSuccessfulAttempts *strictInt     `yaml:"maximum_successful_attempts"`
	SuccessfulStatusCodes     strictIntList  `yaml:"successful_status_codes"`
}

func (config *invariantConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("invariant must be a mapping")
	}
	known := map[string]struct{}{
		"name":                        {},
		"json_integer_path":           {},
		"minimum":                     {},
		"maximum_successful_attempts": {},
		"successful_status_codes":     {},
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		value := node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return errors.New("invariant field names must be strings")
		}
		if _, exists := known[key.Value]; !exists {
			return fmt.Errorf("field %s not found in type scenario.invariantConfig", key.Value)
		}
		if _, exists := seen[key.Value]; exists {
			return fmt.Errorf("invariant field %q is repeated", key.Value)
		}
		seen[key.Value] = struct{}{}
		if key.Value == "successful_status_codes" && value.Tag == "!!null" {
			return errors.New("invariant.successful_status_codes must be a list of integers")
		}
	}

	type plainInvariantConfig invariantConfig
	var decoded plainInvariantConfig
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*config = invariantConfig(decoded)
	return nil
}

type strictString string

func (value *strictString) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("must be a string")
	}
	*value = strictString(node.Value)
	return nil
}

type strictInt int

func (value *strictInt) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return errors.New("must be an integer")
	}
	parsed, err := strconv.ParseInt(node.Value, 10, 0)
	if err != nil {
		return fmt.Errorf("must be a base-10 integer: %w", err)
	}
	*value = strictInt(parsed)
	return nil
}

type strictBool bool

func (value *strictBool) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return errors.New("must be a boolean")
	}
	switch node.Value {
	case "true":
		*value = true
		return nil
	case "false":
		*value = false
		return nil
	default:
		return errors.New("must be true or false")
	}
}

type strictInt64 int64

func (value *strictInt64) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return errors.New("must be an integer")
	}
	parsed, err := strconv.ParseInt(node.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("must be a base-10 integer: %w", err)
	}
	*value = strictInt64(parsed)
	return nil
}

type strictIntList struct {
	configured bool
	values     []strictInt
}

type jsonPathConfig struct {
	configured bool
	values     []string
}

func (value *jsonPathConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return errors.New("json_integer_path must be a list of object keys or non-negative integer indexes")
	}
	value.configured = true
	value.values = make([]string, len(node.Content))
	for index, item := range node.Content {
		if item.Kind != yaml.ScalarNode {
			return fmt.Errorf("json_integer_path entry %d must be an object key or a non-negative integer index", index+1)
		}
		switch item.Tag {
		case "!!str":
			value.values[index] = item.Value
		case "!!int":
			var parsed strictInt
			if err := item.Decode(&parsed); err != nil {
				return fmt.Errorf("json_integer_path entry %d: %w", index+1, err)
			}
			if parsed < 0 {
				return fmt.Errorf("json_integer_path entry %d must not be negative", index+1)
			}
			value.values[index] = strconv.Itoa(int(parsed))
		default:
			return fmt.Errorf("json_integer_path entry %d must be an object key or a non-negative integer index", index+1)
		}
	}
	return nil
}

func (value *strictIntList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return errors.New("must be a list of integers")
	}
	value.configured = true
	value.values = make([]strictInt, len(node.Content))
	for index, item := range node.Content {
		if err := item.Decode(&value.values[index]); err != nil {
			return fmt.Errorf("entry %d must be an integer: %w", index+1, err)
		}
	}
	return nil
}

type headerConfig map[string]string

func (headers *headerConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		*headers = nil
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return errors.New("headers must contain string names and values")
	}

	decoded := make(headerConfig, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		name := node.Content[index]
		value := node.Content[index+1]
		if name.Kind != yaml.ScalarNode || name.Tag != "!!str" {
			return errors.New("header names must be strings")
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return fmt.Errorf("header value for %q must be a string", name.Value)
		}
		if _, exists := decoded[name.Value]; exists {
			return fmt.Errorf("header name %q is repeated", name.Value)
		}
		decoded[name.Value] = value.Value
	}
	*headers = decoded
	return nil
}

func (document documentConfig) definition() (Definition, error) {
	if document.Version != supportedVersion {
		return Definition{}, fmt.Errorf("version must be %d, got %d", supportedVersion, document.Version)
	}

	name := strings.TrimSpace(string(document.Name))
	if name == "" {
		return Definition{}, errors.New("name must not be empty")
	}

	target, err := parseTarget(string(document.Target))
	if err != nil {
		return Definition{}, err
	}

	requestTimeout, err := time.ParseDuration(string(document.RequestTimeout))
	if err != nil {
		return Definition{}, fmt.Errorf("request_timeout must be a duration such as 2s: %w", err)
	}
	if requestTimeout <= 0 {
		return Definition{}, errors.New("request_timeout must be greater than zero")
	}

	operationName := strings.TrimSpace(string(document.Operation.Name))
	if operationName == "" {
		return Definition{}, errors.New("operation.name must not be empty")
	}
	if document.Execution.Attempts <= 0 {
		return Definition{}, errors.New("execution.attempts must be greater than zero")
	}
	if document.Execution.Concurrency <= 0 {
		return Definition{}, errors.New("execution.concurrency must be greater than zero")
	}
	if document.Execution.Concurrency > document.Execution.Attempts {
		return Definition{}, fmt.Errorf(
			"execution.concurrency must not exceed execution.attempts (%d)",
			document.Execution.Attempts,
		)
	}
	if document.Execution.Trials < 1 || document.Execution.Trials > engine.MaxTrials {
		return Definition{}, fmt.Errorf(
			"execution.trials must be between 1 and %d",
			engine.MaxTrials,
		)
	}
	if document.Execution.Reduce {
		if document.Setup == nil {
			return Definition{}, errors.New("execution.reduce requires setup so every candidate trial can reset state")
		}
		if document.Execution.Trials < 3 {
			return Definition{}, errors.New("execution.trials must be at least 3 when execution.reduce is true")
		}
		if document.Execution.Attempts < 2 {
			return Definition{}, errors.New("execution.attempts must be at least 2 when execution.reduce is true")
		}
		if document.Execution.Concurrency < 2 {
			return Definition{}, errors.New("execution.concurrency must be at least 2 when execution.reduce is true")
		}
	}

	invariantName := strings.TrimSpace(string(document.Invariant.Name))
	if invariantName == "" {
		return Definition{}, errors.New("invariant.name must not be empty")
	}
	invariant, err := document.Invariant.invariant(invariantName)
	if err != nil {
		return Definition{}, err
	}
	if invariant.JSONIntegerMinimum != nil && document.Observation == nil {
		return Definition{}, errors.New("observation is required for a JSON integer minimum invariant")
	}

	var setup *engine.HTTPRequest
	if document.Setup != nil {
		request, err := document.Setup.httpRequest("setup", target)
		if err != nil {
			return Definition{}, err
		}
		setup = &request
	}

	operationRequest, err := document.Operation.request().httpRequest("operation", target)
	if err != nil {
		return Definition{}, err
	}
	var observation *engine.HTTPRequest
	if document.Observation != nil {
		request, err := document.Observation.httpRequest("observation", target)
		if err != nil {
			return Definition{}, err
		}
		observation = &request
	}

	return Definition{
		Name:           name,
		Target:         target.String(),
		RequestTimeout: requestTimeout,
		Trials:         int(document.Execution.Trials),
		Reduce:         bool(document.Execution.Reduce),
		Scenario: engine.Scenario{
			Setup: setup,
			Operation: engine.Operation{
				Name:    operationName,
				Request: operationRequest,
			},
			Attempts:    int(document.Execution.Attempts),
			Concurrency: int(document.Execution.Concurrency),
			Observation: observation,
			Invariant:   invariant,
		},
	}, nil
}

func (config invariantConfig) invariant(name string) (engine.Invariant, error) {
	pathConfigured := config.JSONIntegerPath.configured
	minimumConfigured := config.Minimum != nil
	maximumConfigured := config.MaximumSuccessfulAttempts != nil
	statusesConfigured := config.SuccessfulStatusCodes.configured

	if pathConfigured || minimumConfigured {
		if maximumConfigured || statusesConfigured {
			return engine.Invariant{}, errors.New("invariant must define either a JSON integer path or maximum successful attempts, not both")
		}
		if !pathConfigured {
			return engine.Invariant{}, errors.New("invariant.json_integer_path is required")
		}
		if !minimumConfigured {
			return engine.Invariant{}, errors.New("invariant.minimum is required")
		}
		if len(config.JSONIntegerPath.values) == 0 {
			return engine.Invariant{}, errors.New("invariant.json_integer_path must not be empty")
		}
		path := make([]string, len(config.JSONIntegerPath.values))
		for index, segment := range config.JSONIntegerPath.values {
			if strings.TrimSpace(segment) == "" {
				return engine.Invariant{}, fmt.Errorf("invariant.json_integer_path entry %d must not be empty", index+1)
			}
			path[index] = segment
		}
		definition := engine.JSONIntegerMinimumInvariant{
			Name:    name,
			Path:    path,
			Minimum: int64(*config.Minimum),
		}
		return engine.Invariant{JSONIntegerMinimum: &definition}, nil
	}

	if !maximumConfigured {
		if statusesConfigured {
			return engine.Invariant{}, errors.New("invariant.maximum_successful_attempts is required with successful_status_codes")
		}
		return engine.Invariant{}, errors.New("invariant must define json_integer_path and minimum or maximum_successful_attempts")
	}
	if *config.MaximumSuccessfulAttempts < 0 {
		return engine.Invariant{}, errors.New("invariant.maximum_successful_attempts must not be negative")
	}
	if statusesConfigured && len(config.SuccessfulStatusCodes.values) == 0 {
		return engine.Invariant{}, errors.New("invariant.successful_status_codes must not be empty when provided")
	}

	statuses := []int(nil)
	if statusesConfigured {
		statuses = make([]int, 0, len(config.SuccessfulStatusCodes.values))
		seen := make(map[int]struct{}, len(config.SuccessfulStatusCodes.values))
		for _, configured := range config.SuccessfulStatusCodes.values {
			status := int(configured)
			if status < 100 || status > 599 {
				return engine.Invariant{}, fmt.Errorf(
					"invariant.successful_status_codes entries must be between 100 and 599, got %d",
					status,
				)
			}
			if _, exists := seen[status]; exists {
				return engine.Invariant{}, fmt.Errorf(
					"invariant.successful_status_codes must not repeat HTTP status %d",
					status,
				)
			}
			seen[status] = struct{}{}
			statuses = append(statuses, status)
		}
	}

	definition := engine.MaximumSuccessfulAttemptsInvariant{
		Name:                  name,
		Maximum:               int(*config.MaximumSuccessfulAttempts),
		SuccessfulStatusCodes: statuses,
	}
	return engine.Invariant{MaximumSuccessfulAttempts: &definition}, nil
}

func parseTarget(rawTarget string) (*url.URL, error) {
	if strings.TrimSpace(rawTarget) == "" {
		return nil, errors.New("target must not be empty")
	}

	target, err := url.Parse(rawTarget)
	if err != nil {
		return nil, fmt.Errorf("target must be a valid URL: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, errors.New("target must use http or https")
	}
	if target.Host == "" || target.Hostname() == "" {
		return nil, errors.New("target must include a host")
	}
	if target.User != nil {
		return nil, errors.New("target must not contain a username or password; use request headers instead")
	}
	if target.RawQuery != "" || target.ForceQuery {
		return nil, errors.New("target must not contain a query string; add it to a request path instead")
	}
	if target.Fragment != "" {
		return nil, errors.New("target must not contain a fragment")
	}
	if target.Path != "" && target.Path != "/" {
		return nil, errors.New("target must not contain a path; add it to each request instead")
	}

	target.Path = ""
	target.RawPath = ""
	return target, nil
}

func (request requestConfig) httpRequest(label string, target *url.URL) (engine.HTTPRequest, error) {
	method := string(request.Method)
	if strings.TrimSpace(method) == "" {
		return engine.HTTPRequest{}, fmt.Errorf("%s.method must not be empty", label)
	}
	if method != strings.TrimSpace(method) {
		return engine.HTTPRequest{}, fmt.Errorf("%s.method must not start or end with whitespace", label)
	}
	path := string(request.Path)
	if strings.TrimSpace(path) == "" {
		return engine.HTTPRequest{}, fmt.Errorf("%s.path must not be empty", label)
	}
	if path != strings.TrimSpace(path) {
		return engine.HTTPRequest{}, fmt.Errorf("%s.path must not start or end with whitespace", label)
	}
	if !strings.HasPrefix(path, "/") {
		return engine.HTTPRequest{}, fmt.Errorf("%s.path must start with /", label)
	}

	reference, err := url.Parse(path)
	if err != nil {
		return engine.HTTPRequest{}, fmt.Errorf("%s.path must be a valid URL path: %w", label, err)
	}
	if reference.IsAbs() || reference.Host != "" || reference.Opaque != "" {
		return engine.HTTPRequest{}, fmt.Errorf("%s.path must be relative to target", label)
	}
	if reference.Fragment != "" {
		return engine.HTTPRequest{}, fmt.Errorf("%s.path must not contain a fragment", label)
	}
	if reference.Path == "" {
		return engine.HTTPRequest{}, fmt.Errorf("%s.path must include a path before any query string", label)
	}

	headers, err := validatedHeaders(label, map[string]string(request.Headers))
	if err != nil {
		return engine.HTTPRequest{}, err
	}
	resolvedURL := target.ResolveReference(reference).String()

	body := string(request.Body)
	_, err = http.NewRequest(method, resolvedURL, nil)
	if err != nil {
		return engine.HTTPRequest{}, fmt.Errorf("%s request is invalid: %w", label, err)
	}

	return engine.HTTPRequest{
		Method: method,
		URL:    resolvedURL,
		Header: headers,
		Body:   []byte(body),
	}, nil
}

func validatedHeaders(label string, configured map[string]string) (http.Header, error) {
	if len(configured) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)

	headers := make(http.Header, len(configured))
	seen := make(map[string]string, len(configured))
	for _, name := range names {
		if !validHeaderName(name) {
			return nil, fmt.Errorf("%s.headers contains an invalid name: %q", label, name)
		}
		key := strings.ToLower(name)
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf(
				"%s.headers contains the same name twice: %q and %q",
				label,
				previous,
				name,
			)
		}
		if !validHeaderValue(configured[name]) {
			return nil, fmt.Errorf("%s.headers contains an invalid value for %q", label, name)
		}
		seen[key] = name
		headers.Set(name, configured[name])
	}
	return headers, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		if !isHeaderTokenByte(name[index]) {
			return false
		}
	}
	return true
}

func isHeaderTokenByte(value byte) bool {
	switch {
	case '0' <= value && value <= '9':
		return true
	case 'a' <= value && value <= 'z':
		return true
	case 'A' <= value && value <= 'Z':
		return true
	}

	switch value {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

func validHeaderValue(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\t' {
			continue
		}
		if character < ' ' || character == 0x7f {
			return false
		}
	}
	return true
}
