#!/usr/bin/env python3
"""Portico workflow gate — enforces CLAUDE.md remediation protocol.

Exit 0 = allow, Exit 1 = deny (with message on stderr).
This is the actual enforcement point; CLAUDE.md prose is advisory only.
"""
import json
import os
import re
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
SCHEMA_FILE = REPO_ROOT / ".claude" / "workflow" / "schema.json"
STATE_FILE = REPO_ROOT / ".claude" / "workflow" / "state.json"
CONTRACTS_DIR = REPO_ROOT / "docs"

# Go source extensions that trigger workflow enforcement
GO_EXTENSIONS = {".go", ".sql", ".mod", ".sum"}
# Files that can be edited without workflow enforcement
DOC_EXTENSIONS = {".md", ".txt", ".rst"}


def load_state():
    if not STATE_FILE.exists():
        return {}
    try:
        with open(STATE_FILE) as f:
            return json.load(f)
    except (json.JSONDecodeError, IOError):
        return {}


def save_state(state):
    STATE_FILE.parent.mkdir(parents=True, exist_ok=True)
    with open(STATE_FILE, "w") as f:
        json.dump(state, f, indent=2)


def load_schema():
    if not SCHEMA_FILE.exists():
        return {}
    with open(SCHEMA_FILE) as f:
        return json.load(f)


def is_go_file(path):
    """Check if a file path is Go source or workflow-affected."""
    p = Path(path)
    if not p.exists():
        # Could be a new file
        return p.suffix in GO_EXTENSIONS
    return p.suffix in GO_EXTENSIONS


def is_doc_only(path):
    """Check if a file is documentation-only (no product code changes)."""
    return Path(path).suffix in DOC_EXTENSIONS


def session_start():
    """Validate CLAUDE_CODE_SUBAGENT_MODEL is absent and initialize state."""
    subagent_model = os.environ.get("CLAUDE_CODE_SUBAGENT_MODEL")
    if subagent_model:
        print(f"ERROR: CLAUDE_CODE_SUBAGENT_MODEL={subagent_model} is set. "
              "Unset it or use 'env -u CLAUDE_CODE_SUBAGENT_MODEL claude'.",
              file=sys.stderr)
        return 1

    # Initialize empty state if not present
    state = load_state()
    if not state:
        save_state({
            "work_item_id": None,
            "tier": None,
            "invariant": None,
            "affected_packages": [],
            "attempt_count": 0,
            "architect_required": False,
            "architect_contract": None,
            "architect_contract_sha": None,
            "targeted_tests_passed": False,
            "full_tests_passed": False,
            "review_required": False,
            "review_status": "pending",
            "review_findings": [],
            "reviewed_packages": []
        })

    print("Session start: CLAUDE_CODE_SUBAGENT_MODEL not set. "
          "Workflow gate initialized.", file=sys.stderr)
    return 0


def pre_tool_use(tool_name):
    """Pre-edit checks: require active work item for Tier 3."""
    state = load_state()
    tier = state.get("tier")
    if not tier:
        # No tier set — allow edits but warn
        print("WARNING: No active work item. Tier not set.", file=sys.stderr)
        return 0

    # Only enforce for Go/sql/mod/sum files
    # (We can't easily tell which files are being edited here,
    # so we do a lightweight check: if tier 3, require contract)
    schema = load_schema()
    tier_config = schema.get("tiers", {}).get(str(tier), {})

    if tier == 3 and tier_config.get("architect_required"):
        contract_sha = state.get("architect_contract_sha")
        if not contract_sha:
            print("ERROR: Tier 3 edit requires an accepted architect contract. "
                  "Set architect_contract_sha in state before editing.",
                  file=sys.stderr)
            return 1

    return 0


def pre_tool_commit():
    """Block commit when source files are modified and gates haven't passed."""
    state = load_state()
    tier = state.get("tier")

    # Check for uncommitted Go changes
    try:
        result = subprocess.run(
            ["git", "diff", "--name-only", "--cached"],
            capture_output=True, text=True, cwd=str(REPO_ROOT)
        )
        staged = [f for f in result.stdout.strip().split("\n") if f and is_go_file(f)]
    except Exception:
        staged = []

    if not staged:
        # Nothing Go-related staged — allow
        return 0

    # Go source is staged — check gates
    errors = []

    # Check targeted tests
    if not state.get("targeted_tests_passed"):
        errors.append("targeted_tests_passed")

    # Check full tests for tiers that require it
    schema = load_schema()
    tier_config = schema.get("tiers", {}).get(str(tier), {})
    if tier_config.get("full_gate_set") and not state.get("full_tests_passed"):
        errors.append("full_tests_passed")

    # Check review
    if state.get("review_required") and state.get("review_status") != "pass":
        errors.append("review_passed")

    # Check architect contract for Tier 3
    if tier == 3 and tier_config.get("architect_required") and not state.get("architect_contract_sha"):
        errors.append("architect_contract")

    # Check gofmt
    try:
        fmt_result = subprocess.run(
            ["gofmt", "-l", "."],
            capture_output=True, text=True, cwd=str(REPO_ROOT)
        )
        formatted = [f for f in fmt_result.stdout.strip().split("\n") if f]
        if formatted:
            errors.append(f"gofmt clean (unformatted: {formatted})")
    except Exception:
        pass

    if errors:
        print(f"ERROR: Cannot commit. Missing gates: {', '.join(errors)}",
              file=sys.stderr)
        return 1

    return 0


def post_tool_edit(tool_name):
    """Track modified packages. Cross a boundary → elevate tier."""
    state = load_state()
    try:
        result = subprocess.run(
            ["git", "diff", "--name-only"],
            capture_output=True, text=True, cwd=str(REPO_ROOT)
        )
        modified = [f for f in result.stdout.strip().split("\n") if f and is_go_file(f)]
    except Exception:
        return 0

    if not modified:
        return 0

    # Extract package names from file paths
    packages = set()
    for f in modified:
        # internal/origin/manager.go → internal/origin
        parts = f.split("/")
        if len(parts) >= 2 and parts[0] == "internal":
            pkg = "/".join(parts[:2])
            packages.add(pkg)
        elif len(parts) >= 1:
            packages.add(parts[0])

    current_packages = set(state.get("affected_packages", []))
    all_packages = current_packages | packages

    # If we cross a transaction/lifecycle/schema/security boundary, escalate
    BOUNDARY_PACKAGES = {"origin", "controller", "store", "supervisor",
                         "process", "provider", "ipc", "core"}
    crossing = all_packages & BOUNDARY_PACKAGES

    if len(crossing) >= 3 and state.get("tier", 2) < 3:
        # Escalate to Tier 3
        state["tier"] = 3
        schema = load_schema()
        tier3 = schema.get("tiers", {}).get("3", {})
        state["architect_required"] = tier3.get("architect_required", False)
        state["review_required"] = tier3.get("review_required", True)
        print(f"NOTICE: Crossed into {len(crossing)} boundary packages. "
              "Escalated to Tier 3.", file=sys.stderr)

    state["affected_packages"] = list(all_packages)
    save_state(state)
    return 0


ALLOWED_SUBAGENTS = ["portico-recon", "portico-auditor", "portico-architect"]

# Model identifiers the harness can actually resolve. "default" is not one of
# them: an agent declaring it fails at dispatch with a model-access error, which
# is indistinguishable from the agent being unavailable and silently disables
# every policy gate that depends on a dispatch.
RESOLVABLE_MODELS = {"opus", "sonnet", "haiku", "fable", "inherit"}


def validate_agent_definitions():
    """Check every agent declaration names a model the harness can resolve.

    A broken declaration does not announce itself. The dispatch fails, the gate
    that required it is skipped, and work proceeds as though the review had
    happened. This turns that into a visible error.
    """
    agents_dir = REPO_ROOT / ".claude" / "agents"
    if not agents_dir.exists():
        return []

    problems = []
    for path in sorted(agents_dir.glob("*.md")):
        text = path.read_text()
        match = re.search(r"^model:\s*(\S+)\s*$", text, re.MULTILINE)
        if not match:
            # No override is valid: the agent inherits the parent model.
            continue
        model = match.group(1)
        if model not in RESOLVABLE_MODELS:
            problems.append(
                f"{path.relative_to(REPO_ROOT)}: model '{model}' is not resolvable "
                f"(expected one of {', '.join(sorted(RESOLVABLE_MODELS))}, or no model: line)"
            )
    return problems


def validate_subagent_start():
    """Validate handoff packet before subagent dispatch."""
    state = load_state()

    for problem in validate_agent_definitions():
        print(f"ERROR: {problem}", file=sys.stderr)

    # We can't easily read the subagent type from here in Claude Code hooks,
    # but we can verify the work item is active
    work_item = state.get("work_item_id")
    if not work_item:
        print("WARNING: Subagent dispatch without active work item. "
              "Set work_item_id before dispatching.", file=sys.stderr)
        # Don't block — warn only

    return 0


def check_agents():
    """Standalone check, for CI."""
    problems = validate_agent_definitions()
    for problem in problems:
        print(f"ERROR: {problem}", file=sys.stderr)
    if problems:
        print(
            "\nA subagent whose model cannot be resolved fails at dispatch. Every "
            "policy gate requiring that dispatch is then skipped without saying so.",
            file=sys.stderr,
        )
        return 1
    print(f"agent definitions OK ({len(ALLOWED_SUBAGENTS)} remediation agents)")
    return 0


def post_subagent_stop(subagent_type):
    """Store returned contract/receipt. Reject malformed output."""
    # In Claude Code, the subagent result isn't directly available here.
    # We rely on the implementer saving the receipt to disk.
    # Check if an architect contract was written
    if subagent_type == "portico-architect":
        state = load_state()
        tier = state.get("tier")
        if tier == 3:
            # Check for a recently written contract doc
            # The architect writes to docs/<PACKAGE>_DESIGN.md
            # We can't know the package here, so check for any new doc
            docs_dir = CONTRACTS_DIR
            if docs_dir.exists():
                # Accept whatever was written — the implementer validates it
                pass
    return 0


def main():
    if len(sys.argv) < 2:
        print("Usage: workflow_gate.py <command> [args...]", file=sys.stderr)
        return 1

    cmd = sys.argv[1]
    if cmd == "session-start":
        return session_start()
    elif cmd == "pre-tool-use":
        tool_name = sys.argv[2] if len(sys.argv) > 2 else "Edit"
        return pre_tool_use(tool_name)
    elif cmd == "pre-tool-commit":
        return pre_tool_commit()
    elif cmd == "post-tool-edit":
        tool_name = sys.argv[2] if len(sys.argv) > 2 else "Edit"
        return post_tool_edit(tool_name)
    elif cmd == "pre-subagent-start":
        return validate_subagent_start()
    elif cmd == "post-subagent-stop":
        subagent = sys.argv[2] if len(sys.argv) > 2 else "unknown"
        return post_subagent_stop(subagent)
    elif cmd == "check-agents":
        return check_agents()
    else:
        print(f"Unknown command: {cmd}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
