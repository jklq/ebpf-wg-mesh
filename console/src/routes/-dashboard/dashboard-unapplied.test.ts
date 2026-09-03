import { describe, expect, it } from "vitest";

import { unappliedChangeActionLabel } from "./dashboard-unapplied";

describe("unappliedChangeActionLabel", () => {
	it.each([
		["SERVICE_UNAPPLIED_CHANGE_ACTION_ADD", "add"],
		["SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE", "update"],
		["SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE", "remove"],
		["SERVICE_UNAPPLIED_CHANGE_ACTION_UNSPECIFIED", "unspecified"],
	] as const)("maps %s to %s", (action, label) => {
		expect(unappliedChangeActionLabel(action)).toBe(label);
	});
});
