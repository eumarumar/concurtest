package scenario

import (
	"errors"
	"fmt"
	"strings"

	"github.com/eumarumar/concurtest/internal/engine"
	"go.yaml.in/yaml/v3"
)

type assertionConfig struct {
	Name            strictString   `yaml:"name"`
	JSONIntegerPath jsonPathConfig `yaml:"json_integer_path"`
	JSONBooleanPath jsonPathConfig `yaml:"json_boolean_path"`
	Minimum         *strictInt64   `yaml:"minimum"`
	Maximum         *strictInt64   `yaml:"maximum"`
	Equals          yaml.Node      `yaml:"equals"`
}

// Setup's custom decoder uses node.Decode, so nested assertions must enforce
// known fields themselves rather than relying on the outer YAML decoder.
func (config *assertionConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := checkMappingFields(node, "name", "json_integer_path", "json_boolean_path", "minimum", "maximum", "equals"); err != nil {
		return err
	}
	type plainAssertion assertionConfig
	var decoded plainAssertion
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*config = assertionConfig(decoded)
	return nil
}

func (config assertionConfig) assertion() (engine.ResponseAssertion, error) {
	name := strings.TrimSpace(string(config.Name))
	if name == "" {
		return engine.ResponseAssertion{}, errors.New("assert.name must not be empty")
	}
	if config.JSONIntegerPath.configured == config.JSONBooleanPath.configured {
		return engine.ResponseAssertion{}, errors.New("assert must define exactly one json_integer_path or json_boolean_path")
	}
	if config.JSONBooleanPath.configured {
		if config.Minimum != nil || config.Maximum != nil || config.Equals.Kind != yaml.ScalarNode || config.Equals.Tag != "!!bool" {
			return engine.ResponseAssertion{}, errors.New("Boolean assertion requires equals: true or equals: false without integer bounds")
		}
		var equals strictBool
		if err := config.Equals.Decode(&equals); err != nil {
			return engine.ResponseAssertion{}, err
		}
		assertion := engine.ResponseAssertion{JSONBoolean: &engine.JSONBooleanAssertion{
			Name: name, Path: append([]string{}, config.JSONBooleanPath.values...), Equals: bool(equals),
		}}
		return assertion, assertion.Validate()
	}
	var equals *strictInt64
	if config.Equals.Kind != 0 {
		if config.Equals.Kind != yaml.ScalarNode || config.Equals.Tag != "!!int" {
			return engine.ResponseAssertion{}, errors.New("integer assertion equals must be an integer")
		}
		equals = new(strictInt64)
		if err := config.Equals.Decode(equals); err != nil {
			return engine.ResponseAssertion{}, fmt.Errorf("integer assertion equals: %w", err)
		}
	}
	definition, err := (invariantConfig{
		JSONIntegerPath: config.JSONIntegerPath, Minimum: config.Minimum,
		Maximum: config.Maximum, Equals: equals,
	}).invariant(name)
	if err != nil {
		return engine.ResponseAssertion{}, err
	}
	assertion := engine.ResponseAssertion{JSONInteger: definition.JSONInteger}
	return assertion, assertion.Validate()
}

type preconditionConfig struct {
	Request   *requestConfig   `yaml:"request"`
	Assertion *assertionConfig `yaml:"assert"`
}
