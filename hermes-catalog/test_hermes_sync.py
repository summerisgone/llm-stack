"""Unit tests for hermes_sync. Run: python3 -m unittest hermes-catalog/test_hermes_sync.py"""

import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import hermes_sync  # noqa: E402

PROFILE = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                       "..", "config", "hermes", "base-profile")


class SyncTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.catalog = os.path.join(self.tmp, "catalog-src")
        shutil.copytree(PROFILE, self.catalog)
        hermes_sync.build(self.catalog)
        self.home = os.path.join(self.tmp, "home")
        self.report = os.path.join(self.tmp, "report")
        hermes_sync.CATALOG_DIR = self.catalog
        hermes_sync.HERMES_HOME = self.home
        hermes_sync.REPORT_PATH = self.report
        os.environ.pop("HERMES_SELECTION", None)
        os.environ.pop("WEB_SEARCH_ENABLED", None)

    def tearDown(self):
        shutil.rmtree(self.tmp)

    def run_sync(self, selection=None):
        if selection is not None:
            os.environ["HERMES_SELECTION"] = json.dumps(selection)
        else:
            os.environ.pop("HERMES_SELECTION", None)
        hermes_sync.sync()
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
        cfg = hermes_sync.load_yaml(os.path.join(self.home, "config.yaml"), {})
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
        cfg = hermes_sync.load_yaml(os.path.join(self.home, "config.yaml"), {})
        self.assertNotIn("web-search", cfg["mcp_servers"])
        self.assertIn("mine", cfg["mcp_servers"])
        self.assertIn("web", cfg["agent"]["disabled_toolsets"])

        os.environ["WEB_SEARCH_ENABLED"] = "true"
        self.run_sync()
        cfg = hermes_sync.load_yaml(os.path.join(self.home, "config.yaml"), {})
        entry = cfg["mcp_servers"]["web-search"]
        self.assertIn("pat-service", entry["url"])
        self.assertEqual(entry["headers"]["Authorization"], "Bearer ${HERMES_INFERENCE_KEY}")

    def test_new_optional_skill_announced_once(self):
        self.run_sync()
        os.makedirs(os.path.join(self.catalog, "skills", "fresh"))
        with open(os.path.join(self.catalog, "skills", "fresh", "SKILL.md"), "w") as fh:
            fh.write("---\nname: fresh\ndescription: new one\n---\n")
        hermes_sync.build(self.catalog)
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
        hermes_sync.build(self.catalog)
        self.run_sync()
        with open(os.path.join(self.home, "memories", "MEMORY.md")) as fh:
            self.assertEqual(fh.read(), "remember me")
        with open(os.path.join(self.home, "SOUL.md")) as fh:
            self.assertTrue(fh.read().rstrip().endswith("Answer in Russian."))
        self.assertFalse(os.path.exists(
            os.path.join(self.home, "catalog", "skills", "change-summary")))

    def test_build_rejects_unknown_skill(self):
        with open(os.path.join(self.catalog, "catalog.yaml"), "a") as fh:
            fh.write("  ghost:\n    required: true\n")
        with self.assertRaises(SystemExit):
            hermes_sync.build(self.catalog)


if __name__ == "__main__":
    unittest.main()
