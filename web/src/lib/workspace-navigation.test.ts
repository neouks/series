import assert from "node:assert/strict";
import test from "node:test";
import { NEW_TASK_PATH, SETTINGS_TABS, settingsTab, settingsURL, workspaceShortcut } from "./workspace-navigation";

test("settings preserve task, session, source location and filters", () => {
  const search = "?id=132&tab=sessions&session=worker%3A47&activity=800&sort=oldest&q=%E6%BC%8F%E6%B4%9E";
  for (const tab of SETTINGS_TABS) {
    const opened = settingsURL("/function/tasks/detail", search, tab);
    const params = new URL(opened, "http://localhost").searchParams;
    assert.equal(params.get("session"), "worker:47");
    assert.equal(params.get("activity"), "800");
    assert.equal(params.get("q"), "漏洞");
    assert.equal(params.get("settingsTab"), tab);
    assert.equal(settingsURL("/function/tasks/detail", params.toString(), null), `/function/tasks/detail${search}`);
  }
  assert.equal(settingsURL(NEW_TASK_PATH, "", null), NEW_TASK_PATH);
  assert.equal(settingsTab("unknown"), "general");
});

test("shortcuts respect IME, editors, overlays and conversation search", () => {
  const event = {
    key: "j",
    metaKey: true,
    ctrlKey: false,
    altKey: false,
    shiftKey: false,
    isComposing: false,
    defaultPrevented: false,
  };
  assert.equal(workspaceShortcut(event, false), "new-task");
  assert.equal(workspaceShortcut({ ...event, key: "J", metaKey: false, ctrlKey: true }, false), "new-task");
  assert.equal(workspaceShortcut({ ...event, key: "k" }, false), null);
  assert.equal(workspaceShortcut({ ...event, key: "K", metaKey: false, ctrlKey: true }, false), null);
  assert.equal(workspaceShortcut({ ...event, key: "f" }, false), null);
  assert.equal(workspaceShortcut(event, true), null);
  for (const flag of ["isComposing", "defaultPrevented", "altKey", "shiftKey"])
    assert.equal(workspaceShortcut({ ...event, [flag]: true }, false), null);
  assert.equal(workspaceShortcut({ ...event, metaKey: false }, false), null);
});
