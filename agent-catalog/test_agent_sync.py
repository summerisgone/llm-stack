"""Unit tests for agent_sync. Run: python3 -m unittest agent-catalog/test_agent_sync.py"""

import json
import os
import shutil
import sys
import tempfile
import unittest

import yaml

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_sync  # noqa: E402

class JsLoader(yaml.SafeLoader):
    """Reads Cordis `!!js` expressions as plain strings."""


JsLoader.add_constructor("tag:yaml.org,2002:js", lambda loader, node: loader.construct_scalar(node))

PROFILE = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                       "..", "config", "agents", "base-profile")


class SyncTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.catalog = os.path.join(self.tmp, "catalog-src")
        shutil.copytree(PROFILE, self.catalog)
        agent_sync.build(self.catalog)
        self.home = os.path.join(self.tmp, "home")
        self.report = os.path.join(self.tmp, "report")
        agent_sync.CATALOG_DIR = self.catalog
        agent_sync.AGENT_HOME = self.home
        agent_sync.REPORT_PATH = self.report
        os.environ.pop("AGENT_SELECTION", None)
        os.environ.pop("WEB_SEARCH_ENABLED", None)
        os.environ.pop("REPOWISE_ENABLED", None)
        os.environ.pop("AGENT_RUNTIME", None)

    def tearDown(self):
        shutil.rmtree(self.tmp)

    def run_sync(self, selection=None):
        if selection is not None:
            os.environ["AGENT_SELECTION"] = json.dumps(selection)
        else:
            os.environ.pop("AGENT_SELECTION", None)
        agent_sync.sync()
        with open(self.report) as fh:
            return json.load(fh)

    def enabled_dirs(self):
        return sorted(os.listdir(os.path.join(self.home, "catalog-enabled")))

    def test_first_start_uses_defaults_and_announces_nothing(self):
        report = self.run_sync()
        self.assertEqual(report["on"], ["platform-guide", "repo-reader"])
        self.assertEqual(report["new"], [])
        self.assertEqual(self.enabled_dirs(), ["platform-guide", "repo-reader"])

    def test_selection_cannot_disable_required(self):
        report = self.run_sync({"disabled": ["platform-guide", "repo-reader"],
                                "enabled": ["change-summary"]})
        self.assertEqual(report["on"], ["change-summary", "platform-guide"])
        # Selection persists on the next start without an annotation.
        self.assertEqual(self.run_sync()["on"], ["change-summary", "platform-guide"])

    def test_locked_keys_win_over_user_config(self):
        self.run_sync()
        with open(os.path.join(self.home, "config.user.yaml"), "w") as fh:
            fh.write("model:\n  base_url: http://evil/v1\n  default: other\n"
                     "agent:\n  disabled_toolsets: []\n  max_turns: 20\n")
        self.run_sync()
        cfg = agent_sync.load_yaml(os.path.join(self.home, "config.yaml"), {})
        self.assertIn("pat-service", cfg["model"]["base_url"])
        self.assertEqual(cfg["model"]["default"], "other")
        self.assertIn("cronjob", cfg["agent"]["disabled_toolsets"])
        self.assertEqual(cfg["agent"]["max_turns"], 20)

    def test_web_search_entry_follows_install_switch(self):
        user_entry = ("mcp_servers:\n  web-search:\n    url: http://evil/mcp\n"
                      "  mine:\n    url: http://other/mcp\n")
        self.run_sync()
        with open(os.path.join(self.home, "config.user.yaml"), "w") as fh:
            fh.write(user_entry)
        self.run_sync()
        cfg = agent_sync.load_yaml(os.path.join(self.home, "config.yaml"), {})
        self.assertNotIn("web-search", cfg["mcp_servers"])
        self.assertIn("mine", cfg["mcp_servers"])
        self.assertIn("web", cfg["agent"]["disabled_toolsets"])

        os.environ["WEB_SEARCH_ENABLED"] = "true"
        self.run_sync()
        cfg = agent_sync.load_yaml(os.path.join(self.home, "config.yaml"), {})
        entry = cfg["mcp_servers"]["web-search"]
        self.assertIn("pat-service", entry["url"])
        self.assertEqual(entry["headers"]["Authorization"], "Bearer ${HERMES_INFERENCE_KEY}")
        self.assertNotIn("repowise", cfg["mcp_servers"])

    def test_repowise_entry_follows_its_own_switch(self):
        self.run_sync()
        with open(os.path.join(self.home, "config.user.yaml"), "w") as fh:
            fh.write("mcp_servers:\n  repowise:\n    url: http://evil/mcp\n")
        self.run_sync()
        cfg = agent_sync.load_yaml(os.path.join(self.home, "config.yaml"), {})
        self.assertNotIn("repowise", cfg.get("mcp_servers", {}))

        os.environ["REPOWISE_ENABLED"] = "true"
        self.run_sync()
        cfg = agent_sync.load_yaml(os.path.join(self.home, "config.yaml"), {})
        self.assertEqual(list(cfg["mcp_servers"]), ["repowise"])
        self.assertIn("/mcp/repowise/", cfg["mcp_servers"]["repowise"]["url"])

    def test_new_optional_skill_announced_once(self):
        self.run_sync()
        os.makedirs(os.path.join(self.catalog, "skills", "fresh"))
        with open(os.path.join(self.catalog, "skills", "fresh", "SKILL.md"), "w") as fh:
            fh.write("---\nname: fresh\ndescription: new one\n---\n")
        agent_sync.build(self.catalog)
        self.assertEqual(self.run_sync()["new"], ["fresh"])
        self.assertEqual(self.run_sync()["new"], [])

    def test_personal_skill_shadowed_by_catalog_is_archived(self):
        self.run_sync()
        mine = os.path.join(self.home, "skills", "tools", "repo-reader")
        os.makedirs(mine)
        with open(os.path.join(mine, "SKILL.md"), "w") as fh:
            fh.write("---\nname: repo-reader\n---\nmine\n")
        keep = os.path.join(self.home, "skills", "my-own")
        os.makedirs(keep)
        with open(os.path.join(keep, "SKILL.md"), "w") as fh:
            fh.write("---\nname: my-own\n---\n")
        report = self.run_sync()
        self.assertEqual(report["shadowed"], ["repo-reader"])
        self.assertFalse(os.path.exists(mine))
        self.assertTrue(os.path.exists(keep))

    def test_personal_layer_survives_catalog_update(self):
        self.run_sync()
        with open(os.path.join(self.home, "memories", "MEMORY.md"), "w") as fh:
            fh.write("remember me")
        with open(os.path.join(self.home, "SOUL.user.md"), "w") as fh:
            fh.write("Answer in Russian.")
        shutil.rmtree(os.path.join(self.catalog, "skills", "change-summary"))
        with open(os.path.join(self.catalog, "catalog.yaml"), "w") as fh:
            fh.write("skills:\n  platform-guide:\n    required: true\n")
        agent_sync.build(self.catalog)
        self.run_sync()
        with open(os.path.join(self.home, "memories", "MEMORY.md")) as fh:
            self.assertEqual(fh.read(), "remember me")
        with open(os.path.join(self.home, "SOUL.md")) as fh:
            self.assertTrue(fh.read().rstrip().endswith("Answer in Russian."))
        self.assertFalse(os.path.exists(
            os.path.join(self.home, "catalog", "skills", "change-summary")))

    def test_pi_config_gets_skills_and_mcp(self):
        os.environ["AGENT_RUNTIME"] = "pi"
        os.environ["REPOWISE_ENABLED"] = "true"
        report = self.run_sync()
        self.assertEqual(report["on"], ["platform-guide", "repo-reader"])
        agent_dir = os.path.join(self.home, "pi")
        with open(os.path.join(agent_dir, "settings.json")) as fh:
            settings = json.load(fh)
        self.assertEqual(settings["skills"], [os.path.join(self.home, "catalog-enabled")])
        with open(os.path.join(agent_dir, "mcp.json")) as fh:
            servers = json.load(fh)["mcpServers"]
        self.assertEqual(list(servers), ["repowise"])
        self.assertEqual(servers["repowise"]["headers"]["Authorization"], "Bearer ${AGENT_INFERENCE_KEY}")
        with open(os.path.join(agent_dir, "models.json")) as fh:
            self.assertIn("pat-service", fh.read())
        self.assertTrue(os.path.exists(os.path.join(agent_dir, "AGENTS.md")))
        self.assertFalse(os.path.exists(os.path.join(self.home, "config.yaml")))

    def test_dsh_config_gets_skills_and_mcp(self):
        os.environ["AGENT_RUNTIME"] = "dsh"
        os.environ["WEB_SEARCH_ENABLED"] = "true"
        self.run_sync()
        dsh_home = os.path.join(self.home, "dsh")
        with open(os.path.join(dsh_home, "cordis.patch.yml")) as fh:
            text = fh.read()
        rows = yaml.load(text, Loader=JsLoader)
        by_id = {r["id"]: r for r in rows if "id" in r}
        self.assertEqual(by_id["llm-pi-ai"]["config"]["providers"]["pat"]["apiKeyEnv"], "AGENT_INFERENCE_KEY")
        self.assertEqual(by_id["skill-filesystem"]["config"]["customSkillDirs"],
                         [os.path.join(self.home, "catalog-enabled")])
        inserted = [r for row in rows for r in row.get("insert", [])]
        self.assertEqual([r["id"] for r in inserted], ["mcp-web-search"])
        self.assertEqual(inserted[0]["config"]["url"],
                         "http://pat-service.airgap-ai-stack.svc.cluster.local:8080/mcp/web-search/")
        self.assertIn("process.env.AGENT_INFERENCE_KEY", inserted[0]["config"]["headers"]["Authorization"])
        for name in ("acp.patch.yml", "web.patch.yml", "AGENTS.md"):
            self.assertTrue(os.path.exists(os.path.join(dsh_home, name)))

    def test_opencode_config_gets_skills_and_mcp(self):
        os.environ["AGENT_RUNTIME"] = "opencode"
        os.environ["WEB_SEARCH_ENABLED"] = "true"
        os.environ["REPOWISE_ENABLED"] = "true"
        self.run_sync()
        with open(os.path.join(self.home, "opencode", "opencode.json")) as fh:
            cfg = json.load(fh)
        self.assertEqual(sorted(cfg["mcp"]), ["repowise", "web-search"])
        entry = cfg["mcp"]["web-search"]
        self.assertEqual((entry["type"], entry["timeout"]), ("remote", 60000))
        self.assertEqual(entry["headers"]["Authorization"], "Bearer {env:AGENT_INFERENCE_KEY}")
        self.assertEqual(cfg["skills"]["paths"], [os.path.join(self.home, "catalog-enabled")])
        self.assertEqual(cfg["enabled_providers"], ["pat"])

    def test_unknown_runtime_fails(self):
        os.environ["AGENT_RUNTIME"] = "codex"
        with self.assertRaises(SystemExit):
            self.run_sync()

    def test_build_rejects_unknown_skill(self):
        with open(os.path.join(self.catalog, "catalog.yaml"), "a") as fh:
            fh.write("  ghost:\n    required: true\n")
        with self.assertRaises(SystemExit):
            agent_sync.build(self.catalog)


if __name__ == "__main__":
    unittest.main()
