# platform - package notes

Cross-cutting operational concerns. **Two altitudes coexist here by design**, and
`make layercheck` permits both because `platform` is operational tooling, not an
inner layer:

- **Zero-dep leaves**, imported widely: `config`, `tokenize`, `logrotate`,
  `crashloop`, `observability`, `service`, `embedderprobe`, `health`.
- **`doctor`** - a high-altitude diagnostic consumer that imports `application` +
  `infrastructure` to assemble health bundles.

`doctor` stays here (rather than `internal/diagnostics`) because relocating it
churns every importer for a naming nicety with no dependency benefit.
