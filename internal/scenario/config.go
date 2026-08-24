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
	Scenario       engine.Scenario
}

// Decode reads and validates one YAML scenario document.
func Decode(reader io.Reader) (Definition, error) {
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
	Observation    requestConfig   `yaml:"observation"`
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
	Attempts    strictInt `yaml:"attempts"`
	Concurrency strictInt `yaml:"concurrency"`
	Trials      strictInt `yaml:"trials"`
}

type invariantConfig struct {
	Name             strictString `yaml:"name"`
	JSONIntegerField strictString `yaml:"json_integer_field"`
	Minimum          *strictInt64 `yaml:"minimum"`
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

	invariantName := strings.TrimSpace(string(document.Invariant.Name))
	if invariantName == "" {
		return Definition{}, errors.New("invariant.name must not be empty")
	}
	invariantField := string(document.Invariant.JSONIntegerField)
	if strings.TrimSpace(invariantField) == "" {
		return Definition{}, errors.New("invariant.json_integer_field must not be empty")
	}
	if document.Invariant.Minimum == nil {
		return Definition{}, errors.New("invariant.minimum is required")
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
	observationRequest, err := document.Observation.httpRequest("observation", target)
	if err != nil {
		return Definition{}, err
	}

	return Definition{
		Name:           name,
		Target:         target.String(),
		RequestTimeout: requestTimeout,
		Trials:         int(document.Execution.Trials),
		Scenario: engine.Scenario{
			Setup: setup,
			Operation: engine.Operation{
				Name:    operationName,
				Request: operationRequest,
			},
			Attempts:    int(document.Execution.Attempts),
			Concurrency: int(document.Execution.Concurrency),
			Observation: observationRequest,
			Invariant: engine.JSONIntegerMinimumInvariant{
				Name:    invariantName,
				Field:   invariantField,
				Minimum: int64(*document.Invariant.Minimum),
			},
		},
	}, nil
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
