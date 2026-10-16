// Runs logic.test.js under Node's test runner: node internal/web/jstest/run-node.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const require = createRequire(import.meta.url);
Object.assign(globalThis, require("../static/logic.js"), { test, assert });
const file = new URL("./logic.test.js", import.meta.url);
vm.runInThisContext(readFileSync(file, "utf8"), { filename: file.pathname });
