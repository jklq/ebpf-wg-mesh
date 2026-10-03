# Console frontend

The console uses React, TanStack Start and StyleX. Routes load data and enforce access; feature modules own interactions and presentation. Shared UI primitives and design tokens provide the seams for future redesigns.

## Structure

```text
src/
  routes/                 Route definitions, loaders and redirects
  features/
    dashboard/
      dashboard-page.tsx  Page composition
      use-dashboard-controller.ts
      state/              Snapshot reconciliation, live updates and created-service cache
      canvas/             Pan, zoom, layout, service nodes and loading states
      changes/            Apply/discard workflow and release presentation
      navigation/         Project/environment controls and dialogs
      services/           Creation and repository selection
      service-panel/
        deployments/      History, progress, logs and recovery
        settings/         Draft persistence, source/build and runtime forms
        variables/        Environment variables
        domains/          Domain workflow, forms and validation
      shared/             Dashboard domain types and pure helpers
    auth/                 Login presentation
    fleet/                Agent management
    deleted/              Resource recovery
    project-settings/     Project, retention and volume settings
  components/
    ui/                   Buttons, fields, dialogs, tabs, notices and status indicators
    layout/               Shared page shell
    delete-resource-dialog.tsx
  hooks/                  Reusable polling, queued persistence and animation timing
  styles/
    tokens.stylex.ts      Colors, fonts, spacing, shape, motion and layout dimensions
    reset.css             Browser baseline, focus visibility and reduced motion
    dev-styles.tsx        Development CSS delivery and hot updates
  lib/                    Shared data contracts, transport, pure utilities and server code
tooling/
  stylex.ts               Shared compiler setup for client, SSR and unit tests
  check-architecture.ts   Dependency and style-authoring checks
```

Keep a change with the behavior it affects. Feature modules may use shared modules; shared modules cannot import features, and implementation modules cannot import routes. UI primitives have no knowledge of application data. Server function adapters used across features live in `lib/dashboard/server-functions.ts`; fleet-specific adapters stay with fleet. Tests live beside the behavior they verify.

## Dashboard composition

`DashboardPage` arranges navigation, canvas, panel and dialogs. Its controller exposes grouped `canvas`, `navigation`, `selection`, `creation` and `release` interfaces. State synchronization belongs to `useDashboardState`, canvas interaction to `useDashboardCanvas`, and apply/discard coordination to `useEnvironmentRelease`.

Forms keep persistence in their owning workflow. Settings presentation shares a small draft editor interface; domain presentation consumes a grouped domain controller. Deployment rendering separates history, progress, actions and failure recovery. Panels load through React lazy imports and Suspense. Extract a module around an independently understandable responsibility, rather than splitting JSX by line count or adding a generic abstraction for a single caller.

## Styling and redesigns

Follow [Thinking in StyleX](https://stylexjs.com/docs/learn/thinking-in-stylex/): colocate styles with markup, use explicit composition, and name styles for their purpose or state. Use names such as `toolbar`, `secretRow`, `selected` and `promptError`.

Shared values are typed `stylex.defineVars` exports in `tokens.stylex.ts`. Start a redesign there for global color, typography, spacing, shape and motion changes. Change a shared primitive for control-wide changes; change a feature's colocated styles for layout or feature-specific treatment. A new theme can use `stylex.createTheme` against the same variable groups and be applied to an ancestor with `stylex.props`.

```tsx
import * as stylex from "@stylexjs/stylex";
import { Button } from "#/components/ui/button";
import { colors, space } from "#/styles/tokens.stylex";

export function SaveAction({ saving }: { saving: boolean }) {
  return (
    <Button variant="primary" disabled={saving} styles={styles.action}>
      {saving ? "Saving…" : "Save"}
    </Button>
  );
}

const styles = stylex.create({
  action: { marginTop: space.lg, borderColor: colors.accent },
});
```

Primitives accept `styles?: stylex.StyleXStyles` and compose caller styles last. Reuse exported recipes such as `buttonStyles` when a native link needs the same appearance. Use small state modifiers instead of copying a full base style. Use ordinary boolean and ternary expressions in `stylex.props` to select variants.

Keep hover, focus, pseudo-element and media conditions in the relevant style declaration. Style children explicitly; layout uses flex/grid and `gap`. Dynamic coordinates, dimensions and animation delays use StyleX style functions. Component code does not author raw `className` strings, inline `style` objects or global descendant selectors. Global CSS is limited to `reset.css`; its accessibility overrides intentionally take precedence over component animation and display styles.

## Build and verification

Install dependencies with `bun install --frozen-lockfile`. Node 22.12+ runs Vite and its StyleX compiler; Bun remains the package manager and production server runtime. The container includes Node in its build stage. Running Vite under Bun currently produces media-query parser failures with this compiler combination.

```sh
bun run dev
bun run check           # Formatting, lint and architecture/style checks
bun run typecheck
bun run test:unit
bun run build
bun run start
```

StyleX extracts production CSS at build time. Development serves compiled CSS and updates it through the Vite virtual module. The same compiler settings and module resolution are used for client, SSR and tests so variable identities match. Unit tests disable only the CSS server hook, which avoids a development polling interval in Vitest.

CI checks formatting, architecture, types, unit behavior and the production build. The production build exercises StyleX extraction across routes and lazy panels.

Test user behavior through accessible roles and observable state; do not assert generated class names. Existing dashboard tests cover selection, reconciliation, deployment coordination, draft writes, domains and logs. Shared dialog and tab tests cover focus, nested dismissal, scroll locking and keyboard selection. Database integration and local stack end-to-end tests remain separate commands in `package.json`.
