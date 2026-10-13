import {
	createMemoryHistory,
	createRootRoute,
	createRouter,
	RouterContextProvider,
} from "@tanstack/react-router";
import { type RenderOptions, render } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";

/** Supply real link routing while a component test stubs its data mutations. */
export function renderWithRouter(ui: ReactElement, options?: RenderOptions) {
	const router = createRouter({
		routeTree: createRootRoute(),
		history: createMemoryHistory(),
	});
	const Wrapper = options?.wrapper;
	return render(ui, {
		...options,
		wrapper: ({ children }: { children: ReactNode }) => (
			<RouterContextProvider router={router}>
				{Wrapper ? <Wrapper>{children}</Wrapper> : children}
			</RouterContextProvider>
		),
	});
}
