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

ConcurTest executes the operations, records what occurred, observes the resulting state, evaluates the declared invariants, and reports violations.

Conceptually:

```text
Scenario
    ↓
Operations
    ↓
Adversarial execution
    ↓
Execution history
    ↓
State observation
    ↓
Invariant evaluation
    ↓
Failure reproduction/report
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

Execution history should be structured data, not only formatted terminal output.

### Observation

An observation reads externally visible state from the target system.

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
