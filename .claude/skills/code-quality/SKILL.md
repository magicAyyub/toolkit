---
name: code-quality
description: "Apply when writing, modifying, refactoring, or reviewing code. Focuses on code quality, readability, structure, and natural human-authored style."
---

# Code Quality and Style

Write code that is comfortable for an experienced developer to read and maintain.

The goal is not to produce the shortest code, the most abstract code, or code that demonstrates every best practice.

The goal is code that makes the underlying behavior clear.

## Readability

Prefer code that can be understood by reading it naturally from top to bottom.

Use:

- clear names;
- explicit control flow;
- familiar language features;
- meaningful local structure;
- straightforward data flow;
- abstractions that represent real concepts.

Avoid making the reader reconstruct simple behavior across unnecessary layers.

Code should communicate its intent through its structure.

## Do not optimize for short code

Shorter is not automatically better.

Do not compress logic merely to reduce line count.

A longer implementation is preferable when it makes behavior easier to understand.

Likewise, do not split code into many small functions or files simply because small functions or files are considered "clean."

Split code when the resulting separation represents a meaningful concept or makes the code materially easier to understand.

A substantial file is acceptable when the problem itself is substantial.

## Prefer explicitness over cleverness

When two implementations are similarly good, prefer the one that is easier to understand without already knowing the author's intention.

Avoid:

- clever one-liners;
- unnecessary chaining;
- obscure language features;
- implicit behavior that is difficult to discover;
- abstractions whose main purpose is to hide a few lines of simple logic.

Do not avoid concise language features when they genuinely improve readability.

The question is whether the code becomes clearer, not whether it becomes shorter.

## Abstraction should earn its place

Introduce an abstraction when it represents a real concept or solves a real maintenance problem.

Do not introduce abstractions merely because code is duplicated once or twice.

Avoid creating:

- generic wrappers around simple operations;
- helpers that only rename an existing operation;
- layers that forward calls without adding meaning;
- interfaces with only one meaningful implementation;
- utilities created for hypothetical future reuse;
- configuration systems for behavior that has only one value.

Duplication is sometimes easier to understand than premature abstraction.

When duplication becomes meaningful, then abstract it.

## Use the existing language well

Prefer the idioms of the language and framework already used by the project.

Do not fight the language to enforce a personal abstraction style.

Do not introduce a pattern simply because it is common in another ecosystem.

Use the simplest idiomatic construct that expresses the behavior clearly.

## Preserve useful local context

Prefer keeping related behavior close together when that makes the code easier to understand.

Do not extract code solely because a function is getting "too long."

Do not move logic into another file if doing so makes the reader jump around the project to understand a simple operation.

A useful piece of code should be easy to find from the place where it is used.

## Functions

A function should have a reasonably clear responsibility.

This does not mean every function must be tiny.

A function can contain several related steps when those steps form one understandable operation.

Extract a function when the extraction gives the reader a useful name or creates a meaningful boundary.

Avoid extracting functions whose names merely restate the next line of code.

Prefer:

```python
user = create_user(data)
```

when `create_user` represents a meaningful operation.

Avoid:

```python
user = build_user_from_data(data)
```

when the function only wraps a few obvious assignments and provides no meaningful abstraction.

## Error handling

Handle errors where there is enough context to make a meaningful decision.

Do not add defensive checks everywhere merely because something could theoretically fail.

At the same time, do not rely on assumptions that the code cannot safely make.

Error handling should reflect real failure modes.

Avoid:

- catching broad exceptions without a reason;
- silently ignoring failures;
- returning ambiguous fallback values;
- defensive branches that can never occur under the actual contract.

Make important failures visible.

## Comments

Comments should explain something that the code itself does not make sufficiently clear.

Good comments explain:

- why an unusual decision exists;
- an important constraint;
- a non-obvious trade-off;
- an external limitation;
- an invariant;
- why an apparently simpler implementation would be incorrect.

Avoid comments that narrate the code.

Do not write:

```python
# Get the user
user = get_user(user_id)
```

The code already says this.

Prefer:

```python
# Keep the lookup here because the authorization check depends on
# the user's organization membership.
user = get_user(user_id)
```

when that reasoning is not otherwise obvious.

Comments should be concise and specific.

## Code organization

Use structure to help the reader navigate substantial code.

Section comments can be useful when a file contains several meaningful areas of responsibility.

For example:

```python
# Authentication

...

# Session management

...

# Account recovery

...
```

This is useful when it genuinely helps navigation.

Avoid decorative section banners and visual noise such as:

```text
========================
===== AUTHENTICATION ===
========================
```

Do not add symbols, separators, or headings merely to make generated code look polished.

## Documentation

Docstrings and API documentation should be proportional to the code they describe.

Document behavior that is important to understand from outside the implementation.

Avoid verbose documentation that merely restates:

- the function name;
- the parameter names;
- obvious return values;
- straightforward implementation details.

Do not write documentation just to make every function appear formally documented.

## Naming

Names should describe the actual concept represented by the code.

Prefer precise names over vague names such as:

- `data`;
- `result`;
- `handler`;
- `manager`;
- `processor`;
- `helper`;
- `utils`;

when a more meaningful name exists.

Do not make names artificially verbose.

The best name is the one that lets the surrounding code explain itself.

## Refactoring

Refactor when the change improves the code you are already working on.

Good reasons include:

- clarifying confusing logic;
- removing meaningful duplication;
- correcting a misleading abstraction;
- separating genuinely different responsibilities;
- simplifying an unnecessarily complicated implementation;
- making an important behavior easier to understand.

Do not refactor for aesthetic reasons alone.

Do not turn a focused change into an unrelated redesign.

## Avoid AI-shaped code

Do not produce code that looks like it was generated from a generic coding checklist.

In particular, avoid combinations of:

- excessive helper functions;
- excessive abstraction;
- repetitive comments;
- verbose docstrings;
- unnecessary type or validation layers;
- defensive checks without a real failure mode;
- symmetrical structure created for appearance;
- generic names;
- excessive blank lines;
- unnecessary wrappers;
- boilerplate introduced without a concrete purpose.

The presence of any one of these is not automatically wrong.

The concern is code that accumulates them without a real reason.

## Do not make code artificially "clean"

Code does not need to look pristine.

Real software contains:

- substantial modules;
- pragmatic decisions;
- local exceptions;
- code that reflects external constraints;
- implementations that are intentionally straightforward rather than abstract.

Do not polish away useful character simply to make the code conform to an idealized style.

Optimize for understanding and maintainability.

## Human readability test

Before considering an implementation finished, read it as if you encountered it six months later.

Ask:

- Can I understand the main behavior without jumping through unnecessary abstractions?
- Are the important decisions visible?
- Do names describe real concepts?
- Is there unnecessary ceremony?
- Did I introduce abstractions that the problem does not need?
- Did I make the code shorter at the expense of clarity?
- Did I make the code longer without adding clarity?
- Would another developer be comfortable modifying this?

If the code is substantial because the problem is substantial, that is fine.

If it is complicated because the implementation is complicated unnecessarily, simplify it.

## Core principle

Write code that feels intentional.

Not minimal for the sake of minimalism.

Not abstract for the sake of abstraction.

Not verbose for the sake of documentation.

Not clever for the sake of cleverness.

Not decorated for the sake of presentation.

**Make the code itself explain the problem and its solution.**