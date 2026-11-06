package scenario

import (
	"fmt"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"go.yaml.in/yaml/v3"
)

type limitsConfig struct {
	MaxAttempts      *strictInt    `yaml:"max_attempts"`
	MaxConcurrency   *strictInt    `yaml:"max_concurrency"`
	MaxTotalAttempts *strictInt64  `yaml:"max_total_attempts"`
	MaxExchanges     *strictInt64  `yaml:"max_exchanges"`
	MaxRetainedBytes *strictInt64  `yaml:"max_retained_bytes"`
	MaxDuration      *strictString `yaml:"max_duration"`
	MaxReportBytes   *strictInt64  `yaml:"max_report_bytes"`
}

func (c *limitsConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := checkMappingFields(node, "max_attempts", "max_concurrency", "max_total_attempts", "max_exchanges", "max_retained_bytes", "max_duration", "max_report_bytes"); err != nil {
		return err
	}
	for index := 1; index < len(node.Content); index += 2 {
		if node.Content[index].Tag == "!!null" {
			return fmt.Errorf("limits values must not be null")
		}
	}
	type plain limitsConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = limitsConfig(decoded)
	return nil
}

func (c limitsConfig) limits() (engine.Limits, error) {
	l := engine.Limits{}
	for _, field := range []struct {
		name   string
		value  *strictInt
		target *int
	}{
		{"max_attempts", c.MaxAttempts, &l.MaxAttempts}, {"max_concurrency", c.MaxConcurrency, &l.MaxConcurrency},
	} {
		if field.value != nil {
			if *field.value <= 0 {
				return l, fmt.Errorf("limits.%s must be positive", field.name)
			}
			*field.target = int(*field.value)
		}
	}
	for _, field := range []struct {
		name   string
		value  *strictInt64
		target *int64
	}{
		{"max_total_attempts", c.MaxTotalAttempts, &l.MaxTotalAttempts}, {"max_exchanges", c.MaxExchanges, &l.MaxExchanges},
		{"max_retained_bytes", c.MaxRetainedBytes, &l.MaxRetainedBytes}, {"max_report_bytes", c.MaxReportBytes, &l.MaxReportBytes},
	} {
		if field.value != nil {
			if *field.value <= 0 {
				return l, fmt.Errorf("limits.%s must be positive", field.name)
			}
			*field.target = int64(*field.value)
		}
	}
	if c.MaxDuration != nil {
		duration, err := time.ParseDuration(string(*c.MaxDuration))
		if err != nil || duration <= 0 {
			return l, fmt.Errorf("limits.max_duration must be a positive duration")
		}
		l.MaxDuration = duration
	}
	return l, l.Validate()
}
