# ConcurTest

Test whether concurrent API requests violate application correctness rules.

## What is ConcurTest?

ConcurTest is a Go command-line tool that sends concurrent HTTP requests and
checks a declared correctness rule. YAML scenarios define the requests and the
invariant. Reports show the evidence and a command to reproduce a failure.

Features include:

- Bounded concurrency and repeated trials.
- Ordered setup requests before every trial.
- Checks against observed JSON values or recorded HTTP responses.
- Failure reduction, text reports, and structured JSON reports.

## Why does it exist?

Requests can succeed individually while producing an incorrect result together:
oversold inventory, duplicate checkout, or repeated state changes. These failures
can occur even when the application has no data races. ConcurTest tests the
application's behavior through its HTTP interface.

## Installation

Requires Go 1.27 or newer. Install from source:

```bash
git clone https://github.com/eumarumar/concurtest.git
cd concurtest
go install ./cmd/concurtest
```

Add Go's executable directory (`go env GOBIN`, or `$(go env GOPATH)/bin` when
`GOBIN` is unset) to `PATH`. Run the following examples from the repository root.

## Quickstart — reproduce a race condition

The included inventory service has a repeatable overselling bug. It requires
no credentials or external services.

Start it in one terminal:

```bash
go run ./examples/vulnerable-inventory
```

It listens on `127.0.0.1:8080`. In a second terminal:

```bash
concurtest run examples/vulnerable-inventory/observation-scenario.yaml
```

The scenario resets stock to one before each trial, then sends four purchase
requests at concurrency four. Two purchases succeed, leaving stock at `-1`.
Expected output, excerpted:

```text
ConcurTest · inventory oversell

VIOLATED
10/10 trials demonstrated the violation.

Invariant
  final stock must be non-negative
  Expected        $["stock"] >= 0
  Observed        $["stock"] = -1
```

Reduction finds the same failure with two attempts at concurrency two.
Exit code `1` is expected: the scenario demonstrated the intended bug.
Use disposable local targets; the requests change application state.

## Supported invariants

Each scenario declares one invariant:

| Check | YAML fields |
| --- | --- |
| Inclusive integer bounds | `json_integer_path` with `minimum`, `maximum`, or both |
| Exact integer value | `json_integer_path` with `equals` |
| Change from the trial's baseline | `json_integer_path` with `change` |
| Maximum successful operation responses | `maximum_successful_attempts`, optional `successful_status_codes` |

Integer checks require an observation request. History checks count qualifying
HTTP responses; success defaults to any 2xx status. See the
[scenario reference](docs/scenarios.md) for syntax and evaluation rules.

## Examples — Regression and Juice Shop

| Example | Scenarios | Preparation |
| --- | --- | --- |
| [Inventory regression demo](examples/vulnerable-inventory/README.md) | [Observation](examples/vulnerable-inventory/observation-scenario.yaml), [history](examples/vulnerable-inventory/history-scenario.yaml) | Local Go service; no credentials |
| [Juice Shop case study](examples/juice-shop/README.md) | [Stock observation](examples/juice-shop/stock-scenario.yaml), [checkout history](examples/juice-shop/history-scenario.yaml) | Local Juice Shop, customer and accounting credentials, inventory access |

Run scenarios against a shared target sequentially. The Juice Shop guide covers
setup requirements and includes evidence from a previously captured run.

## Ordered setup and trial state

`setup` accepts one request or a list of named requests. Steps run sequentially
in YAML order before every trial, including reduction trials. Every step must
return 2xx; a failed step stops that trial before test operations run.

Setup must establish the preconditions relevant to the invariant. Completed
steps are not rolled back. A fresh baseline measures change within a trial;
it does not remove side effects from previous trials. See
[setup syntax and behavior](docs/scenarios.md#prepare-state-before-each-trial).

## Failure reduction and reproduction

Set `execution.reduce: true` to search smaller attempt and concurrency settings.
Reduction requires setup and at least three trials. It runs after a majority
of trials violate the invariant, with no errored or inconclusive trials.
The result is the smallest observed failure; smaller failures may still exist.

Reports include a reproduction command, for example:

```bash
concurtest run --attempts 2 --concurrency 2 --no-reduce examples/vulnerable-inventory/observation-scenario.yaml
```

Use `--verbose` for retained trial evidence. See the
[reduction reference](docs/scenarios.md#failure-reduction) for search limits.

## CLI exit codes and CI usage

| Exit code | Meaning |
| --- | --- |
| `0` | Every trial passed |
| `1` | At least one trial demonstrated a violation |
| `2` | No violation demonstrated; the run errored, was inconclusive, or was interrupted |

Use the installed binary in CI and select JSON for structured evidence:

```bash
concurtest run --format json examples/vulnerable-inventory/observation-scenario.yaml > report.json
```

Report format does not change exit codes. The
[JSON schema](schemas/report-v1.schema.json) defines the report contract.

## Limitations, license, and contributing

- HTTP/JSON only; one repeated operation and one invariant per scenario.
- Requests are static; YAML values do not expand environment variables or capture
  values from earlier responses.
- Setup controls trial state; automatic rollback and resource isolation are not provided.
- Passing trials do not prove correctness. Net stock changes can hide duplicate
  effects, and successful HTTP responses alone do not prove business outcomes.

Licensed under [Apache 2.0](LICENSE). For contributions, read the
[architecture](docs/architecture.md), keep changes focused, and run:

```bash
go test ./...
go test -race ./...
go vet ./...
```
