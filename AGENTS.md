Use Conventional Commits for all commit messages.

## Deployment status

The platform is not deployed anywhere yet. While this remains true, do not add
platform data migrations, backward-compatibility layers, or compatibility tests
solely to preserve hypothetical existing installations. Prefer direct changes to
schemas, configuration, protocols, and storage formats when they simplify the
implementation.

Application database migration jobs are a product capability and are not excluded
by this instruction. Revisit this constraint before the first real deployment.
