# Scenario reference

Scenarios declare preparation requests, one concurrent operation, and one
invariant. The [inventory observation scenario](../examples/vulnerable-inventory/observation-scenario.yaml)
is a complete runnable example. Start with the [quickstart](../README.md#quickstart--reproduce-a-race-condition).

All commands below run from the repository root after installing ConcurTest.

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
after all setup steps complete. Only the test operation runs concurrently.

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
array of `{name, request}` objects, and each trial's `evidence.setup` is an ordered
array of `{name, execution}` objects for attempted steps. An empty array (`[]`)
represents no steps. A single-request mapping becomes one step with an empty
name.

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
It starts only after a majority of the original trials demonstrate the violation
and none are errored or inconclusive.

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
