package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/eumarumar/concurtest/internal/failure"
)

const (
	// MaxWaitPolls bounds configured polling exchanges and retained evidence.
	MaxWaitPolls = 100
	// MaxWaitBodyBytes bounds each retained poll's response evidence. Assertions
	// are evaluated before trimming, using ExecuteHTTP's complete-body limit.
	MaxWaitBodyBytes = 512
)

// WaitCondition polls an application-defined terminal or readiness condition.
// It does not infer quiescence from stable values or successful HTTP statuses.
type WaitCondition struct {
	Request   HTTPRequest
	Assertion ResponseAssertion
	Timeout   time.Duration
	Interval  time.Duration
	MaxPolls  int // Zero uses MaxWaitPolls.
}

// PollLimit returns the effective bounded number of HTTP polls.
func (c WaitCondition) PollLimit() int {
	if c.MaxPolls == 0 {
		return MaxWaitPolls
	}
	return c.MaxPolls
}

// Validate rejects unbounded or invalid waits before any scenario traffic.
func (c WaitCondition) Validate() error {
	if c.Timeout <= 0 || c.Interval <= 0 {
		return fmt.Errorf("wait timeout and interval must be greater than zero")
	}
	if c.PollLimit() < 1 || c.PollLimit() > MaxWaitPolls {
		return fmt.Errorf("wait max_polls must be between 1 and %d", MaxWaitPolls)
	}
	return c.Assertion.Validate()
}

// WaitPoll retains safe request metadata, a bounded response excerpt, and the
// scalar evaluated from the full response. Headers and request bodies are not
// duplicated. Response.BodyTruncated also identifies evidence trimmed to 512 bytes.
type WaitPoll struct {
	Execution HTTPExecution
	Assertion *AssertionEvaluation
}

// WaitResult retains every attempted poll in order, including a failed exchange.
// Condition snapshots the predicate and safe request metadata, without headers
// or a request body; it is evidence, not a request to replay.
type WaitResult struct {
	Condition   WaitCondition
	StartedAt   time.Time
	CompletedAt time.Time
	Polls       []WaitPoll
	Satisfied   bool
	Err         error
}

// waitForCondition belongs to the run goroutine. Each response is closed before
// evaluating it or waiting. The child context bounds both HTTP execution and
// intervals; timers are stopped on every path and no polling goroutine is added.
func waitForCondition(ctx context.Context, client *http.Client, condition WaitCondition, stage string) (result WaitResult) {
	condition.Request = cloneHTTPRequest(condition.Request)
	condition.Assertion = cloneResponseAssertion(condition.Assertion)
	result.Condition = condition
	result.Condition.Request.Header = nil
	result.Condition.Request.Body = nil
	result.StartedAt = time.Now()
	defer func() { result.CompletedAt = time.Now() }()
	waitCtx, cancel := context.WithTimeout(ctx, condition.Timeout)
	defer cancel()
	code := failure.CodeCompletionFailed
	if stage == "readiness" {
		code = failure.CodeReadinessFailed
	}
	fail := func(err error) {
		result.Err = failure.Wrap(code, stage+" condition was not verified; further trials and candidates must not start", err)
	}
	for index := 0; index < condition.PollLimit(); index++ {
		if err := waitCtx.Err(); err != nil {
			fail(err)
			return result
		}
		execution := ExecuteHTTP(waitCtx, client, condition.Request)
		poll := WaitPoll{Execution: execution}
		err := requireSuccessfulStage(stage, execution)
		if err == nil && execution.Response.BodyTruncated {
			err = failure.New(failure.CodeResponseTruncated, "condition response body was truncated")
		}
		if err == nil {
			evaluation, evaluationErr := EvaluateResponseAssertion(condition.Assertion, execution.Response.Body)
			poll.Assertion = &evaluation
			err = evaluationErr
		}
		result.Polls = append(result.Polls, retainWaitPoll(poll))
		if err != nil {
			fail(err)
			return result
		}
		// A response arriving after the deadline must not establish completion.
		if err := waitCtx.Err(); err != nil {
			fail(err)
			return result
		}
		if poll.Assertion.Satisfied {
			result.Satisfied = true
			return result
		}
		if index+1 == condition.PollLimit() {
			fail(failure.New(failure.CodeWaitLimitReached, fmt.Sprintf("condition did not match within %d polls", condition.PollLimit())))
			return result
		}
		timer := time.NewTimer(condition.Interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			fail(waitCtx.Err())
			return result
		case <-timer.C:
			timer.Stop()
		}
	}
	return result
}

func retainWaitPoll(poll WaitPoll) WaitPoll {
	poll.Execution.Request.Header = nil
	poll.Execution.Request.Body = nil
	if response := poll.Execution.Response; response != nil {
		response.Header = nil
		if len(response.Body) > MaxWaitBodyBytes {
			response.Body = bytes.Clone(response.Body[:MaxWaitBodyBytes])
			response.BodyTruncated = true
		}
	}
	return poll
}

// WaitFailed identifies a boundary whose readiness/completion was not verified.
// It prevents further resets even when ordinary trial errors would allow them.
func (r RunResult) WaitFailed() bool {
	return (r.Readiness != nil && r.Readiness.Err != nil) || (r.Completion != nil && r.Completion.Err != nil)
}

// WaitFailed also identifies a trial sequence stopped early at an unverified
// boundary. The failed trial and its error remain in Trials.
func (r TrialsResult) WaitFailed() bool {
	for _, trial := range r.Trials {
		if trial.Run.WaitFailed() {
			return true
		}
	}
	return false
}
