import { describe, expect, it } from "vitest";

import {
	queuedChangeCount,
	snapshotApplyingChanges,
	unappliedChangeActionLabel,
} from "#/features/dashboard/changes/dashboard-unapplied";
import {
	serviceRecord,
	unappliedChange,
} from "#/features/dashboard/dashboard-page.test-helpers";

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

it("keeps a newer edit to the same field ready while the old value is applying", () => {
	const before = serviceRecord({
		pendingChanges: true,
		unappliedChangeCount: 1,
		unappliedChanges: [
			unappliedChange("runtime.env.FOO", "Variables", "FOO", "old", "new"),
		],
	});
	const applying = new Set(snapshotApplyingChanges(before).changeKeys);
	const edited = {
		...before,
		unappliedChanges: [
			unappliedChange("runtime.env.FOO", "Variables", "FOO", "old", "newer"),
		],
	};
	expect(queuedChangeCount(before, applying)).toBe(0);
	expect(queuedChangeCount(edited, applying)).toBe(1);
});
