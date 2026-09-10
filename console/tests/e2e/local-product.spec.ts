import { readFile } from "node:fs/promises";
import { expect, test } from "@playwright/test";

interface StackSummary {
	product_e2e?: {
		service_name: string;
		route_url: string;
		marker: string;
		session_cookie_name?: string;
		session_cookie_value?: string;
	};
	public_base_url?: string;
}

test("login, deploy, redeploy, and generated domain route work through the local product stack", async ({
	page,
	context,
	request,
	baseURL,
}) => {
	const stack = JSON.parse(
		await readFile("artifacts/e2e-local/stack.json", "utf8"),
	) as StackSummary;
	if (!stack.product_e2e) {
		throw new Error(
			"local product fixture is missing; run through `make test-e2e-local`",
		);
	}
	if (!stack.product_e2e.route_url.startsWith("https://")) {
		throw new Error(
			`product e2e route must use the public tunnel (https), got ${stack.product_e2e.route_url}`,
		);
	}
	if (
		!stack.product_e2e.session_cookie_name ||
		!stack.product_e2e.session_cookie_value
	) {
		throw new Error(
			"product e2e session cookie is missing from stack.json; public tunnel mode cannot use open dev logins",
		);
	}

	const cookieURL = baseURL ?? "http://platform.localtest.me:8080";
	await context.addCookies([
		{
			name: stack.product_e2e.session_cookie_name,
			value: stack.product_e2e.session_cookie_value,
			url: cookieURL,
			httpOnly: true,
			sameSite: "Lax",
		},
	]);

	await page.goto("/");
	await expect(page.getByText(stack.product_e2e.service_name)).toBeVisible({
		timeout: 30_000,
	});
	await expect(page.getByRole("link", { name: "Sign out" })).toBeVisible();
	await page.reload();
	await expect(page.getByText(stack.product_e2e.service_name)).toBeVisible({
		timeout: 30_000,
	});

	const response = await request.get(stack.product_e2e.route_url);
	expect(response.status()).toBe(200);
	expect(await response.text()).toContain(stack.product_e2e.marker);

	// Confirm the public login surface does not expose open dev credentials.
	await context.clearCookies();
	await page.goto("/login");
	await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
	await expect(
		page.getByRole("link", { name: /dev@example\.com/i }),
	).toHaveCount(0);
	await expect(
		page.getByRole("link", { name: /Continue with GitHub/i }),
	).toBeVisible();
});
