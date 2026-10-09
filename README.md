# ConcurTest

ConcurTest tests correctness when state-changing requests run concurrently. It
records what happened, checks declared invariants, and searches for a smaller
observed reproduction of a failure. It interacts with applications through
HTTP, regardless of their language or framework.

The current v0 supports sequential reproducibility trials. A trial can reset
the target, repeat one HTTP operation with bounded concurrency, and evaluate
one invariant. An invariant can check a JSON integer at a chosen path
after the operations or limit how many operation responses have successful
HTTP statuses. An opt-in reduction pass can test smaller concurrent execution
settings after a failure reproduces across a clean majority of trials.

> [!WARNING]
> ConcurTest intentionally sends concurrent requests that may change or damage
> target data. Use the included example only on your local machine, and do not
> point a scenario at live or valuable data.

## Requirements

- Go 1.27 or newer

## Run the reproducible regression demo

The included Go inventory service starts with one item and deliberately handles
two simultaneous purchases incorrectly. It requires no credentials or external
services.

Start it in one terminal:

```bash
go run ./examples/vulnerable-inventory
```

It listens only on `127.0.0.1:8080`.

In a second terminal, run either checked-in scenario. Run them sequentially
because both reset and modify the same inventory:

```bash
go run ./cmd/concurtest run examples/vulnerable-inventory/observation-scenario.yaml
go run ./cmd/concurtest run examples/vulnerable-inventory/history-scenario.yaml
```

| Scenario | Check | Expected failure |
| --- | --- | --- |
| [Observation](examples/vulnerable-inventory/observation-scenario.yaml) | Stock stays non-negative | Final stock is `-1` |
| [History](examples/vulnerable-inventory/history-scenario.yaml) | At most one purchase returns HTTP 201 | Two purchases succeed, even when availability reports `0` |

Both scenarios reset stock to one before every trial, including reduction trials.
See the [demo guide](examples/vulnerable-inventory/README.md) for the routes and
repeatable failure mechanism.

The observation scenario selects the observed value with an explicit list of
object keys:

```yaml
invariant:
  name: final stock must be non-negative
  json_integer_path: [stock]
  minimum: 0
```

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

The checked-in scenario starts with four attempts at concurrency four and runs
10 trials. Each trial resets the inventory and purchase coordination. After the
failure reproduces, ConcurTest tests smaller settings and selects two attempts
at concurrency two. The report includes:

```text
ConcurTest · inventory oversell

VIOLATED
10/10 trials demonstrated the violation.

Invariant
  final stock must be non-negative
  Expected        $["stock"] >= 0
  Observed        $["stock"] = -1

Reduction
  Status          REDUCED
  Attempts        2
  Concurrency     2
  Violations      10/10 trials
  Note            Smallest observed failure; a smaller one may still exist.
```

The command exits with code `1`. That non-zero result is expected in this
demonstration: ConcurTest found the intended correctness failure.

The report describes the smallest failing configuration ConcurTest observed.
It does not claim that the result is mathematically minimal. Its reproduction
command uses execution overrides and disables another reduction pass:

```text
concurtest run --attempts 2 --concurrency 2 --no-reduce examples/vulnerable-inventory/observation-scenario.yaml
```

The default text report shows one smallest observed failure, at most four
relevant attempts, and one example of each problem status. Response excerpts
are limited to 160 bytes. Use `--verbose` to expand every retained trial,
including passing trials and retained reduction evidence; verbose excerpts
retain up to 512 bytes:

```bash
go run ./cmd/concurtest run --verbose examples/vulnerable-inventory/observation-scenario.yaml
```

Terminal color is selected automatically. Redirected or piped output stays
plain by default, and a non-empty `NO_COLOR` disables automatic color. Use
`--color always` or `--color never` to choose explicitly.

## Use JSON reports in CI

Text remains the default. Select the versioned JSON report explicitly when a
CI job or another tool needs structured results:

```bash
go run ./cmd/concurtest run --format json examples/vulnerable-inventory/observation-scenario.yaml
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
[report schema](schemas/report-v1.schema.json). Reports currently use schema
version `1.0.0`, which may evolve before launch. Objects are closed; after launch,
adding, removing, or changing an emitted property requires a new major schema
version.

Response excerpts retain at most 512 bytes. Valid UTF-8 is emitted as text and
other bytes are base64 encoded. Reports never include request bodies or HTTP
headers. Command and scenario-loading failures also produce a JSON error
document when `--format json` is selected.

Exit codes do not depend on report format:

- `0` means every trial passed.
- `1` means at least one trial demonstrated an invariant violation.
- `2` means no violation was demonstrated and the run was inconclusive,
  errored, or interrupted.

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

Use `json_integer_path` to check an observed integer. Bounds are inclusive:

| Fields | Required value |
| --- | --- |
| `minimum: 0` | value >= 0 |
| `maximum: 999` | value <= 999 |
| `minimum: 10` and `maximum: 20` | 10 <= value <= 20 |
| `equals: 999` | value == 999 |
| `change: -1` | final value - baseline value == -1 |

For a checkout that must decrement stock exactly once:

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
valid. Each scenario still declares exactly one invariant.

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

## Why the example fails

Each purchase checks that stock is available while holding a mutex. It then
releases the mutex before decrementing the stock. The two requests can
therefore both observe stock `1`, both decide that a purchase is valid, and
then decrement separately until stock becomes `-1`.

The mutex prevents a Go data race, but it does not make the full business
operation atomic. This distinction is central to ConcurTest: code can be
race-detector clean and still be incorrect under concurrency.

The example uses a two-request rendezvous to make this broken ordering
repeatable within each trial. ConcurTest has no knowledge of that coordination;
it interacts only through the HTTP requests declared in the
[state scenario](examples/vulnerable-inventory/observation-scenario.yaml) or
[history scenario](examples/vulnerable-inventory/history-scenario.yaml).

## Juice Shop case study

The [Juice Shop examples](examples/juice-shop/README.md) exercise duplicate
checkout in an external application:

- [History scenario](examples/juice-shop/history-scenario.yaml): at most one
  checkout attempt returns HTTP 200.
- [Observation scenario](examples/juice-shop/stock-scenario.yaml): checkout
  decreases inventory by exactly one unit.

These templates require customer and accounting credentials, local inventory
access, and ordered basket and stock preparation. The case study guide documents
those requirements and the side effects that setup leaves behind. Credential
placeholders must be replaced in ignored local copies before running.

The following excerpt comes from a previously captured local history run. It
shows baseline invariant evidence and the reduced settings; it is not a new run
of the templates:

```text
ConcurTest · juice shop duplicate checkout

VIOLATED
10 of 10 completed trials demonstrated the violation.

Invariant
  basket must only be checked out once
  Expected        At most 1 successful attempt
  Observed        4 successful attempts

Reduction
  Status          REDUCED
  Smallest observed failure
    Attempts      2
    Concurrency   2
    Violations    10 of 10 trials
```

This demonstrates multiple successful checkout responses under the declared
at-most-once contract. The stock check measures a different property and may
pass when lost updates hide duplicate effects. Neither result alone proves the
contents or payment status of generated orders.

## Development checks

```bash
go test ./...
go test -race ./...
go vet ./...
```

See the [architecture](docs/architecture.md) for the project boundaries and
design direction. ConcurTest is available under the
[Apache License 2.0](LICENSE).
