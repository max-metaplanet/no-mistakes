# Fork divergences

## 2026-11-17: permanent default skips

This fork supports `default_skip_steps` in the operator's global
`~/.no-mistakes/config.yaml`. For example, `default_skip_steps: [document]`
keeps the Document step off for every newly created run, including reruns.
Per-run `--skip` values add to these defaults; they cannot remove a default.
An absent or empty list preserves upstream behavior. Unknown or malformed
entries reject the configuration instead of dropping any entries.

This deliberately diverges from upstream's policy that permanent skips are
unsupported. The Document step repeatedly added unwanted comments to the
connect project's `.github/workflows/ci.yml`, including twice after explicit
instructions not to add them. The operator needs a persistent opt-out without
remembering a flag on every run.

The merge belongs to `RunManager.startRunWithIntentSourceLocked`, where all
new-run and rerun paths configure the executor. The executor's existing set
deduplicates the combined list. Repository configuration cannot set this key,
and custom gates remain ineligible for skipping. Existing runs keep their
recorded skip decisions; changing the global default affects future runs.

When importing upstream changes to skip handling, reconcile the global YAML
schema and validation, the shared daemon launch path, executor skip persistence
and recovery, and CLI/push-option validation. Keep new runs and reruns on the
same union rule and retain the focused tests. Upstream attestation requirements
may still require completed steps, including Document; this option does not
change those requirements or claim that skipped work was validated.

This change was delivered directly to `fm-build` for local compilation, without
running the full no-mistakes pipeline. Installation, live configuration changes,
and any daemon lifecycle action remain the fleet owner's responsibility.
