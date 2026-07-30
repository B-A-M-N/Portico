#!/usr/bin/env python3
"""Validate that a command spec is read-only.

Checks that a CommandSpec (executable + args + env + shell) does not
contain write operations. Used as a pre-flight gate before accepting
a command source for a Portico connection.

Usage:
    validate_readonly_command.py <json-file>
    validate_readonly_command.py --executable <cmd> [--arg <arg>]... [--env KEY=VAL]...
    echo '{"executable":"python3","args":["-m","http.server"]}' | validate_readonly_command.py --stdin

Exit 0 = read-only, Exit 1 = write operations detected or invalid input.
"""
import json
import os
import re
import sys

# Executables that inherently perform write operations.
WRITE_EXECUTABLES = {
    "rm", "mv", "cp", "ln", "mkdir", "rmdir", "touch", "truncate",
    "shred", "wipe", "install",
    "dd", "mkfs", "mkfs.ext4", "mkfs.vfat", "fdisk", "parted",
    "mount", "umount", "losetup",
    "chmod", "chown", "chgrp", "setfacl",
    "apt", "apt-get", "dpkg", "yum", "dnf", "pacman", "pip", "npm",
    "cargo", "go", "make", "cmake",
    "tar", "unzip", "gunzip", "bunzip2", "xz",
    "systemctl", "service", "initctl", "shutdown", "reboot", "halt",
    "kill", "killall", "pkill",
    "useradd", "userdel", "usermod", "groupadd", "groupdel",
}

# Flags that indicate write operations for write-capable executables.
WRITE_FLAGS_FOR_MUTATORS = {
    "--write", "-w", "--remove", "--delete", "-rf", "-fr", "-R",
    "--recursive", "--force", "-f", "--overwrite", "--replace",
    "--create", "--new", "--append", "-a",
}

# Shell redirect/pipe patterns that indicate writes.
SHELL_WRITE_PATTERNS = [
    re.compile(r'>\s*\S'),
    re.compile(r'>>\s*\S'),
    re.compile(r'\|\s*(?:rm|mv|cp|dd|tee|sh|bash)(?:\s|$)'),
]

# Environment variables that could enable code injection.
DANGEROUS_ENV = {
    "LD_PRELOAD", "LD_LIBRARY_PATH", "PYTHONPATH", "NODE_PATH",
}


def check_executable(executable):
    basename = os.path.basename(executable)
    if basename in WRITE_EXECUTABLES:
        return f"executable '{executable}' is a write-capable command"
    return None


def check_args(executable, args):
    basename = os.path.basename(executable)
    for arg in args:
        if arg in (">", ">>", "1>", "2>", "&>") or arg.startswith(">>") or (
            arg.startswith(">") and len(arg) > 1 and not arg.startswith(">&")
        ):
            return f"argument '{arg}' is a write redirect"
        if arg in WRITE_FLAGS_FOR_MUTATORS and basename in (
            "rm", "mv", "cp", "dd", "tar", "mkdir", "rmdir"
        ):
            return f"argument '{arg}' is a write flag for '{basename}'"
        if re.match(r'^(-o|--output|--file|--destination|--dir)=', arg):
            return f"argument '{arg}' specifies a write destination"
    return None


def check_shell_command(executable, args):
    cmd = " ".join([executable] + list(args))
    for pattern in SHELL_WRITE_PATTERNS:
        if pattern.search(cmd):
            return f"shell command contains write pattern: {pattern.pattern}"
    return None


def check_env(env):
    for key in env:
        if key in DANGEROUS_ENV:
            return f"environment variable '{key}' could enable code injection"
    return None


def validate_command_spec(spec):
    violations = []
    executable = spec.get("executable", "")
    if not executable:
        violations.append("missing 'executable' field")
        return violations
    args = spec.get("args", [])
    env = spec.get("env", {})
    use_shell = spec.get("use_shell", False)

    err = check_executable(executable)
    if err:
        violations.append(err)
    err = check_args(executable, args)
    if err:
        violations.append(err)
    if use_shell:
        err = check_shell_command(executable, args)
        if err:
            violations.append(err)
    if env:
        err = check_env(env)
        if err:
            violations.append(err)
    return violations


def main():
    spec = {}
    if len(sys.argv) < 2:
        print("Usage: validate_readonly_command.py <json-file|--executable <cmd> [options]|--stdin>",
              file=sys.stderr)
        return 1

    if sys.argv[1] == "--stdin":
        spec = json.load(sys.stdin)
    elif sys.argv[1] == "--executable":
        spec["executable"] = sys.argv[2]
        spec["args"] = []
        spec["env"] = {}
        spec["use_shell"] = False
        i = 3
        while i < len(sys.argv):
            if sys.argv[i] == "--arg" and i + 1 < len(sys.argv):
                spec["args"].append(sys.argv[i + 1])
                i += 2
            elif sys.argv[i] == "--env" and i + 1 < len(sys.argv):
                kv = sys.argv[i + 1].split("=", 1)
                if len(kv) == 2:
                    spec["env"][kv[0]] = kv[1]
                i += 2
            elif sys.argv[i] == "--shell":
                spec["use_shell"] = True
                i += 1
            else:
                print(f"Unknown option: {sys.argv[i]}", file=sys.stderr)
                return 1
    elif sys.argv[1] in ("--help", "-h"):
        print(__doc__)
        return 0
    else:
        try:
            with open(sys.argv[1]) as f:
                spec = json.load(f)
        except (json.JSONDecodeError, IOError) as e:
            print(f"Error reading spec file: {e}", file=sys.stderr)
            return 1

    violations = validate_command_spec(spec)
    if violations:
        print("REJECTED: command spec contains write operations:", file=sys.stderr)
        for v in violations:
            print(f"  - {v}", file=sys.stderr)
        return 1
    else:
        executable = spec.get("executable", "<unknown>")
        print(f"OK: command spec for '{executable}' is read-only", file=sys.stderr)
        return 0


if __name__ == "__main__":
    sys.exit(main())
