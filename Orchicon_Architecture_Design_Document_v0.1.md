# Orchicon

## Executive Summary & Architecture Design Document

> **Version:** 0.1

## Vision

AI-assisted software development is rapidly evolving from individual AI
conversations into autonomous, multi-agent workflows.

**Orchicon** is an AI orchestration and operations platform that
coordinates autonomous AI work as reliable, observable, recoverable
systems. It separates **orchestration** from **execution**: Orchicon
manages projects, workers, scheduling, policies, telemetry, recovery,
and governance, while pluggable runtimes execute the work.

Its mission is to make autonomous AI systems reliable, observable,
recoverable, and manageable at scale.

------------------------------------------------------------------------

# Design Principles

-   **Runtime Agnostic** --- Integrate existing runtimes through
    adapters.
-   **Project-Centric** --- Projects are the source of truth.
-   **State Driven** --- Continuously reconcile desired and actual
    state.
-   **Observable by Default** --- Every meaningful action produces
    telemetry.
-   **Recoverable** --- Long-running workflows survive failures.
-   **Self-Healing** --- Detect unhealthy execution, preserve progress,
    execute recovery workflows before escalating to humans.
-   **Composable** --- Workers and workflows are reusable building
    blocks.
-   **Human Governed** --- Humans define objectives and policies.
-   **Extensible** --- New runtimes, providers and integrations plug in
    without changing the orchestration core.

------------------------------------------------------------------------

# Product Overview

Orchicon treats AI work as managed execution rather than conversations.

Users create projects, reusable workers, execution plans and policies
while Orchicon coordinates execution, maintains state, collects
telemetry, and continuously reconciles progress toward the desired
outcome.

Core philosophy:

-   Orchicon orchestrates.
-   Runtimes execute.

------------------------------------------------------------------------

# Orchicon API Platform

The API is a first-class platform interface.

Everything available in the UI should also be available through the API.

The UI is simply one client of the API.

The API allows external systems to:

-   Create and manage projects
-   Manage workers
-   Build workflows
-   Submit tasks
-   Monitor execution
-   Retrieve telemetry
-   Manage policies
-   Trigger recovery workflows
-   Completely control Orchicon programmatically

This enables CI/CD systems, portals, dashboards, automation platforms,
and even other AI systems to embed Orchicon as an orchestration
platform.

------------------------------------------------------------------------

# Core Concepts

## Projects

Projects are the persistent source of truth and contain goals,
architecture, documentation, execution history, telemetry and work
hierarchy.

## Work Hierarchy

Epics → Features → Tasks → Subtasks.

Each task tracks description, status, dependencies, assigned worker,
priority, budgets, execution history, context usage and results.

## AI Workers

Workers are reusable execution profiles that can be reused across
projects through drag-and-drop composition.

Each worker defines:

-   Name
-   Description
-   Purpose
-   Runtime
-   AI Model
-   System Prompt
-   Context Sources
-   Permissions
-   Budget Constraints
-   Execution Policies
-   Concurrency Limits

Workers intentionally do not own runtime-specific capabilities such as
MCP servers, tools, terminal access, plugins or file editing.

Those belong to the execution runtime.

## Runtime Abstraction

Execution runtimes provide:

-   Model execution
-   Tool calling
-   MCP integration
-   File editing
-   Terminal access
-   Context management
-   Runtime-specific capabilities

Initial runtime: OpenCode.

Future runtimes may include Claude Code, Codex, local runtimes and
future providers.

------------------------------------------------------------------------

# Runtime & Scheduling Engine

Inspired by Kubernetes, Orchicon continuously reconciles desired project
state with current execution state.

The scheduler evaluates:

-   Task queues
-   Dependencies
-   Worker availability
-   Health
-   Policies
-   Budgets
-   Resource utilization
-   Project state

Responsibilities include scheduling, lifecycle management, dispatching
work, coordinating runtimes, recovering failures and maintaining project
state.

------------------------------------------------------------------------

# Worker Health Management

Workers are monitored for lifecycle state, heartbeat, progress, latency,
context usage, token consumption, cost, runtime health and failure
history.

------------------------------------------------------------------------

# Autonomous Recovery

When a worker becomes unhealthy, stalls, exceeds retry policies,
exhausts its context window, or otherwise stops making forward progress,
Orchicon initiates a configurable Recovery Workflow.

A default recovery workflow consists of:

1.  Capture the worker's execution state.
2.  Generate a concise summary of the completed context.
3.  Preserve execution traces and artifacts.
4.  Launch a configurable **Reviewer Worker** to validate completed
    work.
5.  Produce a continuation plan describing remaining work.
6.  Launch a replacement worker using the summarized context and
    continuation plan.
7.  Resume execution from the last successful checkpoint.

Organizations may customize recovery workflows to fit their own
engineering practices.

Recovery workflows may utilize Reviewer, Architect, Security, Project
Manager or other specialized workers.

The objective is not simply restarting failed workers, but preserving
progress, minimizing context loss, and enabling long-running autonomous
projects to continue with minimal human intervention.

------------------------------------------------------------------------

# AI Gateway

Centralized provider routing, token accounting, cost tracking, request
tracing, policy enforcement, failover and usage analytics.

------------------------------------------------------------------------

# Observability

Telemetry includes task metrics, worker metrics, execution traces,
prompts, responses, tool calls (where exposed by the runtime), runtime
events, file changes, errors, recovery actions and costs.

------------------------------------------------------------------------

# Human Oversight

Humans define objectives, budgets, policies and approvals.

Orchicon manages execution.

------------------------------------------------------------------------

# Initial Platform Strategy

Integrate existing runtimes first through adapters while building a
stable orchestration platform.

Long term, Orchicon may provide a native runtime for organizations
requiring deeper telemetry, deterministic execution and tighter
governance without changing the orchestration architecture.

------------------------------------------------------------------------

# Long-Term Vision

The future of software development is coordinated teams of autonomous AI
workers operating across heterogeneous execution environments.

Orchicon becomes the operational foundation that makes those teams
reliable, observable, recoverable and manageable at scale.
