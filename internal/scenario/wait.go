package scenario

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

type waitConfig struct {
	Request   *requestConfig   `yaml:"request"`
	Assertion *assertionConfig `yaml:"assert"`
	Timeout   strictString     `yaml:"timeout"`
	Interval  strictString     `yaml:"interval"`
	MaxPolls  *strictInt       `yaml:"max_polls"`
}

func (config waitConfig) condition(label string, target *url.URL) (*engine.WaitCondition, error) {
	if config.Request == nil || config.Assertion == nil {
		return nil, errors.New(label + " requires request and assert")
	}
	request, err := config.Request.httpRequest(label, target)
	if err != nil {
		return nil, err
	}
	assertion, err := config.Assertion.assertion()
	if err != nil {
		return nil, fmt.Errorf("%s assertion: %w", label, err)
	}
	timeout, err := time.ParseDuration(string(config.Timeout))
	if err != nil {
		return nil, fmt.Errorf("%s.timeout must be a duration such as 2s: %w", label, err)
	}
	interval, err := time.ParseDuration(string(config.Interval))
	if err != nil {
		return nil, fmt.Errorf("%s.interval must be a duration such as 25ms: %w", label, err)
	}
	condition := &engine.WaitCondition{Request: request, Assertion: assertion, Timeout: timeout, Interval: interval}
	if config.MaxPolls != nil {
		if *config.MaxPolls < 1 || *config.MaxPolls > engine.MaxWaitPolls {
			return nil, fmt.Errorf("%s.max_polls must be between 1 and %d", label, engine.MaxWaitPolls)
		}
		condition.MaxPolls = int(*config.MaxPolls)
	}
	if err := condition.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return condition, nil
}
