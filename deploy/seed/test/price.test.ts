import assert from "node:assert/strict";
import { test } from "node:test";
import { total } from "../src/price.ts";

test("adds up", () => assert.equal(total([100, 250]), 350));
test("applies a discount", () => assert.equal(total([1000], 15), 850));
