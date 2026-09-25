import { expect, it } from "vitest";
import { sealedSecretName, sealedSecretValue } from "./sealed-secrets";

it("uses the backend name and UTF-8 byte limits", () => {
	for (const name of ["", "1TOKEN", "PLATFORM_TOKEN", "a".repeat(129)])
		expect(sealedSecretName.safeParse(name).success).toBe(false);
	expect(sealedSecretName.safeParse("_TOKEN1").success).toBe(true);
	expect(sealedSecretValue.safeParse("é".repeat(32768)).success).toBe(true);
	expect(sealedSecretValue.safeParse("é".repeat(32769)).success).toBe(false);
});
