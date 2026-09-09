# Contracts

`openapi.yaml` is the **source of truth** for the API (ADR-0005). Never hand-edit generated code.

- Go server types: `oapi-codegen` → `apps/api/internal/apigen`
- TS client: `openapi-typescript` → `apps/web/src/api/generated`
- CI lints this file on every PR; a failing lint blocks merge.
