# ConcurTest Architecture

## Purpose

ConcurTest is an open-source adversarial correctness testing engine for stateful backend systems.

It helps developers discover bugs that may not appear during normal unit, integration, or API testing but emerge when operations happen concurrently, are retried, duplicated, reordered, interrupted, or partially fail.

The system being tested may be written in any programming language or framework.

ConcurTest should interact with applications primarily through observable external interfaces.

## Core idea

A developer defines:

1. how to prepare a test scenario;
2. which operations can be performed;
3. how those operations should be exercised adversarially;
4. which properties must remain true.

ConcurTest executes the operations, records what occurred, evaluates the
declared invariant from history or observed state, and reports violations.

The selected invariant determines which evaluation path is used:

```text
Scenario
    ↓
Operations
    ↓
Adversarial execution
    ↓
Execution history
    ├──→ History invariant evaluation ───┐
    │                                    ├──→ Failure reproduction/report
    └──→ State observation               │
              ↓                          │
         State invariant evaluation ─────┘
```

## Example

Suppose an inventory system contains one remaining product.

A normal test may perform purchases sequentially and pass.

ConcurTest may issue many purchase operations concurrently.

The developer can express properties such as:

```text
stock >= 0
successful purchases <= initial stock
```

If two purchases succeed against one available item, ConcurTest should identify and report the violation.

## Core concepts

### Scenario

A scenario describes the environment and behaviour to test.

It may define:

- target application
- setup operations
- test operations
- execution strategy
- observations
- invariants

### Operation

An operation is an action performed against the target system.

Initially operations are HTTP requests.

An operation should capture enough information for ConcurTest to record:

- start time
- completion time
- request
- response
- errors
- execution identity

### Execution strategy

The execution strategy determines how operations are exercised.

Initial strategies should focus on:

- concurrent execution
- repeated execution
- duplicate execution
- retries

Future strategies may include:

- delayed requests
- dropped responses
- reordered operations
- service/network faults

The architecture must not require those future capabilities to exist immediately.

### Reproducibility trials

A scenario may request a bounded number of independent trials. Trials execute
sequentially so setup can establish a clean state before every concurrent
operation group. Concurrency remains bounded within a trial; the trial
orchestrator itself does not add goroutines.

Each trial retains its complete structured run evidence and is classified as
passed, violated, inconclusive, or errored. Ordinary trial errors are recorded
and later trials still run. Parent-context cancellation preserves the active
partial trial, stops new trials from starting, and makes the overall sequence
incomplete.

Aggregate classification gives demonstrated invariant violations precedence
over trial errors, then inconclusive results, then passes. This keeps a proven
correctness failure visible even when another trial could not be evaluated.

### Observed failure reduction

Reduction is an explicit scenario option because it can send substantially more
traffic. It first requires the configured baseline to demonstrate violations in
a strict majority of at least three clean trials. Errored or inconclusive
trials prevent reduction from starting.

Candidates vary only attempt count and concurrency. They run sequentially in a
stable attempts-first order, use the baseline trial count, and reset state
before every trial through the existing complete scenario runner. Candidate
concurrency starts at two so reduction remains focused on concurrent failures.

The search stops at the first qualifying candidate or after 100 candidates.
Rejected candidates retain bounded status summaries; the baseline, selected
candidate, and any interrupted active candidate retain complete evidence.
Reports call the result the smallest observed failure and do not claim
mathematical minimality or statistical confidence.

### Execution history

ConcurTest must maintain an accurate record of what occurred during a run.

An execution history may contain:

- operation identifier
- start timestamp
- completion timestamp
- inputs
- outputs
- failures
- ordering information
- relevant metadata

Trial sequences additionally retain their requested count, stable one-based
trial ordering, aggregate timing and status, and every trial's complete run
history.

Execution history should be structured data, not only formatted terminal output.

History-aware invariants evaluate recorded operation outcomes directly. The
initial history invariant limits the number of attempts whose final HTTP status
matches an explicit list, or the default 2xx range. It retains every qualifying
attempt ID and the stable suffix beyond the configured maximum. Missing or
failed executions do not count as successful; they keep a non-violating trial
from being reported as a trustworthy pass.

### Observation

An observation reads externally visible state from the target system. It is
required when the configured invariant evaluates state and optional when the
invariant evaluates execution history.

Initially this may be another HTTP request.

Future observers may include databases, queues, caches, or custom plugins.

### Invariant

An invariant describes something that must remain true.

Examples include:

```text
stock >= 0
wallet balances never become negative
one idempotency key produces at most one logical operation
one seat cannot have multiple confirmed owners
```

Invariant evaluation should be separate from transport execution.

A scenario continues to declare exactly one invariant. The current concrete
forms are a JSON integer minimum at a configured object path and a maximum
number of successful HTTP attempts. They are represented explicitly rather
than through an expression language or plugin system.

### Failure

A failure represents a demonstrated violation, not merely an HTTP error.

ConcurTest should distinguish between:

- transport failures;
- operation failures expected by the target system;
- invariant violations;
- ConcurTest internal failures.

### Reproduction

When a violation is discovered, the long-term goal is to reduce the execution to the smallest useful reproduction.

Reports should answer:

- what invariant failed;
- which operations contributed;
- what order/timing occurred;
- what state was observed;
- how the developer can reproduce it.

## Architecture boundaries

The project should keep these concerns separate:

```text
configuration
execution
transport
history
observation
invariants
reduction
reporting
```

They do not necessarily need to become separate Go packages immediately.

Package boundaries should emerge when there is a real separation of responsibility.

Do not create interfaces or plugin systems before multiple implementations actually require them.

## Initial transport

HTTP/JSON is the first supported application interface.

The core model should not depend directly on a particular application framework such as:

- Laravel
- NestJS
- Django
- Rails
- Spring
- Gin

The target application is a black box from ConcurTest's perspective.

## Configuration

Human-readable configuration is expected to be YAML.

Configuration should be:

- explicit
- versionable
- suitable for CI
- easy to review
- deterministic where possible

Configuration parsing should be separate from execution logic.

## CLI

The primary interface is the `concurtest` command.

A typical invocation should eventually feel roughly like:

```bash
concurtest run scenario.yaml
```

The CLI should remain thin.

Business logic belongs in reusable internal packages rather than directly inside CLI commands.

## Reporting contract

The CLI defaults to a human-readable text report and provides one explicit JSON
format for automation. Both formats consume the same structured engine and
reduction results and preserve the same exit status meanings: pass, demonstrated
violation, or incomplete/untrustworthy execution.

JSON has two discriminated top-level forms: a run report and an early error
report. Run reports contain scenario metadata, aggregate counts, every trial in
stable one-based order, complete run and invariant evidence, reduction
summaries and retained candidate trials, errors, timing, and reproduction
arguments. Early error reports cover command and scenario-loading failures for
which no trustworthy trial result exists.

Errors cross package boundaries as a small typed tree with a stable code,
human-readable message, and ordered causes. Standard context cancellation and
deadline errors retain their Go identity while receiving distinct report codes.
This keeps text useful to people without requiring CI consumers to parse error
strings.

The checked-in JSON Schema uses semantic versions and closed objects. The
initial contract is `1.0.0`; changing emitted fields or their shapes requires a
new major version. Schema validation is a test concern, not a runtime dependency
of the reporter.

Report evidence stays bounded and safe. Response excerpts retain at most 512
bytes and identify UTF-8 or base64 encoding plus truncation. HTTP headers and
request bodies are never emitted. The top-level baseline is not duplicated
inside reduction output; rejected candidates retain summaries, while selected
or interrupted candidates may retain their complete ordered trials.

The default text presentation is optimized for terminal scanning. It expands
one representative for each semantically distinct violation, always expands
errored and inconclusive trials, and summarizes equivalent violations by their
stable trial numbers. Equivalence ignores timing jitter but includes invariant
values, stage and attempt presence, stable attempt identities, errors, HTTP
statuses, and the same bounded response excerpts shown to the user. Verbose
text expands every retained trial and retained reduction candidate evidence.

Terminal color is a CLI presentation decision rather than engine state. Auto
mode requires a terminal, respects a non-empty `NO_COLOR`, and emits no ANSI
escapes when output is redirected. Explicit always and never modes override
automatic detection. JSON never receives terminal presentation options and its
versioned contract is unchanged.

Text reproduction commands use POSIX shell quoting and remain free of color
escapes. Presentation flags are intentionally omitted so the command describes
the same scenario and reduced execution configuration without changing
execution or the JSON contract.

## Performance

ConcurTest should efficiently run many simultaneous network operations, but raw throughput is not the primary product goal.

Correct execution histories and trustworthy results are more important than artificially high request-per-second benchmarks.

Concurrency should be bounded and configurable.

## Safety

ConcurTest performs intentionally disruptive testing.

The tool should make it difficult for a developer to accidentally interpret it as safe production traffic.

Potentially destructive capabilities should be explicit.

Never assume a target is disposable.

## Engineering direction

Prefer:

- small composable components;
- deterministic behaviour where possible;
- structured results;
- explicit ownership;
- bounded concurrency;
- good error messages;
- reproducible tests.

Avoid:

- speculative abstraction;
- premature distributed execution;
- unnecessary dependencies;
- coupling to one framework;
- coupling to one cloud;
- adding AI where deterministic logic is sufficient.

## Success criterion

ConcurTest becomes useful when it can discover a correctness bug that ordinary application tests failed to expose, explain the violated invariant clearly, and give the developer enough information to reproduce and fix the bug.
