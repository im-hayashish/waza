# Demo Script: waza

> Step-by-step walkthrough for demonstrating the waza framework.

## Demo Overview

**Duration:** ~12-15 minutes  
**Goal:** Show how to evaluate Agent Skills with the same rigor as AI agent evals

---

## Pre-Demo Setup

```bash
# Clean environment
cd ~/demo && rm -rf waza-demo
mkdir waza-demo && cd waza-demo

# Install waza
curl -fsSL https://raw.githubusercontent.com/microsoft/waza/main/install.sh | bash
```

---

## Part 1: Introduction (1 min)

### The Hook

> "Agent Skills are becoming as important as the agents themselves. But how do you know if a skill actually works? How do you catch regressions? How do you compare performance across models?"

> "Today I'll show you **waza** — a framework that brings the same evaluation rigor we use for AI agents to Agent Skills."

### The Problem

> "Right now, testing skills is mostly manual. You try a prompt, eyeball the result, and hope it works in production. That doesn't scale."

> "waza fixes this with:"
> - **Automated test suites** generated from SKILL.md files
> - **Multiple grader types** — from simple regex to LLM-as-judge
> - **Model comparison** — benchmark skills across GPT-4, Claude, etc.
> - **CI/CD integration** — catch regressions before they ship

### Quick Demo

```bash
waza --version
waza --help
```

**Key commands:**
- `generate` — Create eval from SKILL.md
- `run` — Execute evaluation
- `compare` — Benchmark across models

---

## Part 2: Create and Discover Skills (2 min)

### Talking Points

> "Let me show you how to find and create evals. You can discover skills in repositories or create new ones from scratch."

### Discover Skills with `coverage`

```bash
# Find all SKILL.md files in a directory tree
waza coverage ./skills
```

> "The `coverage` command walks through your skills directory and reports which ones have evals and which don't."

### Create a New Skill and Eval

```bash
# Start with `waza new` to create a skill from scratch
waza new skill code-reviewer
```

**Expected Output:**
```
✓ Created skill: code-reviewer
  
Structure created:
  skills/code-reviewer/
  ├── SKILL.md
  ├── index.ts (or index.py)
  └── code-reviewer.agent.json (if using Azure SDK)

Next: Create an eval suite
  waza new eval code-reviewer
```

> "This creates the skill metadata. Now let's create an eval suite for it."

### Create Eval Suite for the Skill

```bash
waza new eval code-reviewer
```

**Expected Output:**
```
✓ Created eval suite: code-reviewer-eval

Structure created:
  evals/code-reviewer-eval/
  ├── eval.yaml
  ├── tasks/
  │   └── task-001.yaml
  ├── fixtures/
  │   └── example.txt
  └── graders/
      └── custom_grader.py

Next: Run the evaluation
  waza run evals/code-reviewer-eval/eval.yaml --context-dir evals/code-reviewer-eval/fixtures
```

> "The `new` command scaffolds an eval suite with example tasks and fixtures. You customize it from there."

---

## Part 3: Initialize Your Project (2 min)

### Talking Points

> "Let's set up a new waza project with `waza init`. This creates the directory structure for skills and evals."

### Run Init Command

```bash
waza init ./my-skill-project
```

**Expected Output:**
```
✓ Initialized waza project at: ./my-skill-project

Structure created:
  my-skill-project/
  ├── skills/          # Your skill definitions
  ├── evals/           # Your evaluation suites
  ├── .github/
  │   └── workflows/
  │       └── eval.yml # CI/CD pipeline
  ├── README.md        # Getting started guide
  └── .gitignore       # waza-specific entries

Next step:
  Create your first skill: waza new skill my-skill
```

> "The `init` command scaffolds your project structure. Now you can create skills and evals."

### Show the Project Structure

```bash
tree my-skill-project -L 2
```

**Expected Structure:**
```
my-skill-project/
├── skills/          # Where skill definitions go
├── evals/           # Where eval suites go
├── .github/workflows/eval.yml
├── README.md
└── .gitignore
```

**Highlight:**
- `trials_per_task: 3` — Multiple runs for consistency
- Three metrics: task_completion (40%), trigger_accuracy (30%), behavior_quality (30%)
- Configurable thresholds — fail the eval if quality drops

---

## Part 3b: Create Tasks and Fixtures (2 min)

### Talking Points

> "Tasks are individual test cases. Let's create real tasks for our skill with fixtures."

### Create a Task File

```bash
cd my-skill-project

# Create your first task
waza new eval my-code-reviewer
```

**Expected Output:**
```
✓ Created eval suite

Structure created:
  evals/my-code-reviewer/
  ├── eval.yaml         # Main evaluation config
  ├── tasks/            # Test cases
  │   └── task-001.yaml
  ├── fixtures/         # Context files for the skill
  │   └── example.txt
  └── graders/          # Custom grading logic (optional)
```

### Create a Fixture File

```bash
# Add a code file as context
cat > evals/my-code-reviewer/fixtures/example.py << 'EOF'
def calculate_total(items):
    total = 0
    for i in range(len(items)):
        total = total + items[i]['price']
    return total
EOF
```

### Customize a Task File

```bash
cat > evals/my-code-reviewer/tasks/review-python-code.yaml << 'EOF'
# Review Python Code Task
id: review-python-001
name: Review Python Function
description: Test reviewing a Python function for issues

inputs:
  prompt: "Review this Python code for issues"
  context:
    language: "python"
  files:
    - path: example.py

expected:
  output_contains:
    - "review"
  
  behavior:
    max_tool_calls: 10

graders:
  - name: found_issues
    type: regex
    config:
      pattern: '(improve|suggest|issue)'
EOF
```

### Show the Task

```bash
cat evals/my-code-reviewer/tasks/review-python-code.yaml
```

**Highlight:**
- `inputs.files.path`: Filename relative to `--context-dir`
- `expected`: What success looks like
- `graders`: How to score the result

> "The path is relative to `--context-dir` — it's never hardcoded, so tasks are portable across machines."

---

## Part 4: Trigger Tests (1 min)

### Talking Points

> "Trigger tests ensure your skill activates on the right prompts and stays quiet on the wrong ones."

### View the Trigger Tests File

```bash
cat evals/my-code-reviewer/trigger_tests.yaml
```

**Expected Output:**
```yaml
# Trigger accuracy tests for code-reviewer
skill: my-code-reviewer

should_trigger:
  - prompt: "Review this code"
    reason: "Explicit review request"
  
  - prompt: "Check my Python function for bugs"
    reason: "Bug checking is code review"

should_not_trigger:
  - prompt: "What time is it?"
    reason: "Unrelated question"
  
  - prompt: "Deploy my app to Azure"
    reason: "Deployment, not review"
```

> "These trigger tests are part of the eval spec. They help measure whether your skill recognizes when to activate."

---


## Part 5: Run the Eval (2 min)

### Talking Points

> "Now let's run the evaluation. I'll use verbose mode to see what's happening in real-time."

### Execute the Eval with Verbose Output

```bash
waza run evals/my-code-reviewer/eval.yaml \
  --context-dir evals/my-code-reviewer/fixtures \
  -v
```

**Expected Output (verbose shows execution details):**
```
waza v0.11.0

✓ Loaded eval: my-code-reviewer
  Skill: my-code-reviewer
  Executor: mock
  Model: claude-sonnet-4-20250514
  Tasks: 1

Running tasks...
  ⠋ Task: review-python-001 [Trial 1/3]...
  ⠹ Grading...

╭────────────────────────── my-code-reviewer ──────────────────────╮
│ ✅ PASSED                                                         │
│                                                                   │
│ Pass Rate: 100.0% (1/1)                                           │
│ Composite Score: 1.00                                             │
│ Duration: 245ms                                                   │
╰───────────────────────────────────────────────────────────────────╯
```

> "Verbose mode shows you the execution in real-time — great for debugging!"

### Run with Different Task Filters

```bash
# Run only tasks matching a pattern
waza run evals/my-code-reviewer/eval.yaml \
  --context-dir evals/my-code-reviewer/fixtures \
  --task "review-python*"
```

> "The `--task` flag lets you run specific tasks for faster iteration."

### Save Results to JSON

```bash
waza run evals/my-code-reviewer/eval.yaml \
  --context-dir evals/my-code-reviewer/fixtures \
  --output results.json
```

**View the results:**
```bash
cat results.json | jq .summary
```

> "JSON results can be stored and compared later, perfect for tracking skill performance over time."

### Enable Session Logging for Debugging

```bash
waza run evals/my-code-reviewer/eval.yaml \
  --context-dir evals/my-code-reviewer/fixtures \
  --session-dir ./logs \
  --session-log
```

> "Session logs record every step of the evaluation in NDJSON format. Helpful for deep debugging and auditing."

---

## Part 6: Show Different Grader Types (1 min)

### Talking Points

> "waza supports multiple grader types, from simple regex matching to LLM-as-judge scoring."

### Check Available Graders

```bash
# View which validators are available
waza check evals/my-code-reviewer/eval.yaml
```

**Sample output:**
```
✓ Eval loaded successfully

Validators:
  - regex    Pattern matching against output
  - code     Python assertions
  - llm      LLM-as-judge with rubric
```

> "These graders give you flexibility: regex for CI/CD speed, LLM for nuanced quality assessment."

---

## Part 7: Run the Built-in Example (1 min)

### Talking Points

> "Let me show you a complete example — the code-explainer eval. It demonstrates the full workflow."

### Run Code Explainer Eval

```bash
# From the waza repo
cd /path/to/waza

# Run with mock executor and fixtures
waza run examples/code-explainer/eval.yaml \
  --context-dir examples/code-explainer/fixtures \
  -v
```

**Expected Output:**
```
waza v0.11.0

✓ Loaded eval: code-explainer-eval
  Skill: code-explainer
  Context: examples/code-explainer/fixtures (4 files)
  Executor: mock
  Model: claude-sonnet-4-20250514
  Tasks: 4

 Progress: ██████████████████████████████ 4/4 (100%)

╭──────────────────────── code-explainer-eval ────────────────────────╮
│ ✅ PASSED                                                           │
│                                                                     │
│ Pass Rate: 100.0% (4/4)                                             │
│ Composite Score: 1.00                                               │
│ Duration: 1230ms                                                    │
╰─────────────────────────────────────────────────────────────────────╯
```

### Explore the Example Structure

```bash
tree examples/code-explainer
```

**Structure:**
```
code-explainer/
├── SKILL.md                 # ⭐ Skill definition (source of truth)
├── eval.yaml                # Main eval config
├── fixtures/                # Code files the skill will explain
│   ├── factorial.py         # Python recursion
│   ├── fetch_user.js        # JavaScript async/await
│   ├── squares.py           # List comprehension
│   └── user_orders.sql      # SQL JOIN
├── tasks/                   # Test tasks
│   ├── explain-python-recursion.yaml
│   ├── explain-js-async.yaml
│   ├── explain-list-comprehension.yaml
│   └── explain-sql-join.yaml
└── trigger_tests.yaml       # Trigger accuracy tests
```

> "Notice it includes a SKILL.md — this is what guides the eval design. The fixtures directory has real code files for context."

---

## Part 8: Model Comparison (1.5 min)

### Talking Points

> "One of the most powerful features is comparing how your skill performs across different models."

### Run with Different Models

```bash
# Run with GPT-4o
waza run examples/code-explainer/eval.yaml \
  --context-dir examples/code-explainer/fixtures \
  --model gpt-4o \
  -o results-gpt4o.json

# Run with Claude
waza run examples/code-explainer/eval.yaml \
  --context-dir examples/code-explainer/fixtures \
  --model claude-sonnet-4-20250514 \
  -o results-claude.json
```

### Compare Results

```bash
waza compare results-gpt4o.json results-claude.json
```

**Expected Output:**
```
Model Comparison Report

              Summary Comparison              
┏━━━━━━━━━━━━━━━━━┳━━━━━━━━┳━━━━━━━━━━━━━━━━━┓
┃ Metric          ┃ gpt-4o ┃ claude-sonnet-4 ┃
┡━━━━━━━━━━━━━━━━━╇━━━━━━━━╇━━━━━━━━━━━━━━━━━┩
│ Pass Rate       │ 100.0% │          100.0% │
│ Composite Score │   1.00 │            1.00 │
│ Tasks Passed    │    4/4 │             4/4 │
│ Duration        │  403ms │           401ms │
└─────────────────┴────────┴─────────────────┘
```

> "This is incredibly useful for benchmarking and deciding which model works best for your skill."

---

## Part 9: Real Integration Testing (1 min)

### Talking Points

> "For real integration tests, you can use the copilot-sdk executor to get actual LLM responses."

### Run with Copilot SDK (requires auth)

```bash
# This uses real Copilot SDK - requires authentication
waza run examples/code-explainer/eval.yaml \
  --executor copilot-sdk \
  --context-dir examples/code-explainer/fixtures \
  --model claude-sonnet-4-20250514 \
  -v
```

> "The mock executor is perfect for CI/CD and fast iteration. The copilot-sdk executor is for real integration testing."

---

## Part 10: CI/CD Integration (30 sec)

### Talking Points

> "Skill evals integrate directly into your CI/CD pipeline."

### Show GitHub Actions Workflow

```bash
cat .github/workflows/waza-eval.yml | head -50
```

**Highlight:**
- Triggered on pull requests
- Configurable model selection
- Artifact uploads for results
- Integration with status checks

---

## Part 11: Summary (30 sec)

### Talking Points

> "To recap what we've seen:"

1. **Initialize** a project with `waza init`
2. **Create skills and evals** with `waza new skill` and `waza new eval`
3. **Define tasks** — individual test cases with fixtures
4. **Define triggers** — when should your skill activate?
5. **Choose graders** — regex, code assertions, or LLM-as-judge
6. **Run evals** — with `-v` for real-time output, `--context-dir` for project files
7. **Filter tasks** — use `--task` patterns for faster iteration
8. **Track results** — save JSON for later comparison and analysis
9. **Debug** — use session logging with `--session-log`
10. **Compare models** — benchmark across different LLMs
11. **Get results** — structured JSON aligned with agent eval standards

> "Skills are becoming as important as agents. Now we can evaluate them with the same rigor."

---

## Bonus: Quick Reference Commands

```bash
# Initialize new project
waza init my-project

# Create a new skill
waza new skill code-reviewer

# Create an eval suite for a skill
waza new eval code-reviewer

# Run evaluation (basic)
waza run evals/code-reviewer/eval.yaml

# Run with verbose output (see execution details)
waza run evals/code-reviewer/eval.yaml -v

# Run with project context
waza run evals/code-reviewer/eval.yaml \
  --context-dir evals/code-reviewer/fixtures

# Run with JSON output
waza run evals/code-reviewer/eval.yaml -o results.json

# Run specific task(s)
waza run evals/code-reviewer/eval.yaml --task "review-*"

# Run in parallel for speed
waza run evals/code-reviewer/eval.yaml --parallel

# Run with specific model
waza run evals/code-reviewer/eval.yaml --model gpt-4o

# Run with Copilot SDK (real integration)
waza run evals/code-reviewer/eval.yaml --executor copilot-sdk

# Compare results across models
waza compare results-gpt4o.json results-claude.json

# Check eval configuration
waza check evals/code-reviewer/eval.yaml

# Discover and run all evals in a directory
waza run --discover

# List available graders
waza check evals/code-reviewer/eval.yaml

# Enable session logging
waza run evals/code-reviewer/eval.yaml \
  --session-dir ./logs \
  --session-log

# Full debugging run
waza run evals/code-reviewer/eval.yaml -v \
  --context-dir evals/code-reviewer/fixtures \
  --output results.json \
  --session-dir ./logs \
  --session-log
```

---

## Demo Cleanup

```bash
# Remove demo project
rm -rf my-skill-project
```

---

## Key Messages for Demo

1. **"Evals for skills, just like evals for agents"** — Same patterns, same rigor
2. **"One CLI tool, zero Python"** — Pure Go implementation, fast and portable
3. **"Fixtures for realistic testing"** — Project files give context
4. **"Flexible graders"** — From regex to LLM-as-judge
5. **"Real-time verbose output"** — See execution as it happens
6. **"Session logging"** — Debug with full transcript
7. **"Model comparison"** — Benchmark skills across different LLMs
8. **"Two executor modes"** — Mock for CI/CD, Copilot SDK for real tests
9. **"CI/CD ready"** — Integrate into your pipeline
10. **"Portable tasks"** — Fixtures are always relative to --context-dir

---

## Appendix: Troubleshooting During Demo

### If `waza` command not found
```bash
curl -fsSL https://raw.githubusercontent.com/microsoft/waza/main/install.sh | bash
```

### If tasks not loading
```bash
# Check eval YAML syntax
waza check evals/my-eval/eval.yaml
```

### If results look wrong
```bash
# Run with verbose for details
waza run evals/my-eval/eval.yaml -v
```

### If you need to verify available commands
```bash
waza --help
```
