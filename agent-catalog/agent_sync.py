#!/usr/bin/env python3
"""Agent catalog build and per-start profile sync (docs/adr/0014 sections 1-4).

build <profile-dir>
    Validates the base profile and writes catalog.json (the index the broker
    reads) and CATALOG_VERSION into it. Runs once, in the catalog image build.

sync
    Runs as the init container of every agent pod, as a uid other than the
    agent's, so the catalog layer it writes is read-only to the agent:
      1. applies the selection passed by the broker (AGENT_SELECTION),
      2. replaces catalog/ with the image's copy,
      3. rebuilds catalog-enabled/ (the skills dir every runtime reads),
      4. renders the runtime's config (AGENT_RUNTIME: hermes, pi, opencode or dsh)
         with the MCP servers of mcp-servers.yaml,
      5. hermes only: moves personal skills shadowed by an enabled catalog
         skill into skills/.archive/ (Hermes does not scan it),
      6. prints a compact JSON report to the termination log for the broker.
"""

import copy
import hashlib
import json
import os
import shutil
import sys
import time

import yaml

CATALOG_DIR = os.environ.get("CATALOG_DIR", "/catalog")
AGENT_HOME = os.environ.get("AGENT_HOME", "/opt/data/home")
REPORT_PATH = os.environ.get("REPORT_PATH", "/dev/termination-log")
RUNTIMES = ("hermes", "pi", "opencode", "dsh")

# Personal-layer directories the agent writes into. Group-writable + setgid so
# files created by either uid stay in the shared group.
PERSONAL_DIRS = ("skills", "memories", "sessions", "logs")
PERSONAL_FILES = ("config.user.yaml", "SOUL.user.md")

def load_yaml(path, default):
    try:
        with open(path, encoding="utf-8") as fh:
            data = yaml.safe_load(fh)
    except FileNotFoundError:
        return default
    return default if data is None else data


def skill_description(skill_dir):
    """Frontmatter `description` of SKILL.md, or its first non-heading line."""
    with open(os.path.join(skill_dir, "SKILL.md"), encoding="utf-8") as fh:
        text = fh.read()
    body = text
    if text.startswith("---"):
        end = text.find("\n---", 3)
        if end != -1:
            meta = yaml.safe_load(text[3:end]) or {}
            if meta.get("description"):
                return str(meta["description"]).strip()
            body = text[end + 4:]
    for line in body.splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            return line[:120]
    return ""


def skill_name(skill_dir):
    with open(os.path.join(skill_dir, "SKILL.md"), encoding="utf-8") as fh:
        text = fh.read()
    if text.startswith("---"):
        end = text.find("\n---", 3)
        if end != -1:
            meta = yaml.safe_load(text[3:end]) or {}
            if meta.get("name"):
                return str(meta["name"]).strip()
    return os.path.basename(skill_dir)


def tree_hash(root):
    digest = hashlib.sha256()
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for name in sorted(filenames):
            if name in ("catalog.json", "CATALOG_VERSION"):
                continue
            path = os.path.join(dirpath, name)
            digest.update(os.path.relpath(path, root).encode())
            with open(path, "rb") as fh:
                digest.update(fh.read())
    return digest.hexdigest()[:12]


def build(profile_dir):
    meta = load_yaml(os.path.join(profile_dir, "catalog.yaml"), {}).get("skills") or {}
    skills_root = os.path.join(profile_dir, "skills")
    present = sorted(
        d for d in os.listdir(skills_root)
        if os.path.isfile(os.path.join(skills_root, d, "SKILL.md")))
    missing = sorted(set(meta) - set(present))
    if missing:
        sys.exit(f"catalog.yaml names skills that do not exist: {', '.join(missing)}")
    for key in load_yaml(os.path.join(profile_dir, "locked-keys.yaml"), []):
        if not isinstance(key, str):
            sys.exit(f"locked-keys.yaml: not a dotted key: {key!r}")
    registry = load_yaml(os.path.join(profile_dir, "mcp-servers.yaml"), {})
    for name, spec in (registry.get("servers") or {}).items():
        if not registry.get("pat_service") or not (spec or {}).get("flag") or not (spec or {}).get("timeout"):
            sys.exit(f"mcp-servers.yaml: {name} needs pat_service, flag and timeout")
    index = []
    for name in present:
        entry = meta.get(name) or {}
        required = bool(entry.get("required", False))
        index.append({
            "name": name,
            "description": skill_description(os.path.join(skills_root, name)),
            "required": required,
            "default_enabled": True if required else bool(entry.get("default_enabled", True)),
        })
    version = tree_hash(profile_dir)
    with open(os.path.join(profile_dir, "catalog.json"), "w", encoding="utf-8") as fh:
        json.dump({"version": version, "skills": index}, fh, indent=2)
    with open(os.path.join(profile_dir, "CATALOG_VERSION"), "w", encoding="utf-8") as fh:
        fh.write(version + "\n")
    print(f"catalog {version}: {len(index)} skills")


def enabled_skills(index, selection):
    disabled = set(selection.get("disabled") or [])
    enabled = set(selection.get("enabled") or [])
    out = []
    for skill in index:
        name = skill["name"]
        if skill["required"]:
            out.append(name)
        elif skill["default_enabled"] and name not in disabled:
            out.append(name)
        elif not skill["default_enabled"] and name in enabled:
            out.append(name)
    return out


def deep_merge(base, override):
    out = copy.deepcopy(base)
    for key, value in (override or {}).items():
        if isinstance(value, dict) and isinstance(out.get(key), dict):
            out[key] = deep_merge(out[key], value)
        else:
            out[key] = copy.deepcopy(value)
    return out


def get_path(data, dotted):
    node = data
    for part in dotted.split("."):
        if not isinstance(node, dict) or part not in node:
            return False, None
        node = node[part]
    return True, node


def set_path(data, dotted, value, present):
    parts = dotted.split(".")
    node = data
    for part in parts[:-1]:
        if not isinstance(node.get(part), dict):
            if not present:
                return
            node[part] = {}
        node = node[part]
    if present:
        node[parts[-1]] = copy.deepcopy(value)
    else:
        node.pop(parts[-1], None)


def merged_config(catalog_cfg, user_cfg, locked):
    merged = deep_merge(catalog_cfg, user_cfg if isinstance(user_cfg, dict) else {})
    for key in locked:
        present, value = get_path(catalog_cfg, key)
        set_path(merged, key, value, present)
    return merged


def write_atomic(path, text, mode=0o644):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(text)
    os.chmod(tmp, mode)
    os.replace(tmp, path)


def swap_dir(new, final):
    """Replace `final` with `new`; a crash leaves either old or new in place."""
    old = final + ".old"
    if os.path.isdir(old):
        shutil.rmtree(old)
    if os.path.isdir(final):
        os.rename(final, old)
    os.rename(new, final)
    if os.path.isdir(old):
        shutil.rmtree(old)


def copy_readonly(src, dst):
    shutil.copytree(src, dst)
    for dirpath, dirnames, filenames in os.walk(dst):
        os.chmod(dirpath, 0o755)
        for name in filenames:
            os.chmod(os.path.join(dirpath, name), 0o644)


def personal_skill_dirs(skills_root):
    for dirpath, dirnames, filenames in os.walk(skills_root):
        dirnames[:] = [d for d in dirnames if not d.startswith(".")]
        if "SKILL.md" in filenames:
            dirnames[:] = []
            yield dirpath


def mcp_servers(catalog):
    """Enabled servers of mcp-servers.yaml as {name: (url, timeout seconds)},
    plus every name the registry knows."""
    registry = load_yaml(os.path.join(catalog, "mcp-servers.yaml"), {})
    base = str(registry.get("pat_service", "")).rstrip("/")
    known = registry.get("servers") or {}
    enabled = {
        name: (f"{base}/mcp/{name}/", int(spec["timeout"]))
        for name, spec in known.items()
        if os.environ.get(spec["flag"]) == "true"
    }
    return enabled, list(known)


def soul_text(catalog, home):
    """Catalog SOUL.md plus the user's SOUL.user.md addendum."""
    with open(os.path.join(catalog, "SOUL.md"), encoding="utf-8") as fh:
        soul = fh.read()
    try:
        with open(os.path.join(home, "SOUL.user.md"), encoding="utf-8") as fh:
            addendum = fh.read().strip()
    except FileNotFoundError:
        addendum = ""
    if addendum:
        soul = soul.rstrip() + "\n\n" + addendum + "\n"
    return soul


def load_json(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def catalog_owned_dir(path):
    """A config dir the agent can add files to but whose catalog files it
    cannot replace (sticky, group-writable)."""
    os.makedirs(path, exist_ok=True)
    try:
        os.chmod(path, 0o3775)
    except PermissionError:
        pass


def render_hermes(catalog, home, soul, servers, all_servers):
    locked = load_yaml(os.path.join(catalog, "locked-keys.yaml"), [])
    catalog_cfg = load_yaml(os.path.join(catalog, "config.yaml"), {})
    # MCP servers are opt-in per install (docs/adr/0017 section 7, 0018
    # section 7). Every registry name is locked, so a disabled entry also
    # removes a user's copy of it.
    if servers:
        catalog_cfg["mcp_servers"] = {
            name: {"url": url, "headers": {"Authorization": "Bearer ${HERMES_INFERENCE_KEY}"},
                   "timeout": timeout}
            for name, (url, timeout) in servers.items()
        }
    locked = list(locked) + ["mcp_servers." + name for name in all_servers]
    cfg = merged_config(
        catalog_cfg,
        load_yaml(os.path.join(home, "config.user.yaml"), {}),
        locked)
    if cfg.get("mcp_servers") == {}:
        del cfg["mcp_servers"]
    write_atomic(os.path.join(home, "config.yaml"), yaml.safe_dump(cfg, sort_keys=False))
    write_atomic(os.path.join(home, "SOUL.md"), soul)


def render_pi(catalog, home, soul, servers):
    """PI_CODING_AGENT_DIR is <home>/pi (agent-adapter). pi has no MCP
    client of its own; pi-mcp-adapter, shipped in the agent image, reads
    mcp.json from the agent dir."""
    src = os.path.join(catalog, "runtimes", "pi")
    agent_dir = os.path.join(home, "pi")
    catalog_owned_dir(agent_dir)
    settings = load_json(os.path.join(src, "settings.json"))
    settings["skills"] = [os.path.join(home, "catalog-enabled")]
    write_atomic(os.path.join(agent_dir, "settings.json"), json.dumps(settings, indent=2) + "\n")
    shutil.copyfile(os.path.join(src, "models.json"), os.path.join(agent_dir, "models.json.tmp"))
    os.replace(os.path.join(agent_dir, "models.json.tmp"), os.path.join(agent_dir, "models.json"))
    mcp = {
        "settings": {"allowInstall": False, "hostConfigDiscovery": "off"},
        "mcpServers": {
            name: {"url": url, "headers": {"Authorization": "Bearer ${AGENT_INFERENCE_KEY}"},
                   # Connected at start, so the model sees the server's tools
                   # themselves rather than only the adapter's `mcp` proxy.
                   "lifecycle": "keep-alive", "directTools": True,
                   "requestTimeoutMs": timeout * 1000}
            for name, (url, timeout) in servers.items()
        },
    }
    write_atomic(os.path.join(agent_dir, "mcp.json"), json.dumps(mcp, indent=2) + "\n")
    write_atomic(os.path.join(agent_dir, "AGENTS.md"), soul)


def render_opencode(catalog, home, soul, servers):
    """OPENCODE_CONFIG is <home>/opencode/opencode.json (agent-adapter)."""
    config_dir = os.path.join(home, "opencode")
    catalog_owned_dir(config_dir)
    cfg = load_json(os.path.join(catalog, "runtimes", "opencode", "opencode.json"))
    cfg["skills"] = {"paths": [os.path.join(home, "catalog-enabled")]}
    cfg["instructions"] = [os.path.join(config_dir, "AGENTS.md")]
    cfg["mcp"] = {
        name: {"type": "remote", "url": url, "enabled": True, "oauth": False,
               "headers": {"Authorization": "Bearer {env:AGENT_INFERENCE_KEY}"},
               "timeout": timeout * 1000}
        for name, (url, timeout) in servers.items()
    }
    write_atomic(os.path.join(config_dir, "opencode.json"), json.dumps(cfg, indent=2) + "\n")
    write_atomic(os.path.join(config_dir, "AGENTS.md"), soul)


def render_dsh(catalog, home, soul, servers):
    """DSH_HOME is <home>/dsh (agent-adapter dsh.mjs): the home-level
    cordis.patch.yml shared by the acp and web profiles, their --patch
    overlays, and the user-global AGENTS.md."""
    src = os.path.join(catalog, "runtimes", "dsh")
    dsh_home = os.path.join(home, "dsh")
    catalog_owned_dir(dsh_home)
    with open(os.path.join(src, "cordis.patch.yml"), encoding="utf-8") as fh:
        patch = fh.read().rstrip() + "\n"
    patch += "\n" + yaml.safe_dump(
        [{"id": "skill-filesystem",
          "config": {"customSkillDirs": [os.path.join(home, "catalog-enabled")]}}],
        sort_keys=False)
    if servers:
        # The header is a Cordis !!js expression, so these rows are written
        # as text rather than through yaml.safe_dump.
        patch += "\n- insert:\n"
        for name, (url, timeout) in servers.items():
            patch += (
                f"    - id: mcp-{name}\n"
                "      name: '@deepseek-ai/dsh-mcp-client'\n"
                "      config:\n"
                f"        serverName: {name}\n"
                "        transport: streamable-http\n"
                f"        url: {json.dumps(url)}\n"
                f"        toolCallTimeoutMs: {timeout * 1000}\n"
                "        headers:\n"
                "          Authorization: !!js '`Bearer ${process.env.AGENT_INFERENCE_KEY}`'\n")
    write_atomic(os.path.join(dsh_home, "cordis.patch.yml"), patch)
    for name in ("acp.patch.yml", "web.patch.yml", "approval-hooks.json"):
        shutil.copyfile(os.path.join(src, name), os.path.join(dsh_home, name + ".tmp"))
        os.replace(os.path.join(dsh_home, name + ".tmp"), os.path.join(dsh_home, name))
    write_atomic(os.path.join(dsh_home, "AGENTS.md"), soul)


def archive_shadowed(home, enabled):
    # Catalog wins on a name collision (ADR 0014 section 2); Hermes itself
    # prefers the local copy, so the personal one is moved out of its scan.
    shadowed, shadow_failed = [], []
    skills_root = os.path.join(home, "skills")
    enabled_set = set(enabled)
    for path in list(personal_skill_dirs(skills_root)):
        name = skill_name(path)
        if name not in enabled_set:
            continue
        dest_root = os.path.join(skills_root, ".archive", "shadowed-by-catalog")
        dest = os.path.join(dest_root, f"{name}-{int(time.time())}")
        try:
            os.makedirs(dest_root, exist_ok=True)
            os.rename(path, dest)
            shadowed.append(name)
        except OSError as exc:
            shadow_failed.append(name)
            print(f"cannot move shadowed personal skill {path}: {exc}", file=sys.stderr)
    return shadowed, shadow_failed


def sync():
    home = AGENT_HOME
    os.makedirs(home, exist_ok=True)
    try:
        # Sticky + group-writable: the agent can create its own files here
        # but cannot rename or delete the catalog layer owned by this uid.
        os.chmod(home, 0o3775)
    except PermissionError:
        pass
    # Recover from a crash between the two renames of swap_dir.
    for name in ("catalog", "catalog-enabled"):
        final = os.path.join(home, name)
        if not os.path.isdir(final) and os.path.isdir(final + ".old"):
            os.rename(final + ".old", final)

    first_start = not os.path.exists(os.path.join(home, ".catalog-version"))
    for name in PERSONAL_DIRS:
        path = os.path.join(home, name)
        if not os.path.isdir(path):
            os.makedirs(path)
            os.chmod(path, 0o2775)
    for name in PERSONAL_FILES:
        path = os.path.join(home, name)
        if not os.path.exists(path):
            write_atomic(path, "", 0o664)

    selection_path = os.path.join(home, "catalog-selection.yaml")
    pending = os.environ.get("AGENT_SELECTION", "").strip()
    if pending:
        wanted = json.loads(pending)
        selection = {
            "disabled": sorted(set(wanted.get("disabled") or [])),
            "enabled": sorted(set(wanted.get("enabled") or [])),
        }
        write_atomic(selection_path, yaml.safe_dump(selection, sort_keys=True))
    selection = load_yaml(selection_path, {}) or {}

    new = os.path.join(home, "catalog.new")
    if os.path.isdir(new):
        shutil.rmtree(new)
    copy_readonly(CATALOG_DIR, new)
    swap_dir(new, os.path.join(home, "catalog"))
    catalog = os.path.join(home, "catalog")

    with open(os.path.join(catalog, "catalog.json"), encoding="utf-8") as fh:
        index = json.load(fh)
    version = index["version"]
    enabled = enabled_skills(index["skills"], selection)

    new = os.path.join(home, "catalog-enabled.new")
    if os.path.isdir(new):
        shutil.rmtree(new)
    os.makedirs(new)
    for name in enabled:
        copy_readonly(os.path.join(catalog, "skills", name), os.path.join(new, name))
    os.chmod(new, 0o755)
    swap_dir(new, os.path.join(home, "catalog-enabled"))

    runtime = os.environ.get("AGENT_RUNTIME", "hermes")
    if runtime not in RUNTIMES:
        sys.exit(f"unknown AGENT_RUNTIME {runtime!r}")
    soul = soul_text(catalog, home)
    servers, all_servers = mcp_servers(catalog)
    shadowed, shadow_failed = [], []
    if runtime == "hermes":
        render_hermes(catalog, home, soul, servers, all_servers)
        shadowed, shadow_failed = archive_shadowed(home, enabled)
    elif runtime == "pi":
        render_pi(catalog, home, soul, servers)
    elif runtime == "dsh":
        render_dsh(catalog, home, soul, servers)
    else:
        render_opencode(catalog, home, soul, servers)

    optional = [s["name"] for s in index["skills"] if not s["required"]]
    seen_path = os.path.join(home, ".catalog-seen")
    seen = set(load_yaml(seen_path, []) or [])
    new_optional = [] if first_start else sorted(set(optional) - seen)
    write_atomic(seen_path, yaml.safe_dump(sorted(set(optional) | seen)))
    write_atomic(os.path.join(home, ".catalog-version"), version + "\n")

    report = {
        "v": version,
        "on": sorted(enabled),
        "sel": selection,
        "new": new_optional,
        "shadowed": shadowed,
    }
    if shadow_failed:
        report["shadow_failed"] = shadow_failed
    text = json.dumps(report, separators=(",", ":"), sort_keys=True)
    print(text)
    try:
        with open(REPORT_PATH, "w", encoding="utf-8") as fh:
            fh.write(text)
    except OSError as exc:
        print(f"cannot write report to {REPORT_PATH}: {exc}", file=sys.stderr)


def main(argv):
    if len(argv) == 3 and argv[1] == "build":
        build(argv[2])
    elif len(argv) == 2 and argv[1] == "sync":
        sync()
    else:
        sys.exit("usage: agent_sync.py build <profile-dir> | sync")


if __name__ == "__main__":
    main(sys.argv)
