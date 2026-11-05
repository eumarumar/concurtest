# Scenario reference

Scenarios declare preparation requests, one concurrent operation, and one
invariant. The [inventory observation scenario](../examples/vulnerable-inventory/observation-scenario.yaml)
is a complete runnable example. Start with the [quickstart](../README.md#quickstart--reproduce-a-race-condition).

All commands below run from the repository root after installing ConcurTest.

## HTTP requests, retries, and redirects

Setup, operations, and observations use the same HTTP behavior. ConcurTest does
not follow redirects: it records the original 3xx status and response body. Use
the intended URL directly. A redirect during setup or observation fails that
stage because those stages require 2xx responses. An operation's 3xx response
is evaluated against the declared status rule; it is not a transport error.

Go's HTTP transport can retry requests after connection failures, including
POST requests carrying an `Idempotency-Key` or `X-Idempotency-Key` header.
ConcurTest preserves these headers and connection reuse. It records observed
request-header writes in each JSON execution's `request_writes` field. More
than one produces a `request_retried` execution error while retaining the final
response. Text reports show the repeated-write count alongside the error;
verbose reports show the count for every execution.

A retried operation makes an otherwise passing trial inconclusive (exit 2).
A demonstrated invariant violation still takes precedence (exit 1). A retried
setup or observation fails that stage. Retries are detected after the exchange;
this does not prevent duplicate effects or roll them back.

The count describes observed header writes, not confirmed delivery or committed
effects. Headers may be buffered before a failed send, and incomplete header
writes may not be counted. A zero count means no header writes were observed;
custom Go transports that omit HTTP trace events cannot be checked this way.
Retries within a proxy or the target are outside this evidence. A 3xx or other
nonmatching response can satisfy the literal upper-bound invariant, but the
operation outcome requirements below must also be met for a clean pass.

## Operation outcomes and useful work

An invariant holding is only one condition for a passing trial. Each trial must
also meet its HTTP outcome policy. Without an `outcomes` section, ConcurTest
requires at least one accepted response and treats other statuses as unexpected.
Acceptance uses the history invariant's `successful_status_codes` when that
list is configured; otherwise it accepts HTTP 200–299.

Declare expected rejections for the application you are testing:

```yaml
outcomes:
  accepted_status_codes: [201]
  rejected_status_codes: [400, 409]
  minimum_accepted_attempts: 1
```

This example requires at least one 201 response in each trial, allows 400 and
409 as expected rejections, and treats every other status as unexpected. All
fields are optional. `accepted_status_codes` overrides the default acceptance
filter; `rejected_status_codes` defaults to no expected rejections. Explicit
status lists must use unique integers from 100 through 599. Acceptance and
rejection lists must not overlap, including the effective default acceptance
filter. An explicitly empty acceptance list is invalid.

`minimum_accepted_attempts` defaults to 1 and must be between zero and the
configured attempt count. It counts clean accepted responses within each trial;
earlier trials, setup, and observations cannot satisfy it. For a test deliberately
expecting every operation to be rejected, declare a zero minimum:

```yaml
outcomes:
  rejected_status_codes: [409]
  minimum_accepted_attempts: 0
```

A zero minimum does not excuse unexpected statuses or exchange errors. A history
rule requiring zero successful responses usually needs this explicit minimum.
The history invariant's status filter still determines what its upper bound
counts. An explicit acceptance override does not rewrite that invariant.

Outcome evidence separates accepted and rejected responses from unexpected
responses and unknown executions. Unknown executions include missing attempts,
transport failures, implicit retries, and response read/close failures. A clean
HTTP 500 remains an HTTP response rather than becoming a transport error; its
effects are uncertain. Expected rejection is also a declared response contract,
not a verified absence of side effects.

Unexpected responses, unknown executions, or a missed minimum make an otherwise
passing trial inconclusive (exit 2). A demonstrated invariant violation still
takes precedence (exit 1). State observation continues after unexpected operation
responses, allowing a state rule to expose effects committed before a 500.
Setup and observation retain their separate 2xx requirements.

Text reports show the effective policy, outcome counts, and reasons for missing
coverage. JSON includes `scenario.outcomes` and each trial's `outcome_evaluation`
with stable attempt-ID lists, the effective minimum, and `requirements_met`.
The evaluation is null when no operation history was available.

These checks establish response coverage under the declared contract. A 2xx
response can still mean empty work, an idempotent replay, or an application error
inside the body. Choose explicit acceptance statuses and meaningful state checks
where needed; response counts alone do not establish durable business effects.

## Prepare state before each trial

The optional `setup` field defines preparation requests that run before each
trial. A single request uses the following mapping:

```yaml
setup:
  method: POST
  path: /testing/reset-inventory
```

For several preparation requests, use a list of named steps:

```yaml
setup:
  - name: Reset inventory
    request:
      method: POST
      path: /testing/reset-inventory
      headers:
        Content-Type: application/json
      body: '{"ProductId":6,"quantity":1000}'

  - name: Clear basket
    request:
      method: DELETE
      path: /testing/basket/2/items

  - name: Prepare basket
    request:
      method: POST
      path: /api/BasketItems
      headers:
        Content-Type: application/json
      body: '{"BasketId":2,"ProductId":6,"quantity":1}'
```

The example paths are illustrative and must exist in the target application.
Each request supports its own headers and body. List entries require a non-empty
`name` and a `request`. Setup supports at most 100 steps and can be omitted when
preparation is not needed. The single-request mapping remains supported.

ConcurTest runs setup requests in YAML order, waiting for each response to be
read and closed before starting the next request. All steps run before every
trial, including reduction trials. A change invariant captures its baseline
after setup and the optional precondition check complete. Only the test operation
runs concurrently.

Each setup response must have a 2xx status. A failed request, timeout, or non-2xx
response stops that trial before any remaining setup steps, baseline observation,
or test operations run. The report retains the attempted steps, including the
failed step. Later trials start setup again from the first step; cancellation
stops the trial sequence. Completed steps are not rolled back, so setup must
also work after a partially prepared or previously tested state.

Setup establishes controlled preconditions when its requests reset all state
relevant to the invariant. Reading a fresh baseline does not remove side effects
from previous trials.

Verbose text shows every configured setup step and marks steps not reached.
Compact text identifies the failed step. In JSON, `scenario.setup` is an ordered
array of `{name, request, assertion}` objects, and each trial's `evidence.setup` is
an ordered array of `{name, execution, assertion}` objects for attempted steps.
The assertion fields are null when no check was declared or no evaluation was
available. An empty array (`[]`)
represents no steps. A single-request mapping becomes one step with an empty
name.

## Verify setup responses and initial state

A 2xx setup response does not establish that a reset succeeded. For a reset
returning `{"ok":true}`, use a named setup step with an explicit response check:

```yaml
setup:
  - name: Reset counter
    request:
      method: POST
      path: /testing/reset
    assert:
      name: reset must be acknowledged
      json_boolean_path: [ok]
      equals: true
```

The check runs after the response has been fully read and closed, before the next
setup step. A response containing `{"ok":false}` stops the trial before remaining
setup, the precondition, baseline capture, or operations. Use the named list form
even for a single checked step; the legacy request mapping remains unchanged.

To verify initial state after all setup steps, add one optional `precondition`:

```yaml
precondition:
  request:
    method: GET
    path: /state
  assert:
    name: counter must start empty
    json_integer_path: [count]
    equals: 0
```

This request is independent of the final `observation`. It works with state or
history invariants, including history checks without a final observation, and
can be used without setup. Every ordinary trial and every reduction trial repeats
its own checks. The request must complete cleanly with a 2xx response.

Both locations accept one named `assert` with exactly one of:

- `json_boolean_path` with a literal Boolean `equals: true` or `equals: false`.
- `json_integer_path` with the existing `minimum`, `maximum`, bounds pair, or
  exclusive `equals` constraint. Values remain exact signed 64-bit integers.

Names and paths must be non-empty. Object keys and zero-based array indexes use
the same path syntax as invariants. Quoted Boolean/numeric strings are not coerced.
Assertions do not support `change`, status lists, expressions, or string matching.
Definitions are validated before setup can send any requests.

A mismatched check is a trial error with code `precondition_failed` and exit 2
when no other trial demonstrated a violation. Missing paths, nulls, wrong types,
malformed or trailing JSON, and truncated bodies also prevent verification and
stop that trial. They do not become invariant violations. HTTP execution errors
retain their causes and cancellation identity. Ordinary failed checks allow the
next trial to restart all preparation; parent cancellation stops new trials.

Reports retain each attempted request and its assertion result, including false
and zero observations. A null observed value means the scalar could not be
verified; it is not a successful match. JSON includes `scenario.precondition`
and each trial's `evidence.precondition`, both null when absent or not reached.
Setup assertion definitions appear in `scenario.setup[].assertion`; evaluations
appear in `evidence.setup[].assertion`. Text identifies failed and unverified
checks; verbose text also expands successful setup checks. Selected and interrupted
reduction trials retain the same evidence, while rejected candidates keep their
existing bounded summaries.

Checks are opt-in and only establish the declared scalar values at the moment
they were read. A truthful reset acknowledgement does not prove the reset covered
all relevant state. A fresh scalar value does not drain pending work, isolate
external writes, or prevent state changing after the check. These immediate
checks do not poll or roll back state. Use the bounded waits below when the
application exposes a trustworthy completion signal. When both a precondition
and a change invariant use the same URL, the change baseline remains a separate
request after precondition verification.

## Wait for readiness and deferred effects

An accepted HTTP response can precede the actual effect. For queued or delayed
work, declare a completion condition before evaluating final state. If reset can
overlap work left from an earlier run, also declare readiness before setup:

```yaml
readiness:
  request: {method: GET, path: /testing/work-state}
  assert:
    name: no pending work before reset
    json_integer_path: [pending]
    equals: 0
  timeout: 2s
  interval: 25ms
  max_polls: 100

completion:
  request: {method: GET, path: /testing/work-state}
  assert:
    name: all accepted work has finished
    json_integer_path: [pending]
    equals: 0
  timeout: 2s
  interval: 25ms
  max_polls: 100
```

The example requires an application endpoint with a truthful pending-work count
covering all admitted work. Reset must not erase that accounting while work can
still commit. A terminal job-status Boolean can also use `json_boolean_path` and
`equals: true`. Waits use the same named Boolean/integer assertions as preparation
checks; they do not test the post-operation invariant repeatedly until it passes.

Both fields are optional. `request`, `assert`, a positive `timeout`, and a positive
`interval` are required when a wait is declared. `max_polls` defaults to 100 and
must be between 1 and 100. The first poll runs immediately. Only a complete 2xx
response with a valid, nonmatching scalar is retried after the interval. The next
poll starts after the previous body has closed and the interval has elapsed.
The condition must match before either the deadline or poll limit is reached.
HTTP failures, implicit transport retries, malformed/missing values, and truncated
responses stop verification rather than being silently retried.

The deadline covers the entire wait, including requests, response bodies, and
intervals. The usual per-request timeout still applies if it is shorter. Parent
cancellation interrupts requests and interval waits. No extra polling worker runs
in parallel with the operation workers or setup.

The execution order is:

```text
readiness → setup → precondition → change baseline → operations
          → completion → final observation and invariant evaluation
```

Readiness therefore protects the reset itself. Completion runs after operation
workers finish, including when their outcomes are uncertain, and also works with
history invariants without a final observer. Every ordinary trial and reduction
candidate repeats the declared waits. No wait runs unless it is configured.

An unverified wait records `readiness_failed` or `completion_failed` and stops
later trials and candidates, even though ordinary trial errors normally allow
another trial. Final state is not observed or evaluated after failed completion.
The active trial retains its error and all poll evidence. A response-count
violation already demonstrated by completed responses remains a violation; a
previously demonstrated violation also retains aggregate priority. Otherwise the
run is errored (exit 2). Parent cancellation retains its existing interrupted-run
behavior. The Go trial API retains a non-cancellation wait failure in `Trial.Err`
and returns the stopped result; inspect its status, recorded count, and `WaitFailed()`.

Reduction reports `stopped`, retains the stopped candidate's trials when present,
and selects no reproduction from an unsettled search. A known baseline violation
remains visible; stopping reduction does not turn it into a pass or claim a
smaller result. New candidates cannot start after a failed wait.

JSON includes nullable `scenario.readiness`/`scenario.completion` definitions and
matching per-trial evidence with ordered, numbered polls, timing, scalar results,
and errors. All polls are retained, up to the declared limit. Default text shows
the last relevant poll and how many earlier polls were omitted; verbose text
expands every poll. Polls retain safe request metadata and at most 512 response
bytes each. Assertions use the full response before trimming, subject to the
usual 1 MiB complete-body limit. A truncated excerpt can therefore accompany a
verified scalar result. Request bodies and headers are not duplicated into poll
evidence.

These waits enforce the declared application's completion contract. A fixed
sleep, an unchanged value, or a momentary zero counter alone cannot prove that no
later write will occur. The condition must account for delayed work, including
processing that continues after a client timeout. If it cannot, use stronger
application-side accounting, a disposable namespace, or generation fencing;
ConcurTest does not create those guarantees. Waits do not cancel target work,
perform rollback, or establish isolation from unrelated writers.

## JSON integer constraints

Nested responses use one entry for each object key or array index, such as
`json_integer_path: [data, quantity]`. Array indexes start at zero:

```yaml
json_integer_path: [data, Products, 0, BasketItem, quantity]
```

This selects `quantity` inside the first product's `BasketItem`. Indexes may be
unquoted integers or quoted decimal strings such as `'0'`. When traversing an
object, entries match literal keys, including numeric keys. When traversing an
array, entries must be non-negative decimal indexes without leading zeros.
Missing keys, out-of-range indexes, and non-integer values produce an evaluation
error. Reports retain path entries as strings, including indexes.

Use `json_integer_path` to check an observed integer. Bounds are inclusive:

| Fields | Required value |
| --- | --- |
| `minimum: 0` | value >= 0 |
| `maximum: 999` | value <= 999 |
| `minimum: 10` and `maximum: 20` | 10 <= value <= 20 |
| `equals: 999` | value == 999 |
| `change: -1` | final value - baseline value == -1 |

With stock reset to 1,000, a checkout that decrements it once should leave 999:

```yaml
invariant:
  name: basket checkout must decrement stock exactly once
  json_integer_path: [data, quantity]
  equals: 999
```

An observation request is required for these checks. Define at least one of
`minimum`, `maximum`, `equals`, or `change`. Do not combine `equals` with either
bound, and keep `minimum` less than or equal to `maximum`. All constraint values must
be integers representable as signed 64-bit values; zero and negative values are
valid. Each scenario declares exactly one invariant.

In JSON reports, these checks use type `json_integer`; their definition includes
only the configured constraints. The evaluation records `observed` and `violated`.

To check a decrement without assuming the starting stock:

```yaml
invariant:
  name: stock must decrease exactly once
  json_integer_path: [data, quantity]
  change: -1
```

With `change`, ConcurTest sends the configured observation request after setup
and before operations, then again after all operations complete. It checks
`final - baseline == change`. Each trial, including each reduction trial, reads
its own baseline. `change` must appear alone; it cannot be combined with
`minimum`, `maximum`, or `equals`. Positive changes and zero are valid.

A failed baseline stops the trial before operations run. Invalid, missing, or
truncated observations produce an error. Values and their difference must fit
in a signed 64-bit integer; an out-of-range difference produces an error instead
of wrapping. This checks the net change, so unrelated writes or offsetting
changes can affect the result.

Text reports show the baseline, final value, and observed change. JSON trial
evidence includes `baseline_observation` (null for other checks); change
evaluations add `baseline` and `change`.

## History-based invariants

A history invariant evaluates recorded operation responses. The regression
history scenario limits accepted purchases to one:

```yaml
invariant:
  name: accepted purchases must not exceed stock
  maximum_successful_attempts: 1
  successful_status_codes: [201]
```

If `successful_status_codes` is omitted, every HTTP status from `200` through
`299` counts as success. Reports identify qualifying attempt IDs and those beyond
the configured maximum. An optional observation provides context without
replacing the history check. Successful HTTP responses are evidence for the
declared contract; they do not automatically establish a business outcome such
as a paid order.

## Failure reduction

Enable reduction with `execution.reduce: true`. Reduction requires setup,
at least three trials, and at least two attempts and two concurrent requests.
It starts only after a majority of the original trials demonstrate the violation,
none are errored or inconclusive, and every trial meets its operation outcome
requirements. Candidates must meet the same requirements. Candidates with fewer
attempts than the minimum accepted-response requirement are not tested. Rejected
candidate summaries retain `outcome_requirements_met`, even when their full
trial evidence is not retained. This does not yet preserve the original failure's
identity or direction.

Unverified readiness or completion stops the whole search with status `stopped`;
the active candidate is retained and later candidates are not run.

The search varies attempts and concurrency, trying smaller attempt counts first,
with concurrency of at least two. Each candidate runs the configured number of
trials and repeats all setup steps. The search is bounded to 100 candidates.
The selected result is the smallest observed failure, not a guarantee that no
smaller failure exists or a statistical confidence estimate.

Reports include a reproduction command with execution overrides and reduction
disabled:

```bash
concurtest run --attempts 2 --concurrency 2 --no-reduce examples/vulnerable-inventory/observation-scenario.yaml
```

## Reports

The default text report shows one smallest observed failure, at most four
relevant attempts, and one example of each problem status. Response excerpts
are limited to 160 bytes. Use `--verbose` to expand every retained trial,
including passing trials and retained reduction evidence; verbose excerpts
retain up to 512 bytes:

```bash
concurtest run --verbose examples/vulnerable-inventory/observation-scenario.yaml
```

Terminal color is selected automatically. Redirected or piped output stays
plain by default, and a non-empty `NO_COLOR` disables automatic color. Use
`--color always` or `--color never` to choose explicitly.

### JSON reports

Text remains the default. Select the versioned JSON report explicitly when a
CI job or another tool needs structured results:

```bash
concurtest run --format json examples/vulnerable-inventory/observation-scenario.yaml
```

The JSON document includes scenario metadata, aggregate counts, every ordered
trial with complete evidence, invariant results, retained reduction evidence,
structured errors, nanosecond timing, and an argument array for reproduction.
Passing trials are complete in JSON even though the text report summarizes
them.

Text-only options such as `--verbose` and `--color` cannot be combined with
`--format json`; JSON output and its versioned schema are unchanged by terminal
presentation settings.

The contract is defined by the checked-in
[report schema](../schemas/report-v1.schema.json). Reports use schema version `1.0.0`.

Response excerpts retain at most 512 bytes. Valid UTF-8 is emitted as text and
other bytes are base64 encoded. Reports never include request bodies or HTTP
headers. Command and scenario-loading failures also produce a JSON error
document when `--format json` is selected.

Exit codes do not depend on report format:

- `0` means every trial passed.
- `1` means at least one trial demonstrated an invariant violation.
- `2` means no violation was demonstrated and the run was inconclusive,
  errored, or interrupted.
